// tools.js — dispatches a resolved AgentConfig tool call over http or mcp,
// and adapts each into an `ai` SDK tool() so the model can call it directly.
// This intentionally does NOT use an MCP client library: our servers
// (cmd/example-mcp, cmd/script-mcp) implement only initialize/tools/list/
// tools/call over Streamable-HTTP, and a hand-rolled client keeps the surface
// exactly as large as what we actually speak.
'use strict';

import { tool, jsonSchema } from 'ai';

let rpcID = 0;

async function callMCP(endpoint, mcpToolName, args) {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Accept: 'application/json, text/event-stream' },
    body: JSON.stringify({ jsonrpc: '2.0', id: ++rpcID, method: 'tools/call', params: { name: mcpToolName, arguments: args } }),
  });
  const ct = res.headers.get('content-type') || '';
  let payload;
  if (ct.includes('text/event-stream')) {
    const text = await res.text();
    const line = text.split('\n').find((l) => l.startsWith('data: '));
    payload = JSON.parse((line || 'data: {}').slice('data: '.length));
  } else {
    payload = await res.json();
  }
  if (payload.error) throw new Error(`mcp error ${payload.error.code}: ${payload.error.message}`);
  const result = payload.result;
  const text = (result?.content || []).map((c) => c.text).filter(Boolean).join('\n');
  if (result?.isError) throw new Error(text || 'tool reported an error');
  return text;
}

async function callHTTP(endpoint, args) {
  const res = await fetch(endpoint, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(args),
  });
  const text = await res.text();
  if (!res.ok) throw new Error(`http tool ${res.status}: ${text}`);
  return text;
}

// buildTools turns AgentConfig.tools[] into an `ai` SDK tool set, keyed by
// name. Each call is logged (name + truncated args/result) the same way
// cmd/agent-runtime does, so operators diffing behavior between the two
// runtimes see comparable output.
export function buildTools(tools, log) {
  const out = {};
  for (const t of tools || []) {
    out[t.name] = tool({
      description: t.description || t.name,
      inputSchema: jsonSchema(t.inputSchema || { type: 'object' }),
      execute: async (args) => {
        log(`[tool] ${t.name}(${JSON.stringify(args)})`);
        try {
          const result = t.type === 'mcp'
            ? await callMCP(t.endpoint, t.mcpToolName, args)
            : await callHTTP(t.endpoint, args);
          log(`  \u21b3 ${truncate(result, 200)}`);
          return result;
        } catch (e) {
          log(`  \u21b3 tool error: ${e.message}`);
          return `tool error: ${e.message}`;
        }
      },
    });
  }
  return out;
}

function truncate(s, n) {
  s = String(s);
  return s.length > n ? s.slice(0, n) + '\u2026' : s;
}
