// main.js — entrypoint for the Node/Vercel-AI-SDK Agent Plane runtime.
// Serve mode only for now (the scenario this runtime exists for: a browser
// chat UI backed by a model whose reasoning/tool-calling behavior the Go SDK
// mishandles — see cmd/agent-runtime/thinking.go for the problem writeup).
'use strict';

import http from 'node:http';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';
import { fetchConfig, watchConfig } from './registry.js';
import { SecretReader } from './secrets.js';
import { Session } from './agent.js';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const webPage = readFileSync(path.join(__dirname, 'web.html'), 'utf8');

function envOr(key, def) {
  const v = process.env[key];
  return v === undefined || v === '' ? def : v;
}

const REGISTRY = envOr('AGENTPLANE_REGISTRY', 'http://localhost:9090');
const NAMESPACE = envOr('AGENTPLANE_AGENT_NAMESPACE', 'default');
const NAME = envOr('AGENTPLANE_AGENT_NAME', 'demo-agent');
const PORT = Number(envOr('PORT', '8080'));
const MAX_STEPS = Number(envOr('AGENTPLANE_MAX_STEPS', '12'));

function log(msg) {
  console.log('  ' + msg);
}

async function main() {
  log(`fetching config for ${NAMESPACE}/${NAME} from ${REGISTRY}`);
  let cfg = await fetchConfig(REGISTRY, NAMESPACE, NAME);
  if (cfg.phase !== 'Ready') {
    console.error(`Agent ${NAMESPACE}/${NAME} is not Ready (phase=${cfg.phase}); a runtime needs a resolved Model`);
    process.exit(1);
  }
  log(`model=${cfg.model.provider}/${cfg.model.modelName}`);
  for (const t of cfg.tools || []) log(`tool: ${t.name} (type=${t.type}) -> ${t.endpoint}`);
  for (const sk of cfg.skills || []) log(`skill (catalog): ${sk.name}`);

  const secrets = new SecretReader(NAMESPACE);
  const apiKey = await secrets.read(cfg.model.secretName, cfg.model.secretKey);

  let endpoint = cfg.model.endpoint;
  if (!endpoint && cfg.model.provider === 'openrouter') endpoint = 'https://openrouter.ai/api/v1';
  const provider = createOpenAICompatible({ name: cfg.model.provider || 'custom', apiKey, baseURL: endpoint });
  const model = provider(cfg.model.modelName);

  // Keep hot-reloading the resolved config (Model/Tools/Skills can change
  // independently of any one session) — new Sessions pick it up; sessions
  // already in flight finish on what they started with.
  const controller = new AbortController();
  watchConfig(REGISTRY, NAMESPACE, NAME, (next) => { cfg = next; }, { signal: controller.signal });

  const sessions = new Map();
  const getSession = (id) => {
    let s = sessions.get(id);
    if (!s) {
      s = new Session(model, cfg, { maxSteps: MAX_STEPS, log });
      sessions.set(id, s);
    }
    return s;
  };

  const toolNames = (cfg.tools || []).map((t) => t.name);
  const mcpEndpoints = [...new Set((cfg.tools || []).filter((t) => t.type === 'mcp' && t.endpoint).map((t) => t.endpoint))];

  const server = http.createServer((req, res) => {
    void handle(req, res).catch((e) => {
      console.error('request failed:', e);
      if (!res.headersSent) res.writeHead(500);
      res.end(JSON.stringify({ error: e.message }));
    });
  });

  async function handle(req, res) {
    const url = new URL(req.url, 'http://x');
    if (req.method === 'GET' && url.pathname === '/healthz') {
      res.writeHead(200); res.end('ok'); return;
    }
    if (req.method === 'GET' && url.pathname === '/') {
      res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' }); res.end(webPage); return;
    }
    if (req.method === 'GET' && url.pathname === '/api/info') {
      writeJSON(res, { agent: NAME, model: cfg.model?.modelName, tools: toolNames }); return;
    }
    if (req.method === 'GET' && url.pathname.startsWith('/api/artifact/')) {
      await proxyArtifact(url.pathname.slice('/api/artifact/'.length), res); return;
    }
    if (req.method === 'POST' && url.pathname === '/api/chat') {
      const body = await readBody(req);
      let parsed;
      try { parsed = JSON.parse(body); } catch { res.writeHead(400); res.end('invalid JSON'); return; }
      const sessionId = parsed.sessionId || 'default';
      const message = parsed.message;
      if (!message) { res.writeHead(400); res.end('expected {sessionId, message}'); return; }
      try {
        const answer = await getSession(sessionId).send(message);
        writeJSON(res, { answer });
      } catch (e) {
        writeJSON(res, { error: e.message });
      }
      return;
    }
    res.writeHead(404); res.end();
  }

  // Proxies a tool-produced artifact (e.g. script-mcp's render_trip) to
  // whichever MCP endpoint actually served it — mirrors cmd/agent-runtime's
  // /api/artifact proxy, including the X-Agent-Plane-Artifact discriminator
  // (an unrelated vendor MCP endpoint can return 200 for any path with its
  // own error body, which a bare status check would wrongly accept).
  async function proxyArtifact(id, res) {
    if (!id) { res.writeHead(404); res.end(); return; }
    for (const ep of mcpEndpoints) {
      try {
        const r = await fetch(`${ep.replace(/\/$/, '')}/artifacts/${encodeURIComponent(id)}`, { signal: AbortSignal.timeout(10000) });
        if (r.ok && r.headers.get('x-agent-plane-artifact') === '1') {
          const ct = r.headers.get('content-type'); if (ct) res.setHeader('Content-Type', ct);
          const cd = r.headers.get('content-disposition'); if (cd) res.setHeader('Content-Disposition', cd);
          res.writeHead(200);
          for await (const chunk of r.body) res.write(chunk);
          res.end();
          return;
        }
      } catch { /* try the next endpoint */ }
    }
    res.writeHead(404); res.end('artifact not found on any known tool endpoint');
  }

  server.listen(PORT, () => log(`node runtime web UI for "${NAME}" on :${PORT} (model=${cfg.model.modelName} tools=${JSON.stringify(toolNames)})`));

  for (const sig of ['SIGINT', 'SIGTERM']) {
    process.on(sig, () => { controller.abort(); server.close(() => process.exit(0)); });
  }
}

function writeJSON(res, obj) {
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify(obj));
}

function readBody(req) {
  return new Promise((resolve, reject) => {
    let data = '';
    req.on('data', (d) => { data += d; });
    req.on('end', () => resolve(data));
    req.on('error', reject);
  });
}

main().catch((e) => { console.error(e); process.exit(1); });
