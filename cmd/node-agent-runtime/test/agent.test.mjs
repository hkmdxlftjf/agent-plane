import test from 'node:test';
import assert from 'node:assert/strict';
import http from 'node:http';
import { createOpenAICompatible } from '@ai-sdk/openai-compatible';
import { Session } from '../src/agent.js';

// A fake OpenAI-compatible endpoint that always returns an empty
// content/tool_calls message with finish_reason "length" — the exact shape
// that made the Go SDK (agentloop.Session.Send) silently return "" instead of
// an error (see cmd/agent-runtime/thinking.go's problem writeup: a reasoning
// model can leave content empty while still "answering").
function fakeChatServer(respond) {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      let body = '';
      req.on('data', (d) => { body += d; });
      req.on('end', () => {
        res.setHeader('Content-Type', 'application/json');
        res.end(JSON.stringify(respond(JSON.parse(body))));
      });
    });
    server.listen(0, () => resolve(server));
  });
}

function chunkResponse(message, finishReason) {
  return {
    id: 'x', object: 'chat.completion', created: 0, model: 'm',
    choices: [{ index: 0, message, finish_reason: finishReason }],
  };
}

test('an empty content + no tool_calls + non-stop finishReason throws, not ""', async () => {
  const server = await fakeChatServer(() =>
    chunkResponse({ role: 'assistant', content: null }, 'length'));
  const provider = createOpenAICompatible({ name: 'test', apiKey: 'x', baseURL: `http://127.0.0.1:${server.address().port}` });
  const session = new Session(provider('m'), { tools: [], skills: [] }, { log: () => {} });
  await assert.rejects(() => session.send('hi'), /no final answer/);
  server.close();
});

test('a normal stop with text content returns that text', async () => {
  const server = await fakeChatServer(() =>
    chunkResponse({ role: 'assistant', content: 'hello there' }, 'stop'));
  const provider = createOpenAICompatible({ name: 'test', apiKey: 'x', baseURL: `http://127.0.0.1:${server.address().port}` });
  const session = new Session(provider('m'), { tools: [], skills: [] }, { log: () => {} });
  const answer = await session.send('hi');
  assert.equal(answer, 'hello there');
  server.close();
});
