'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const http = require('node:http');
const os = require('node:os');
const path = require('node:path');
const fs = require('node:fs');

process.env.ARTIFACT_DIR = fs.mkdtempSync(path.join(os.tmpdir(), 'artifacts-'));
const { buildServer } = require('../server.js');

function withServer(fn) {
  return new Promise((resolve, reject) => {
    const server = buildServer();
    server.listen(0, async () => {
      const port = server.address().port;
      try { await fn(port); resolve(); }
      catch (e) { reject(e); }
      finally { server.close(); }
    });
  });
}

function rpc(port, method, params) {
  return new Promise((resolve, reject) => {
    const body = JSON.stringify({ jsonrpc: '2.0', id: 1, method, params });
    const req = http.request({ port, method: 'POST', path: '/', headers: { 'Content-Type': 'application/json' } }, (res) => {
      let data = '';
      res.on('data', (d) => { data += d; });
      res.on('end', () => { try { resolve(JSON.parse(data)); } catch (e) { reject(e); } });
    });
    req.on('error', reject);
    req.end(body);
  });
}

function get(port, urlPath) {
  return new Promise((resolve, reject) => {
    http.get({ port, path: urlPath }, (res) => {
      let data = '';
      res.on('data', (d) => { data += d; });
      res.on('end', () => resolve({ status: res.statusCode, body: data, headers: res.headers }));
    }).on('error', reject);
  });
}

test('tools/list advertises render_trip, run_script, check_urls', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/list', {});
  const names = res.result.tools.map((t) => t.name).sort();
  assert.deepEqual(names, ['check_urls', 'render_trip', 'run_script']);
}));

test('run_script(python): executes, captures stdout, no artifacts', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', {
    name: 'run_script',
    arguments: { language: 'python', source: 'print(1+1)' },
  });
  const out = JSON.parse(res.result.content[0].text);
  assert.equal(out.exitCode, 0);
  assert.equal(out.stdout.trim(), '2');
  assert.equal(out.artifacts.length, 0);
  assert.equal(res.result.isError, false);
}));

test('run_script(node): writes an artifact file, it is fetchable', () => withServer(async (port) => {
  const src = "require('fs').writeFileSync('out/hello.txt', 'hi there')";
  const res = await rpc(port, 'tools/call', { name: 'run_script', arguments: { language: 'node', source: src } });
  const out = JSON.parse(res.result.content[0].text);
  assert.equal(out.exitCode, 0);
  assert.equal(out.artifacts.length, 1);
  const id = out.artifacts[0];
  const fetched = await get(port, '/artifacts/' + encodeURIComponent(id));
  assert.equal(fetched.status, 200);
  assert.equal(fetched.body, 'hi there');
}));

test('run_script: nonzero exit is reported as isError with stderr', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', {
    name: 'run_script',
    arguments: { language: 'python', source: 'import sys; sys.stderr.write("boom"); sys.exit(3)' },
  });
  const out = JSON.parse(res.result.content[0].text);
  assert.equal(out.exitCode, 3);
  assert.match(out.stderr, /boom/);
  assert.equal(res.result.isError, true);
}));

test('run_script: a timeout kills the process and reports it', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', {
    name: 'run_script',
    arguments: { language: 'python', source: 'import time; time.sleep(30)', timeoutMs: 1000 },
  });
  const out = JSON.parse(res.result.content[0].text);
  assert.notEqual(out.exitCode, 0);
  assert.equal(out.signal, 'SIGKILL');
}));

test('run_script: rejects an unknown language', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', { name: 'run_script', arguments: { language: 'ruby', source: 'x' } });
  assert.equal(res.result.isError, true);
  assert.match(res.result.content[0].text, /language must be one of/);
}));

test('render_trip: invalid trip is rejected before rendering (no artifact)', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', { name: 'render_trip', arguments: { trip: { title: 'x' } } });
  assert.equal(res.result.isError, true);
  assert.match(res.result.content[0].text, /validation failed/);
}));

test('render_trip: a minimal valid trip renders and is fetchable', () => withServer(async (port) => {
  const trip = {
    title: '测试行程', startDate: '2026-09-01', disclaimer: '本页信息仅供参考，出行前请自行核实所有信息。',
    reminders: [], days: [{ date: '2026-09-01', weekday: '周二', slots: [
      { period: 'morning', name: '天安门', time: '09:00', lat: 39.9, lng: 116.4, rating: 4.8, review: '很好', needsBooking: false, leadDays: 0 },
    ], dining: [] }],
  };
  const res = await rpc(port, 'tools/call', { name: 'render_trip', arguments: { trip } });
  assert.equal(res.result.isError, false);
  const out = JSON.parse(res.result.content[0].text);
  assert.ok(out.artifactId);
  const fetched = await get(port, '/artifacts/' + out.artifactId);
  assert.equal(fetched.status, 200);
  assert.match(fetched.body, /天安门/);
  assert.match(fetched.body, /initTravelMap/);
}));

test('check_urls: reports a live and a dead URL', () => withServer(async (port) => {
  const res = await rpc(port, 'tools/call', {
    name: 'check_urls',
    arguments: { urls: ['https://example.com', 'https://example.com/definitely-not-a-real-path-xyz-404'] },
  });
  assert.match(res.result.content[0].text, /dead URLs/);
}));

test('unknown artifact id returns 404', () => withServer(async (port) => {
  const res = await get(port, '/artifacts/deadbeef');
  assert.equal(res.status, 404);
}));
