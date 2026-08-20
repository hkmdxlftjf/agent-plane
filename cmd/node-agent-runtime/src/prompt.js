// prompt.js — composes the system prompt: the Agent's PromptTemplate text
// plus a flat catalog of available skills (name + description only). Mirrors
// cmd/agent-runtime's buildSystemPrompt.
'use strict';

export function buildSystemPrompt(cfg) {
  let system = cfg.prompt?.system || 'You are a helpful assistant. Use tools when they can answer the question.';
  const catalog = (cfg.skills || [])
    .filter((sk) => sk.content)
    .map((sk) => `- ${sk.name}: ${sk.description || sk.name}`);
  if (catalog.length) {
    system += '\n\n# Skills available\n' +
      'The following skills are available but their full instructions are NOT loaded. ' +
      "When a skill is relevant to the user's request, call load_skill(name) to load its " +
      'instructions before acting on that task.\n' + catalog.join('\n');
  }
  return system;
}
