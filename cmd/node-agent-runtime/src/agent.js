// agent.js — one conversation session: builds tools from the resolved
// AgentConfig, drives generateText with a bounded step loop, and returns the
// final text. This is the module the multi-step reasoning-model problem
// (docs: thinking.go in cmd/agent-runtime) is being solved for — the AI SDK
// treats a provider's reasoning tokens as a first-class, separate field
// (result.reasoningText), so a model that "thinks" without yet emitting
// content/tool-calls doesn't get misread as having given a final answer.
'use strict';

import { generateText, stepCountIs } from 'ai';
import { buildTools } from './tools.js';
import { buildLoadSkillTool } from './skills.js';
import { buildSystemPrompt } from './prompt.js';

const DEFAULT_MAX_STEPS = 12;

export class Session {
  constructor(model, cfg, { maxSteps = DEFAULT_MAX_STEPS, log = console.log } = {}) {
    this.model = model;
    this.log = log;
    this.maxSteps = maxSteps;
    this.system = buildSystemPrompt(cfg);
    this.tools = buildTools(cfg.tools, log);
    const loadSkill = buildLoadSkillTool(cfg.skills, log);
    if (loadSkill) this.tools.load_skill = loadSkill;
    this.messages = [];
  }

  // send appends userText, runs the bounded tool-calling loop, and returns
  // the assistant's final text. History (including tool calls/results)
  // accumulates across calls on the same Session, matching the Go runtime's
  // per-session conversation memory.
  async send(userText) {
    this.messages.push({ role: 'user', content: userText });
    const result = await generateText({
      model: this.model,
      system: this.system,
      messages: this.messages,
      tools: this.tools,
      stopWhen: stepCountIs(this.maxSteps),
    });
    this.messages.push(...result.response.messages);

    if (!result.text && result.finishReason !== 'stop') {
      // The model exhausted maxSteps still calling tools, or stopped for a
      // reason other than a natural finish (length, content-filter, error) —
      // report it rather than silently handing back "".
      throw new Error(`no final answer (finishReason=${result.finishReason}, steps=${result.steps.length})`);
    }
    return result.text;
  }
}
