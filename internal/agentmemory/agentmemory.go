/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package agentmemory is a minimal, dependency-free persistence layer for agent
// conversation history. It stands in for a real memory subsystem the same way
// internal/agentloop stands in for a real agent framework: it exists so the
// Memory CRD can be exercised end to end.
//
// A Store persists an ordered list of conversation Turns per session key. The
// only backend implemented is Redis (spoken over its RESP wire protocol with
// the standard library, so no third-party client is pulled in). Other backends
// declared by the Memory CRD (postgres/vector/graph/s3) return ErrUnsupported.
package agentmemory

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ErrUnsupported is returned by Open for a backend that has no implementation.
var ErrUnsupported = errors.New("agentmemory: unsupported backend")

// Turn is one message in a conversation. Role is "user" or "assistant".
type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Store persists and retrieves conversation history per session key.
type Store interface {
	// Load returns the stored turns for a session in chronological order.
	Load(ctx context.Context, sessionID string) ([]Turn, error)
	// Append persists turns to the end of a session's history.
	Append(ctx context.Context, sessionID string, turns ...Turn) error
}

// maxTurns caps how many turns are retained per session (older ones are
// trimmed). Keeps memory bounded without a real eviction policy.
const maxTurns = 40

// Open builds a Store for the given backend using dsn as the connection string
// and keyNamespace as a prefix that scopes/isolates entries within the backend.
// Only "redis" is implemented today.
func Open(backend, dsn, keyNamespace string) (Store, error) {
	switch strings.ToLower(backend) {
	case "redis":
		return newRedisStore(dsn, keyNamespace)
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnsupported, backend)
	}
}

// --- Redis backend (RESP over TCP, standard library only) -------------------

type redisStore struct {
	addr      string
	password  string
	db        string
	keyPrefix string
}

func newRedisStore(dsn, keyNamespace string) (*redisStore, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse redis dsn: %w", err)
	}
	if u.Scheme != "redis" && u.Scheme != "rediss" {
		return nil, fmt.Errorf("agentmemory: dsn scheme %q is not redis", u.Scheme)
	}
	addr := u.Host
	if !strings.Contains(addr, ":") {
		addr += ":6379"
	}
	pass, _ := u.User.Password()
	db := strings.Trim(u.Path, "/")
	prefix := keyNamespace
	if prefix == "" {
		prefix = "agentplane"
	}
	return &redisStore{addr: addr, password: pass, db: db, keyPrefix: prefix}, nil
}

func (r *redisStore) key(sessionID string) string {
	return r.keyPrefix + ":" + sessionID
}

func (r *redisStore) Load(ctx context.Context, sessionID string) ([]Turn, error) {
	c, err := r.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = c.close() }()

	items, err := c.lrange(r.key(sessionID), 0, -1)
	if err != nil {
		return nil, err
	}
	turns := make([]Turn, 0, len(items))
	for _, it := range items {
		var t Turn
		if err := json.Unmarshal([]byte(it), &t); err == nil && t.Role != "" {
			turns = append(turns, t)
		}
	}
	return turns, nil
}

func (r *redisStore) Append(ctx context.Context, sessionID string, turns ...Turn) error {
	if len(turns) == 0 {
		return nil
	}
	c, err := r.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = c.close() }()

	key := r.key(sessionID)
	for _, t := range turns {
		b, _ := json.Marshal(t)
		if err := c.rpush(key, string(b)); err != nil {
			return err
		}
	}
	// Keep only the most recent maxTurns entries.
	return c.ltrim(key, -maxTurns, -1)
}

func (r *redisStore) dial(ctx context.Context) (*respConn, error) {
	d := net.Dialer{Timeout: 5 * time.Second}
	nc, err := d.DialContext(ctx, "tcp", r.addr)
	if err != nil {
		return nil, fmt.Errorf("dial redis %s: %w", r.addr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = nc.SetDeadline(dl)
	} else {
		_ = nc.SetDeadline(time.Now().Add(10 * time.Second))
	}
	c := &respConn{conn: nc, r: bufio.NewReader(nc)}
	if r.password != "" {
		if _, err := c.do("AUTH", r.password); err != nil {
			_ = c.close()
			return nil, err
		}
	}
	if r.db != "" && r.db != "0" {
		if _, err := c.do("SELECT", r.db); err != nil {
			_ = c.close()
			return nil, err
		}
	}
	return c, nil
}

// --- tiny RESP client -------------------------------------------------------

type respConn struct {
	conn net.Conn
	r    *bufio.Reader
}

func (c *respConn) close() error { return c.conn.Close() }

func (c *respConn) rpush(key, val string) error {
	_, err := c.do("RPUSH", key, val)
	return err
}

func (c *respConn) ltrim(key string, start, stop int) error {
	_, err := c.do("LTRIM", key, strconv.Itoa(start), strconv.Itoa(stop))
	return err
}

func (c *respConn) lrange(key string, start, stop int) ([]string, error) {
	v, err := c.do("LRANGE", key, strconv.Itoa(start), strconv.Itoa(stop))
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]string)
	if !ok {
		return nil, nil
	}
	return arr, nil
}

// do sends a command as a RESP array of bulk strings and reads one reply.
func (c *respConn) do(args ...string) (any, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := c.conn.Write([]byte(b.String())); err != nil {
		return nil, err
	}
	return c.readReply()
}

// readReply parses a single RESP reply (simple string, error, integer, bulk
// string, or array of bulk strings — the subset this store needs).
func (c *respConn) readReply() (any, error) {
	line, err := c.readLine()
	if err != nil {
		return nil, err
	}
	if len(line) == 0 {
		return nil, errors.New("agentmemory: empty redis reply")
	}
	switch line[0] {
	case '+': // simple string
		return line[1:], nil
	case '-': // error
		return nil, fmt.Errorf("redis: %s", line[1:])
	case ':': // integer
		return line[1:], nil
	case '$': // bulk string
		return c.readBulk(line)
	case '*': // array
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad array header %q", line)
		}
		if n < 0 {
			return []string(nil), nil
		}
		out := make([]string, 0, n)
		for range n {
			hdr, err := c.readLine()
			if err != nil {
				return nil, err
			}
			s, err := c.readBulk(hdr)
			if err != nil {
				return nil, err
			}
			if s != nil {
				out = append(out, *s)
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("redis: unexpected reply type %q", line[0])
	}
}

// readBulk reads a bulk-string body given its "$<len>" header line.
func (c *respConn) readBulk(hdr string) (*string, error) {
	if len(hdr) == 0 || hdr[0] != '$' {
		return nil, fmt.Errorf("redis: expected bulk header, got %q", hdr)
	}
	n, err := strconv.Atoi(hdr[1:])
	if err != nil {
		return nil, fmt.Errorf("redis: bad bulk length %q", hdr)
	}
	if n < 0 {
		return nil, nil // nil bulk
	}
	buf := make([]byte, n+2) // include trailing CRLF
	if _, err := readFull(c.r, buf); err != nil {
		return nil, err
	}
	s := string(buf[:n])
	return &s, nil
}

func (c *respConn) readLine() (string, error) {
	line, err := c.r.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func readFull(r *bufio.Reader, buf []byte) (int, error) {
	total := 0
	for total < len(buf) {
		n, err := r.Read(buf[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}
