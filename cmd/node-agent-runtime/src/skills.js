// skills.js — the load_skill local tool: advertises a name+description
// catalog in the system prompt (see prompt.js) and serves a skill's full body
// only when the model asks for it, keeping prompt size independent of how
// many skills an Agent mounts. Mirrors cmd/agent-runtime's loadSkillTool.
'use strict';

import { tool, jsonSchema } from 'ai';

export function buildLoadSkillTool(skills, log) {
  const byName = new Map();
  for (const sk of skills || []) {
    if (sk.content) byName.set(sk.name, sk);
  }
  if (byName.size === 0) return null;
  const names = [...byName.keys()];

  return tool({
    description: "Load the full instructions for a named skill from the 'Skills available' catalog " +
      'in the system prompt. Call this before acting on a task the skill covers.',
    inputSchema: jsonSchema({
      type: 'object',
      properties: { name: { type: 'string', description: 'skill name from the catalog' } },
      required: ['name'],
    }),
    execute: async ({ name }) => {
      const sk = byName.get(name);
      if (!sk) {
        return `no such skill "${name}"; available skills: ${names.join(', ')}`;
      }
      log(`[tool] load_skill(${JSON.stringify({ name })})`);
      log(`  \u21b3 ${sk.content.slice(0, 120).replace(/\n/g, ' ')}\u2026`);
      return sk.content;
    },
  });
}
