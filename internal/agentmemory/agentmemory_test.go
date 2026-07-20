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

package agentmemory

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
)

// fakeRedis is a minimal in-process RESP server backing lists in a map. It
// supports the exact command subset redisStore uses: AUTH, SELECT, RPUSH,
// LTRIM, LRANGE. It lets us exercise the hand-rolled RESP client end to end
// without a real Redis.
type fakeRedis struct {
	ln    net.Listener
	lists map[string][]string
}

func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeRedis{ln: ln, lists: map[string][]string{}}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeRedis) addr() string { return f.ln.Addr().String() }

func (f *fakeRedis) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeRedis) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	r := bufio.NewReader(conn)
	for {
		args, err := readCommand(r)
		if err != nil {
			return
		}
		if len(args) == 0 {
			continue
		}
		switch strings.ToUpper(args[0]) {
		case "AUTH", "SELECT":
			fmt.Fprint(conn, "+OK\r\n")
		case "RPUSH":
			key := args[1]
			f.lists[key] = append(f.lists[key], args[2:]...)
			fmt.Fprintf(conn, ":%d\r\n", len(f.lists[key]))
		case "LTRIM":
			fmt.Fprint(conn, "+OK\r\n")
		case "LRANGE":
			key := args[1]
			items := f.lists[key]
			fmt.Fprintf(conn, "*%d\r\n", len(items))
			for _, it := range items {
				fmt.Fprintf(conn, "$%d\r\n%s\r\n", len(it), it)
			}
		default:
			fmt.Fprint(conn, "-ERR unknown command\r\n")
		}
	}
}

// readCommand parses a RESP array-of-bulk-strings request (what clients send).
func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if len(line) == 0 || line[0] != '*' {
		return nil, fmt.Errorf("bad request header %q", line)
	}
	n, err := strconv.Atoi(line[1:])
	if err != nil {
		return nil, err
	}
	args := make([]string, 0, n)
	for range n {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		length, err := strconv.Atoi(hdr[1:])
		if err != nil {
			return nil, err
		}
		buf := make([]byte, length+2)
		if _, err := readFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:length]))
	}
	return args, nil
}

func TestRedisStoreRoundTrip(t *testing.T) {
	f := startFakeRedis(t)
	store, err := Open("redis", "redis://"+f.addr()+"/0", "test-ns")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx := context.Background()

	// Empty history to start.
	got, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load empty: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty history, got %d", len(got))
	}

	// Append two turns, including one with tricky characters (CRLF, quotes).
	want := []Turn{
		{Role: "user", Content: "hello\r\nworld \"quoted\""},
		{Role: "assistant", Content: "hi there"},
	}
	if err := store.Append(ctx, "s1", want...); err != nil {
		t.Fatalf("Append: %v", err)
	}

	got, err = store.Load(ctx, "s1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d turns, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("turn %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A different session key is isolated.
	other, err := store.Load(ctx, "s2")
	if err != nil {
		t.Fatalf("Load s2: %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("expected s2 empty, got %d", len(other))
	}
}

func TestOpenUnsupportedBackend(t *testing.T) {
	if _, err := Open("postgres", "postgres://x", ""); err == nil {
		t.Fatal("expected error for unsupported backend")
	}
}
