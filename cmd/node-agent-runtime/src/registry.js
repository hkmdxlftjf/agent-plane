// registry.js — client for the Agent Plane Registry protocol (docs/runtime-protocol.md).
'use strict';

export async function fetchConfig(registryBase, ns, name) {
  const url = `${registryBase.replace(/\/$/, '')}/v1/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/config`;
  const res = await fetch(url);
  if (!res.ok) {
    throw new Error(`registry config ${res.status}: ${await res.text()}`);
  }
  return res.json();
}

// watchConfig subscribes to the Registry's SSE stream and calls onChange with
// every full snapshot (including the initial one), reconnecting with a fixed
// backoff on any stream error — mirrors the Go SDK's Watch semantics (no
// resume cursor; a reconnect just gets a fresh snapshot).
export async function watchConfig(registryBase, ns, name, onChange, { signal } = {}) {
  const url = `${registryBase.replace(/\/$/, '')}/v1/agents/${encodeURIComponent(ns)}/${encodeURIComponent(name)}/watch`;
  while (!signal?.aborted) {
    try {
      const res = await fetch(url, { signal, headers: { Accept: 'text/event-stream' } });
      if (!res.ok || !res.body) throw new Error(`watch ${res.status}`);
      let buf = '';
      for await (const chunk of res.body) {
        buf += Buffer.from(chunk).toString('utf8');
        let idx;
        while ((idx = buf.indexOf('\n\n')) !== -1) {
          const frame = buf.slice(0, idx);
          buf = buf.slice(idx + 2);
          const line = frame.split('\n').find((l) => l.startsWith('data: '));
          if (line) onChange(JSON.parse(line.slice('data: '.length)));
        }
      }
    } catch (e) {
      if (signal?.aborted) return;
      console.error(`  registry stream ended (${e.message}); reconnecting in 2s`);
      await new Promise((r) => setTimeout(r, 2000));
    }
  }
}
