// check_urls — batch-verify that image/asset URLs actually resolve.
//
// Generated content cites photos the model only "knows"; a HEAD (fallback GET)
// per URL is the mechanical check. Dead ones go back to the model to replace.
'use strict';

const name = 'check_urls';
const description = [
  'Check whether URLs are reachable (HTTP < 400). Input: { urls: string[] } (max 50).',
  'Returns per-URL status. Use before embedding image URLs in content; replace the dead ones.',
].join(' ');

const inputSchema = {
  type: 'object',
  properties: {
    urls: { type: 'array', items: { type: 'string' }, maxItems: 50 },
  },
  required: ['urls'],
};

async function execute(args) {
  const urls = (args.urls || []).slice(0, 50);
  const results = await Promise.all(urls.map(async (u) => {
    const r = { url: u, ok: false, status: 0 };
    try {
      let res = await fetch(u, { method: 'HEAD', redirect: 'follow', signal: AbortSignal.timeout(8000) });
      if (res.status === 405 || res.status === 501) { // some servers reject HEAD
        res = await fetch(u, { method: 'GET', redirect: 'follow', signal: AbortSignal.timeout(8000) });
      }
      r.status = res.status;
      r.ok = res.status < 400;
    } catch (e) {
      r.error = String(e.cause && e.cause.code || e.message).slice(0, 120);
    }
    return r;
  }));
  const dead = results.filter((r) => !r.ok);
  return {
    content: [{
      type: 'text',
      text: dead.length === 0
        ? `all ${results.length} URLs OK`
        : `dead URLs (${dead.length}/${results.length}) — replace them:\n` +
          dead.map((r) => `  - ${r.url} (status=${r.status} ${r.error || ''})`).join('\n'),
    }],
    isError: false,
  };
}

module.exports = { name, description, inputSchema, execute };
