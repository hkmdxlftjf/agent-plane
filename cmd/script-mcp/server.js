#!/usr/bin/env node
// travel-tools MCP server — the executable half of the travel-plan-viz skill.
//
// Hosts MCP (Streamable-HTTP JSON-RPC: initialize / tools/list / tools/call,
// the same dialect cmd/example-mcp speaks) plus two plain-HTTP extras:
//
//   GET /healthz             liveness
//   GET /artifacts/:id       a rendered page produced by render_trip
//
// Tools are discovered from the tools/ directory so adding one never touches
// this file:
//   - *.js  must module.exports = { name, description, inputSchema, execute(args, ctx) }
//           ctx carries registerArtifact(html) -> id and log().
//   - *.py  runs as `python3 <file>` with the JSON arguments on stdin; the
//           first line of stdout must be a JSON result object. Its metadata
//           comes from a sibling <name>.json ({name, description, inputSchema}).
//           Scripts are baked into the image at build time — the model can
//           call registered tools with JSON arguments, never upload code.
//
// Zero npm dependencies: node stdlib only.
'use strict';

const http = require('http');
const fs = require('fs');
const path = require('path');
const crypto = require('crypto');

const PORT = process.env.PORT || '8080';
const ARTIFACT_DIR = process.env.ARTIFACT_DIR || '/data/artifacts';
const TOOLS_DIR = path.join(__dirname, 'tools');

// ---------------------------------------------------------------------------
// Tool registry

function loadJsTool(file) {
  const m = require(file);
  if (!m || typeof m.execute !== 'function' || !m.name) {
    throw new Error(`${file}: a JS tool must export { name, description, inputSchema, execute }`);
  }
  return {
    name: m.name,
    description: m.description || '',
    inputSchema: m.inputSchema || { type: 'object' },
    run: (args, ctx) => m.execute(args || {}, ctx),
  };
}

function loadPyTool(file) {
  const metaFile = file.replace(/\.py$/, '.json');
  if (!fs.existsSync(metaFile)) {
    throw new Error(`${file}: missing sibling ${path.basename(metaFile)} (name/description/inputSchema)`);
  }
  const meta = JSON.parse(fs.readFileSync(metaFile, 'utf8'));
  const { spawn } = require('child_process');
  return {
    name: meta.name,
    description: meta.description || '',
    inputSchema: meta.inputSchema || { type: 'object' },
    run: (args, ctx) => new Promise((resolve, reject) => {
      const p = spawn('python3', [file], { stdio: ['pipe', 'pipe', 'inherit'] });
      let out = '';
      p.stdout.on('data', (d) => { out += d; });
      p.on('error', reject);
      p.on('close', (code) => {
        if (code !== 0) { reject(new Error(`python exited ${code}`)); return; }
        try { resolve(JSON.parse(out)); } catch (e) { reject(new Error(`python stdout not JSON: ${e.message}`)); }
      });
      p.stdin.write(JSON.stringify(args || {}));
      p.stdin.end();
      ctx.log(`spawned python3 ${path.basename(file)}`);
    }),
  };
}

function discoverTools(ctx) {
  const tools = new Map();
  for (const f of fs.readdirSync(TOOLS_DIR).sort()) {
    const full = path.join(TOOLS_DIR, f);
    try {
      if (f.endsWith('.js')) {
        const t = loadJsTool(full);
        tools.set(t.name, t);
      } else if (f.endsWith('.py')) {
        const t = loadPyTool(full);
        tools.set(t.name, t);
      }
    } catch (e) {
      ctx.log(`skip ${f}: ${e.message}`);
    }
  }
  return tools;
}

// ---------------------------------------------------------------------------
// Artifact store: rendered pages land on disk and are served back by id. The
// model only ever sees the id — the HTML itself never passes through its
// context, so no token cost and no truncation.

function ensureArtifactDir() {
  fs.mkdirSync(ARTIFACT_DIR, { recursive: true });
}

function saveArtifact(html) {
  return saveArtifactBuffer(Buffer.from(html), 'text/html; charset=utf-8');
}

// registerArtifactFile stores an arbitrary produced file (run_script output)
// and returns its id. Content type is sniffed from the extension; unknown
// extensions are served as octet-stream (download, not inline).
const MIME = {
  '.html': 'text/html; charset=utf-8', '.json': 'application/json',
  '.png': 'image/png', '.jpg': 'image/jpeg', '.jpeg': 'image/jpeg',
  '.gif': 'image/gif', '.svg': 'image/svg+xml', '.csv': 'text/csv',
  '.txt': 'text/plain; charset=utf-8', '.md': 'text/markdown; charset=utf-8',
  '.pdf': 'application/pdf',
};

function saveArtifactBuffer(buf, contentType) {
  ensureArtifactDir();
  const id = crypto.randomBytes(8).toString('hex');
  const meta = { contentType };
  fs.writeFileSync(path.join(ARTIFACT_DIR, id + '.bin'), buf);
  fs.writeFileSync(path.join(ARTIFACT_DIR, id + '.meta'), JSON.stringify(meta));
  return id;
}

function saveArtifactFile(srcPath) {
  const contentType = MIME[path.extname(srcPath).toLowerCase()] || 'application/octet-stream';
  const id = saveArtifactBuffer(fs.readFileSync(srcPath), contentType);
  // Keep the original filename so downloads carry a sensible name.
  fs.writeFileSync(path.join(ARTIFACT_DIR, id + '.name'), path.basename(srcPath));
  return id + ':' + path.basename(srcPath);
}

function artifactPath(id) {
  const base = id.split(':')[0];
  if (!/^[a-f0-9]{4,64}$/.test(base)) return null; // id is the only untrusted input to a path
  const p = path.join(ARTIFACT_DIR, base + '.bin');
  return fs.existsSync(p) ? p : null;
}

// ---------------------------------------------------------------------------
// JSON-RPC plumbing

function send(res, status, body) {
  const buf = Buffer.from(JSON.stringify(body));
  res.writeHead(status, { 'Content-Type': 'application/json', 'Content-Length': buf.length });
  res.end(buf);
}

function rpcResult(res, id, result) { send(res, 200, { jsonrpc: '2.0', id, result }); }
function rpcError(res, id, code, message) { send(res, 200, { jsonrpc: '2.0', id: id ?? null, error: { code, message } }); }

function textContent(text, isError) {
  return { content: [{ type: 'text', text }], isError: !!isError };
}

// ---------------------------------------------------------------------------
// Server

function buildServer() {
  const log = (...a) => console.log(new Date().toISOString(), ...a);
  const ctx = { log, registerArtifact: saveArtifact, registerArtifactFile: saveArtifactFile };
  const tools = discoverTools(ctx);
  log('registered tools:', [...tools.keys()].join(', ') || '(none)');

  return http.createServer((req, res) => {
    if (req.method === 'GET' && req.url === '/healthz') {
      res.writeHead(200); res.end('ok'); return;
    }
    // CORS: the chat UI may live on a different origin when port-forwarding.
    if (req.method === 'OPTIONS') {
      res.writeHead(204, {
        'Access-Control-Allow-Origin': '*',
        'Access-Control-Allow-Methods': 'GET, POST, OPTIONS',
        'Access-Control-Allow-Headers': 'Content-Type',
      });
      res.end(); return;
    }
    if (req.method === 'GET' && req.url.startsWith('/artifacts/')) {
      const id = decodeURIComponent(req.url.slice('/artifacts/'.length).split(/[?#]/)[0]);
      const p = artifactPath(id);
      if (!p) { res.writeHead(404); res.end('no such artifact'); return; }
      const base = id.split(':')[0];
      let contentType = 'application/octet-stream';
      try { contentType = JSON.parse(fs.readFileSync(path.join(ARTIFACT_DIR, base + '.meta'), 'utf8')).contentType; } catch { /* default */ }
      let filename = base;
      try { filename = fs.readFileSync(path.join(ARTIFACT_DIR, base + '.name'), 'utf8'); } catch { /* no original name (e.g. render_trip) */ }
      const headers = { 'Content-Type': contentType, 'Access-Control-Allow-Origin': '*' };
      if (!contentType.startsWith('text/html')) headers['Content-Disposition'] = `inline; filename="${filename.replace(/"/g, '')}"`;
      res.writeHead(200, headers);
      fs.createReadStream(p).pipe(res);
      return;
    }
    if (req.method !== 'POST' || req.url !== '/') {
      res.writeHead(404); res.end(); return;
    }

    let body = '';
    req.on('data', (d) => { body += d; if (body.length > 8 * 1024 * 1024) req.destroy(); });
    req.on('end', () => {
      let rpc;
      try { rpc = JSON.parse(body); } catch { send(res, 200, { jsonrpc: '2.0', id: null, error: { code: -32700, message: 'parse error' } }); return; }
      if (rpc.id === undefined) { res.writeHead(202); res.end(); return; }

      const params = rpc.params || {};
      switch (rpc.method) {
        case 'initialize':
          rpcResult(res, rpc.id, {
            protocolVersion: '2025-06-18',
            capabilities: { tools: {} },
            serverInfo: { name: 'agent-plane-travel-tools', version: '1.0.0' },
          });
          return;
        case 'tools/list': {
          rpcResult(res, rpc.id, {
            tools: [...tools.values()].map((t) => ({
              name: t.name, description: t.description, inputSchema: t.inputSchema,
            })),
          });
          return;
        }
        case 'tools/call': {
          const tool = tools.get(params.name);
          if (!tool) { rpcError(res, rpc.id, -32602, 'unknown tool: ' + params.name); return; }
          Promise.resolve()
            .then(() => tool.run(params.arguments, ctx))
            .then((result) => {
              // A tool returns either an MCP content object (from py) or a plain
              // value (from js) — normalize both.
              if (result && Array.isArray(result.content)) { rpcResult(res, rpc.id, result); return; }
              rpcResult(res, rpc.id, textContent(typeof result === 'string' ? result : JSON.stringify(result, null, 2)));
            })
            .catch((e) => {
              log(`tool ${params.name} failed:`, e.message);
              rpcResult(res, rpc.id, textContent('error: ' + e.message, true));
            });
          return;
        }
        default:
          rpcError(res, rpc.id, -32601, 'method not found: ' + rpc.method);
      }
    });
  });
}

if (require.main === module) {
  buildServer().listen(Number(PORT), () => console.log(new Date().toISOString(), `script-mcp listening on :${PORT}`));
}

module.exports = { buildServer, saveArtifact, artifactPath, discoverTools, textContent };
