// The web UI's markdown renderer lives inline in web.html (and is duplicated
// in cmd/agent-runtime/main.go's Go-string UI — see the sync note in
// web.html's header). This test extracts just the pure-JS markdown functions
// (md/inline/esc/splitCells/isTableRule/artifactCard) from the <script> block
// and runs them in a vm sandbox with a minimal `document` stub, so the
// rendering logic is covered without a full browser/DOM dependency.
import test from 'node:test';
import assert from 'node:assert/strict';
import vm from 'node:vm';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import path from 'node:path';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const html = readFileSync(path.join(__dirname, '../src/web.html'), 'utf8');
// Slice out just the pure-JS markdown functions (esc/BT/inline/splitCells/
// isTableRule/md), from `function esc` up to (not including) `function
// artifactCard` — everything before is markup, everything from artifactCard
// onward touches the live DOM (#log, #modal, event listeners) and isn't
// exercised here.
const lines = html.split('\n');
const start = lines.findIndex((l) => l.startsWith('function esc'));
const end = lines.findIndex((l) => l.startsWith('function artifactCard'));
const script = lines.slice(start, end).join('\n');

function domStub() {
  function escapeHTML(s) {
    return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
  }
  return {
    createElement: () => {
      let text = '';
      return {
        set textContent(v) { text = v; },
        get innerHTML() { return escapeHTML(text); },
      };
    },
  };
}

function loadMarkdownFns() {
  const sandbox = { document: domStub(), console };
  vm.createContext(sandbox);
  vm.runInContext(script + '\nthis.__md=md; this.__inline=inline;', sandbox);
  return { md: sandbox.__md, inline: sandbox.__inline };
}

test('bold, italic, inline code, and links render inside a paragraph', () => {
  const { inline } = loadMarkdownFns();
  const out = inline('**bold** and *italic* and a link [x](https://example.com)');
  assert.match(out, /<strong>bold<\/strong>/);
  assert.match(out, /<em>italic<\/em>/);
  assert.match(out, /<a href="https:\/\/example\.com"[^>]*>x<\/a>/);
});

test('a GFM pipe table renders as <table><thead>/<tbody>', () => {
  const { md } = loadMarkdownFns();
  const out = md('| 类别 | 估算 |\n|---|---|\n| 机票 | ¥5,500 |\n| 租车 | ¥4,200 |');
  assert.match(out, /<table>/);
  assert.match(out, /<th>类别<\/th>/);
  assert.match(out, /<th>估算<\/th>/);
  assert.match(out, /<td>机票<\/td>/);
  assert.match(out, /<td>¥5,500<\/td>/);
  // Two data rows, one header row.
  assert.equal((out.match(/<tr>/g) || []).length, 3);
});

test('a table with alignment markers sets text-align per column', () => {
  const { md } = loadMarkdownFns();
  const out = md('| a | b |\n|:--|--:|\n| x | y |');
  assert.match(out, /<th style="text-align:left">a<\/th>/);
  assert.match(out, /<th style="text-align:right">b<\/th>/);
});

test('plain text with a pipe but no separator row is not mistaken for a table', () => {
  const { md } = loadMarkdownFns();
  const out = md('cost: 10 | 20 per unit');
  assert.doesNotMatch(out, /<table>/);
  assert.match(out, /<p>/);
});

test('headings and lists still work alongside the new table support', () => {
  const { md } = loadMarkdownFns();
  const out = md('# Title\n\n- one\n- two');
  assert.match(out, /<h1>Title<\/h1>/);
  assert.match(out, /<ul><li>one<\/li><li>two<\/li><\/ul>/);
});
