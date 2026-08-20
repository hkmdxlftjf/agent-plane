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

// sendStream drives streamText (SSE), unlike send's generateText — a
// minimal OpenAI-compatible SSE server, streaming one word at a time.
function fakeSSEServer(chunks) {
  return new Promise((resolve) => {
    const server = http.createServer((req, res) => {
      let body = '';
      req.on('data', (d) => { body += d; });
      req.on('end', () => {
        res.setHeader('Content-Type', 'text/event-stream');
        for (const c of chunks) {
          res.write(`data: ${JSON.stringify(c)}\n\n`);
        }
        res.write('data: [DONE]\n\n');
        res.end();
      });
    });
    server.listen(0, () => resolve(server));
  });
}

function sseChunk(delta, finishReason) {
  return {
    id: 'x', object: 'chat.completion.chunk', created: 0, model: 'm',
    choices: [{ index: 0, delta, finish_reason: finishReason ?? null }],
  };
}

test('sendStream yields text-delta events as they arrive, then a final text event', async () => {
  const server = await fakeSSEServer([
    sseChunk({ role: 'assistant', content: 'Hello' }),
    sseChunk({ content: ' there' }),
    sseChunk({}, 'stop'),
  ]);
  const provider = createOpenAICompatible({ name: 'test', apiKey: 'x', baseURL: `http://127.0.0.1:${server.address().port}` });
  const session = new Session(provider('m'), { tools: [], skills: [] }, { log: () => {} });

  const events = [];
  for await (const ev of session.sendStream('hi')) events.push(ev);

  const deltas = events.filter((e) => e.type === 'text-delta').map((e) => e.delta);
  assert.deepEqual(deltas, ['Hello', ' there']);
  const final = events.find((e) => e.type === 'text');
  assert.equal(final.text, 'Hello there');
  server.close();
});

test('sendStream throws (not a silent empty text event) when steps run out mid-tool-use', async () => {
  // Every step calls a tool and never produces a stop — mimics exhausting
  // maxSteps while the model keeps working.
  const server = await fakeSSEServer([
    sseChunk({ role: 'assistant', tool_calls: [{ index: 0, id: 'c1', type: 'function', function: { name: 'noop', arguments: '{}' } }] }),
    sseChunk({}, 'tool_calls'),
  ]);
  const provider = createOpenAICompatible({ name: 'test', apiKey: 'x', baseURL: `http://127.0.0.1:${server.address().port}` });
  const cfg = {
    tools: [{ name: 'noop', type: 'http', endpoint: 'unused', inputSchema: { type: 'object' } }],
    skills: [],
  };
  const session = new Session(provider('m'), cfg, { maxSteps: 1, log: () => {} });
  // The fake server ignores request content and always returns the same
  // single-step tool call, so with maxSteps=1 the loop stops having only
  // ever seen finishReason "tool-calls" — never "stop".
  session.tools.noop.execute = async () => 'ok';

  await assert.rejects(() => (async () => { for await (const _ of session.sendStream('go')) { /* drain */ } })(), /no final answer/);
  server.close();
});
