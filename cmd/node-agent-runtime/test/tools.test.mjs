import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { buildTools } from '../src/tools.js';

function fakeMCP(handler) {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      let body = '';
      req.on('data', (d) => { body += d; });
      req.on('end', () => {
        const rpc = JSON.parse(body);
        res.setHeader('Content-Type', 'application/json');
        try {
          const result = handler(rpc.params);
          res.end(JSON.stringify({ jsonrpc: '2.0', id: rpc.id, result }));
        } catch (e) {
          // Surface an assertion failure as a normal HTTP response instead of
          // hanging the client's fetch forever; the test still fails, on the
          // assertion, without wedging the whole run.
          res.end(JSON.stringify({ jsonrpc: '2.0', id: rpc.id, error: { code: -32000, message: String(e) } }));
        }
      });
    });
    server.listen(0, () => resolve(server));
  });
}

test('mcp tool call: success text is returned and logged', async () => {
  const server = await fakeMCP((params) => {
    assert.equal(params.name, 'get_weather');
    assert.deepEqual(params.arguments, { city: 'Beijing' });
    return { content: [{ type: 'text', text: 'sunny' }], isError: false };
  });
  const logs = [];
  const tools = buildTools(
    [{ name: 'get_weather', type: 'mcp', mcpToolName: 'get_weather', endpoint: `http://127.0.0.1:${server.address().port}`, inputSchema: { type: 'object' } }],
    (m) => logs.push(m),
  );
  const out = await tools.get_weather.execute({ city: 'Beijing' });
  assert.equal(out, 'sunny');
  assert.ok(logs.some((l) => l.includes('get_weather')));
  server.close();
});

test('mcp tool call: isError becomes a returned error string, not a throw', async () => {
  const server = await fakeMCP(() => ({ content: [{ type: 'text', text: 'boom' }], isError: true }));
  const tools = buildTools(
    [{ name: 'flaky', type: 'mcp', endpoint: `http://127.0.0.1:${server.address().port}`, inputSchema: { type: 'object' } }],
    () => {},
  );
  const out = await tools.flaky.execute({});
  assert.match(out, /tool error: boom/);
  server.close();
});

test('http tool call: posts JSON and returns the raw response body', async () => {
  const server = http.createServer((req, res) => {
    let body = '';
    req.on('data', (d) => { body += d; });
    req.on('end', () => { assert.equal(body, '{"orderId":"A-1"}'); res.end('shipped'); });
  });
  await new Promise((r) => server.listen(0, r));
  const tools = buildTools(
    [{ name: 'order-lookup', type: 'http', endpoint: `http://127.0.0.1:${server.address().port}`, inputSchema: { type: 'object' } }],
    () => {},
  );
  const out = await tools['order-lookup'].execute({ orderId: 'A-1' });
  assert.equal(out, 'shipped');
  server.close();
});
