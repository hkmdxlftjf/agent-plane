// agent.js — one conversation session: builds tools from the resolved
// AgentConfig, drives generateText with a bounded step loop, and returns the
// final text. This is the module the multi-step reasoning-model problem
// (docs: thinking.go in cmd/agent-runtime) is being solved for — the AI SDK
// treats a provider's reasoning tokens as a first-class, separate field
// (result.reasoningText), so a model that "thinks" without yet emitting
// content/tool-calls doesn't get misread as having given a final answer.
'use strict';

import { generateText, streamText, stepCountIs } from 'ai';
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
  // the assistant's final text. Uses generateText (non-streaming) — callers
  // that want progress events should use sendStream instead.
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
      throw new Error(`no final answer (finishReason=${result.finishReason}, steps=${result.steps.length})`);
    }
    return result.text;
  }

  // sendStream is the same loop as send, but yields normalized progress
  // events as the model works — reasoning tokens, tool calls/results, and
  // answer tokens — so a caller (the SSE handler in main.js) can show the
  // user what the agent is doing instead of a multi-minute silent wait.
  // Event shapes:
  //   {type:'reasoning', delta}       incremental "thinking" text
  //   {type:'tool-call', name, args}  a tool the model is invoking
  //   {type:'tool-result', name, result}
  //   {type:'text-delta', delta}      incremental answer text
  //   {type:'text', text}             the final answer (emitted once, at the end)
  async *sendStream(userText) {
    this.messages.push({ role: 'user', content: userText });
    const result = streamText({
      model: this.model,
      system: this.system,
      messages: this.messages,
      tools: this.tools,
      stopWhen: stepCountIs(this.maxSteps),
    });

    const pendingToolNames = new Map(); // toolCallId -> name, for tool-result events (which don't carry toolName)
    for await (const part of result.fullStream) {
      switch (part.type) {
        case 'reasoning-delta':
          yield { type: 'reasoning', delta: part.text };
          break;
        case 'tool-call':
          pendingToolNames.set(part.toolCallId, part.toolName);
          yield { type: 'tool-call', name: part.toolName, args: part.input };
          break;
        case 'tool-result':
          yield { type: 'tool-result', name: pendingToolNames.get(part.toolCallId) || part.toolName, result: part.output };
          break;
        case 'text-delta':
          yield { type: 'text-delta', delta: part.text };
          break;
        default:
          break;
      }
    }

    const [finishReason, text, steps, responseMessages] = await Promise.all([
      result.finishReason, result.text, result.steps, result.response.then((r) => r.messages),
    ]);
    this.messages.push(...responseMessages);

    if (!text && finishReason !== 'stop') {
      // The model exhausted maxSteps still calling tools, or stopped for a
      // reason other than a natural finish (length, content-filter, error) —
      // report it rather than silently handing back "".
      throw new Error(`no final answer (finishReason=${finishReason}, steps=${steps.length})`);
    }
    yield { type: 'text', text };
  }
}
