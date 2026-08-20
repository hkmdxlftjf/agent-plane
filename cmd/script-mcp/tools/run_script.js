// run_script — execute uploaded code in the container (soft sandbox).
//
// This is the Code Interpreter tool: the model writes a script, this tool runs
// it and returns stdout/stderr plus any files it left in out/. Languages:
// python (python3) and node. The script runs in a fresh temp directory with a
// minimal environment; files written to ./out/ are registered as artifacts and
// referenced by id (the chat UI fetches them from /artifacts/:id).
//
// Sandbox honesty: this is process-level isolation inside one container —
// timeouts, output caps, a non-root user and resource limits on the Deployment
// bound the blast radius, but the code CAN read the container filesystem and
// reach the network (deployment choice: egress open). Do not expose this
// server to untrusted multi-tenant traffic; a strong sandbox needs a dedicated
// runtime class or per-exec microVMs.
'use strict';

const fs = require('fs');
const os = require('os');
const path = require('path');
const { spawn } = require('child_process');

const name = 'run_script';
const description = [
  'Execute a script and return its output. Languages: "python" (python3) or "node".',
  'Working directory is fresh and private per call; write files you want to keep to ./out/ —',
  'each becomes a downloadable artifact id in the result. stdin is optional. Default timeout 30s.',
  'Use this for data processing, computation, or anything easier to compute than to narrate.',
].join(' ');

const inputSchema = {
  type: 'object',
  properties: {
    language: { type: 'string', enum: ['python', 'node'] },
    source: { type: 'string', description: 'Full script source. python: run by python3; node: run by node.' },
    stdin: { type: 'string', description: 'Optional text piped to the script stdin.' },
    timeoutMs: { type: 'number', description: 'Optional execution timeout in ms, default 30000, max 120000.' },
  },
  required: ['language', 'source'],
};

const MAX_OUTPUT = 64 * 1024; // per stream, truncated with a marker
const MAX_SOURCE = 512 * 1024;
const DEFAULT_TIMEOUT = 30_000;
const MAX_TIMEOUT = 120_000;

const RUNNERS = {
  python: { file: 'main.py', argv: ['python3', 'main.py'] },
  node: { file: 'index.js', argv: ['node', 'index.js'] },
};

function execute(args, ctx) {
  const lang = RUNNERS[args.language];
  if (!lang) {
    return Promise.resolve(errOut(`language must be one of ${Object.keys(RUNNERS).join(', ')}`));
  }
  const source = String(args.source || '');
  if (!source.trim()) return Promise.resolve(errOut('source is empty'));
  if (source.length > MAX_SOURCE) return Promise.resolve(errOut(`source exceeds ${MAX_SOURCE} bytes`));
  const timeout = Math.min(Math.max(Number(args.timeoutMs) || DEFAULT_TIMEOUT, 1000), MAX_TIMEOUT);

  const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'exec-'));
  fs.writeFileSync(path.join(dir, lang.file), source);
  fs.mkdirSync(path.join(dir, 'out'), { recursive: true });

  return new Promise((resolve) => {
    const t0 = Date.now();
    // detached: the child gets its own process group so a timeout can kill
    // the whole tree, not just the interpreter.
    const child = spawn(lang.argv[0], lang.argv.slice(1), {
      cwd: dir,
      detached: true,
      stdio: ['pipe', 'pipe', 'pipe'],
      env: { PATH: process.env.PATH, HOME: dir, LANG: 'C.UTF-8', PYTHONUNBUFFERED: '1' },
    });

    let stdout = '', stderr = '', truncated = false;
    const cap = (d) => {
      if (stdout.length + stderr.length > 2 * MAX_OUTPUT) { truncated = true; return; }
      return d;
    };
    child.stdout.on('data', (d) => { if (cap(d)) stdout += d; else child.kill('SIGKILL'); });
    child.stderr.on('data', (d) => { if (cap(d)) stderr += d; else child.kill('SIGKILL'); });

    let killed = false;
    const timer = setTimeout(() => {
      killed = true;
      try { process.kill(-child.pid, 'SIGKILL'); } catch { /* already gone */ }
    }, timeout);

    const finish = (code, signal) => {
      clearTimeout(timer);
      const artifacts = [];
      try {
        for (const f of fs.readdirSync(path.join(dir, 'out'))) {
          const p = path.join(dir, 'out', f);
          if (fs.statSync(p).isFile() && fs.statSync(p).size <= 20 * 1024 * 1024) {
            artifacts.push(ctx.registerArtifactFile(p));
          }
        }
      } catch { /* out/ unusable — not fatal */ }
      try { fs.rmSync(dir, { recursive: true, force: true }); } catch { /* best effort */ }

      const result = {
        exitCode: code,
        signal: signal || null,
        durationMs: Date.now() - t0,
        stdout: slice(stdout),
        stderr: slice(stderr),
        truncated,
        artifacts,
      };
      ctx.log(`run_script ${args.language}: exit=${code} ${result.durationMs}ms out=${stdout.length}B art=${artifacts.length}`);
      resolve({
        content: [{ type: 'text', text: JSON.stringify(result, null, 2) }],
        isError: code !== 0,
      });
    };

    child.on('error', (e) => { stderr += String(e); finish(-1, null); });
    child.on('close', (code, signal) => finish(code, signal));

    if (args.stdin != null) child.stdin.write(String(args.stdin));
    child.stdin.end();
  });
}

function slice(s) {
  if (s.length <= MAX_OUTPUT) return s;
  return s.slice(0, MAX_OUTPUT) + `\n…[truncated ${s.length - MAX_OUTPUT} bytes]`;
}

function errOut(msg) {
  return { content: [{ type: 'text', text: 'error: ' + msg }], isError: true };
}

module.exports = { name, description, inputSchema, execute, RUNNERS };
