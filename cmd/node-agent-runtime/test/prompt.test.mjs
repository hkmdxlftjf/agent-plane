import test from 'node:test';
import assert from 'node:assert/strict';
import { buildSystemPrompt } from '../src/prompt.js';

test('no skills: prompt is just the base system text', () => {
  const p = buildSystemPrompt({ prompt: { system: 'BASE' }, skills: [] });
  assert.equal(p, 'BASE');
});

test('falls back to a default when no PromptTemplate is resolved', () => {
  const p = buildSystemPrompt({});
  assert.match(p, /helpful assistant/);
});

test('skills with content are listed as a catalog; empty-content skills are skipped', () => {
  const p = buildSystemPrompt({
    prompt: { system: 'BASE' },
    skills: [
      { name: 'refunds', description: 'process a refund', content: 'STEP1' },
      { name: 'empty', description: 'no body', content: '' },
    ],
  });
  assert.match(p, /Skills available/);
  assert.match(p, /- refunds: process a refund/);
  assert.doesNotMatch(p, /empty/);
  // The full body must never be inlined — only name + description.
  assert.doesNotMatch(p, /STEP1/);
});
