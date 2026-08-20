// render_trip — the mechanical half of travel-plan-viz.
//
// The model supplies a trip JSON (the part LLMs are good at: content) and a
// design hint; this tool does the part LLMs are bad at — validates the trip
// against the upstream contract (vendor/validate.js), renders a single-file
// HTML page with the upstream engines (vendor/map.js, vendor/reminders.js)
// inlined verbatim, re-validates the produced HTML, stores it, and returns
// { artifactId, summary, warnings }. The HTML never passes through the
// model's context: the chat UI fetches it by id from /artifacts/:id.
'use strict';

const validate = require('../vendor/validate.js');

const name = 'render_trip';
const description = [
  'Render a complete single-file travel-plan HTML page (interactive map, daily timeline, booking checklist)',
  'from a trip JSON, with mechanical validation before and after rendering. Returns {artifactId, summary,',
  'warnings}; the page itself is fetched by id — do NOT copy HTML into your reply, just cite the artifactId',
  'on the last line as [artifact:<id>]. Fix the trip JSON per the reported errors and call again if',
  'validation fails.',
].join(' ');

const inputSchema = {
  type: 'object',
  properties: {
    trip: { type: 'object', description: 'The trip document per the page contract (title, startDate, preTrip, flights, hotelAreas, disclaimer, tips, reminders, days[].slots/dining)' },
    design: {
      type: 'object',
      description: 'Visual theme: { colorScheme (a CSS color), accentColor, fontFamily, customCSS }. Optional; defaults chosen per trip.',
    },
  },
  required: ['trip'],
};

// Coordinates arriving from Amap are GCJ-02; the engines and OSM tiles are
// WGS-84. The page normalizes before first use — see convertSnippet().
const convertSnippet = `
(function(){
  var trip = JSON.parse(document.getElementById('trip-data').textContent);
  if (trip.coordinateSystem === 'GCJ-02') {
    (trip.days||[]).forEach(function(d){ (d.slots||[]).forEach(function(s){
      var w = gcj02ToWgs84(s.lat, s.lng); s.lat = w.lat; s.lng = w.lng;
    }); });
  }
  window.__trip = trip;
})();
`;

// Minimal, dependency-free page shell. The engines and the data do the heavy
// lifting; design injects the per-trip look.
function page(trip, design) {
  const d = design || {};
  const color = d.colorScheme || '#4f46e5';
  const accent = d.accentColor || '#0ea5e9';
  const font = d.fontFamily || 'ui-sans-serif,system-ui,-apple-system,"PingFang SC","Microsoft YaHei",sans-serif';
  const css = d.customCSS || '';
  const json = JSON.stringify(trip).replace(/</g, '\\u003c'); // </script> can't terminate the block

  const slots = [];
  (trip.days || []).forEach((day) => (day.slots || []).forEach((s) => {
    slots.push({ lat: s.lat, lng: s.lng, name: s.name, time: day.date + ' ' + (s.time || '') });
  }));

  return `<!doctype html>
<html lang="zh-CN"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>${esc(trip.title || '旅行计划')}</title>
<link rel="stylesheet" href="https://unpkg.com/leaflet@1.9.4/dist/leaflet.css">
<script src="https://unpkg.com/leaflet@1.9.4/dist/leaflet.js"></script>
<style>
:root{--c:${color};--a:${accent};--ink:#1f2328;--muted:#6e7781;--line:#e4e7eb;--bg:#fafafa}
*{box-sizing:border-box}
body{margin:0;font-family:${font};color:var(--ink);background:var(--bg);line-height:1.6}
.wrap{max-width:960px;margin:0 auto;padding:20px 16px 60px}
h1{font-size:1.5em;margin:.2em 0}h2{font-size:1.15em;margin:1.6em 0 .5em;color:var(--c)}
.meta{color:var(--muted);font-size:.9em}
section{background:#fff;border:1px solid var(--line);border-radius:14px;padding:16px 18px;margin:14px 0}
#map{height:380px;border-radius:10px;border:1px solid var(--line)}
.day{border-left:3px solid var(--c);padding-left:14px;margin:18px 0}
.slot{display:flex;gap:12px;margin:12px 0}
.slot img{width:120px;height:80px;object-fit:cover;border-radius:8px;background:var(--line)}
.slot .b{min-width:0}
.slot .nm{font-weight:600}
.badge{display:inline-block;background:var(--c);color:#fff;border-radius:6px;font-size:.75em;padding:1px 7px;margin-left:6px;vertical-align:middle}
.rating{color:#d97706;font-size:.85em}
.opt{border:1px dashed var(--line);border-radius:10px;padding:8px 12px;margin:8px 0;font-size:.92em}
.disc{font-size:.8em;color:var(--muted);border-top:1px solid var(--line);padding-top:10px;margin-top:20px}
ul{margin:.3em 0;padding-left:1.3em}
@media (max-width:640px){
  .wrap{padding:14px 10px 40px}
  .slot{flex-direction:column}
  .slot img{width:100%;height:160px}
  #map{height:280px}
}
${css}
</style></head><body>
<div class="wrap">
<h1>${esc(trip.title || '')}</h1>
<div class="meta">${esc(trip.startDate || '')}${trip.days ? ' · ' + trip.days.length + ' 天' : ''}</div>
<h2>出发前待办</h2><section id="checklist"></section>
<h2>行前须知</h2><section id="pretrip"></section>
<h2>航班</h2><section id="flights"></section>
<h2>酒店</h2><section id="hotels"></section>
<h2>行程地图</h2><section><div id="map"></div></section>
<h2>每日行程</h2><section id="days"></section>
<h2>贴士</h2><section id="tips"></section>
<div class="disc" id="disclaimer"></div>
</div>
<script id="trip-data" type="application/json">${json}</script>
<script>/*map.js*/
${require('../vendor/map.js').toString().replace(/^[\s\S]*?\n/, '')}</script>
<script>/*reminders.js*/
${require('../vendor/reminders.js').toString().replace(/^[\s\S]*?\n/, '')}</script>
<script>${convertSnippet}</script>
<script>
(function(){
  var trip = window.__trip;
  var $ = function(id){ return document.getElementById(id); };

  $('checklist').innerHTML = renderChecklistHTML(computeReminders(trip.startDate, trip.reminders || []));

  var pt = trip.preTrip || {};
  $('pretrip').innerHTML =
    (pt.weather ? '<p><b>天气</b>：' + esc(pt.weather.summary || '') + (pt.weather.typhoon ? '；台风：' + esc(pt.weather.typhoon) : '') + '</p>' : '') +
    (pt.packing ? '<p><b>穿搭</b>：' + esc(pt.packing) + '</p>' : '') +
    (pt.payment ? '<p><b>支付</b>：' + esc(pt.payment) + '</p>' : '') +
    (pt.apps && pt.apps.length ? '<p><b>必备 App</b>：' + esc(pt.apps.join('、')) + '</p>' : '') +
    (pt.ticketTip ? '<p><b>购票时机</b>：' + esc(pt.ticketTip) + '</p>' : '');

  var fl = trip.flights || {};
  $('flights').innerHTML =
    (fl.booked || []).map(function(f){ return '<p>✅ ' + esc(f.label) + ' ' + esc(f.code || '') + ' ' + esc(f.time || '') + '</p>'; }).join('') +
    (fl.candidates || []).map(function(f){ return '<div class="opt">待选 · ' + esc(f.label) + ' ' + esc(f.code || '') + ' ' + esc(f.time || '') + (f.note ? ' — ' + esc(f.note) : '') + '</div>'; }).join('') ||
    '<p class="meta">未提供航班信息</p>';

  $('hotels').innerHTML = (trip.hotelAreas || []).map(function(h){
    return '<div class="opt"><b>' + esc(h.area) + '</b> — ' + esc(h.reason || '') +
      '<ul>' + (h.options || []).map(function(o){ return '<li>' + esc(o.tier) + '：' + esc(o.name) + '（' + esc(o.priceRange || '') + '）' + (o.note ? ' — ' + esc(o.note) : '') + '</li>'; }).join('') + '</ul></div>';
  }).join('') || '<p class="meta">未提供酒店信息</p>';

  var periodName = { morning: '上午', noon: '中午', evening: '晚上' };
  $('days').innerHTML = (trip.days || []).map(function(day){
    var html = '<div class="day"><b>' + esc(day.date) + ' ' + esc(day.weekday || '') + '</b>' +
      (day.theme ? ' · ' + esc(day.theme) : '') + '</div>';
    (day.slots || []).forEach(function(s){
      html += '<div class="slot">' +
        (s.photo ? '<img loading="lazy" src="' + esc(s.photo) + '" alt="" onerror="this.style.visibility=\'hidden\'">' : '') +
        '<div class="b"><div class="nm">' + esc(s.name) + (s.needsBooking ? reminderBadgeHTML(s.leadDays) : '') + '</div>' +
        '<div class="meta">' + esc(periodName[s.period] || '') + ' ' + esc(s.time || '') +
        (typeof s.rating === 'number' ? ' · <span class="rating">★ ' + s.rating + '</span>' : '') + '</div>' +
        (s.review ? '<div>' + esc(s.review) + '</div>' : '') +
        '<div class="meta">' + [s.openingHours, s.ticketPrice, s.transport ? (s.transport.mode + ' ' + (s.transport.fare || '') + ' ' + (s.transport.duration || '')) : '', s.seasonal]
          .filter(Boolean).map(esc).join(' · ') + '</div></div></div>';
    });
    (day.dining || []).forEach(function(m){
      html += '<div class="opt">🍜 <b>' + esc(m.meal) + '</b> ' + esc(m.place) + '（' + esc(m.hours || '') + '）' +
        (m.dishes || []).map(function(ds){ return '<li>' + esc(ds.name) + ' ' + esc(ds.price || '') + '</li>'; }).join('') + '</div>';
    });
    (day.alternatives || []).forEach(function(alt){
      html += '<div class="opt">🔀 <b>' + esc(alt.label) + '</b> ' + esc(alt.summary || '') + '</div>';
    });
    if (day.tips && day.tips.length) {
      html += '<div class="meta">当日贴士：' + esc(day.tips.join('；')) + '</div>';
    }
    return html;
  }).join('');

  $('tips').innerHTML = (trip.tips || []).map(function(t){ return '<li>' + esc(t) + '</li>'; }).join('') || '';
  $('disclaimer').textContent = trip.disclaimer || '';

  var points = [];
  (trip.days || []).forEach(function(day){ (day.slots || []).forEach(function(s){
    points.push({ lat: s.lat, lng: s.lng, name: s.name, time: day.date + ' ' + (s.time || '') });
  }); });
  if (points.length) initTravelMap('map', points);

  function esc(s){ var d = document.createElement('div'); d.textContent = s == null ? '' : String(s); return d.innerHTML; }
})();
</script>
</body></html>`;
}

function esc(s) {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}

async function execute(args, ctx) {
  const trip = args.trip;
  if (!trip || typeof trip !== 'object') {
    return { content: [{ type: 'text', text: 'error: trip must be an object' }], isError: true };
  }

  // 1. Validate the trip BEFORE rendering — errors go back to the model to fix.
  const pre = validate.validateTrip(trip);
  if (pre.errors.length) {
    return {
      content: [{ type: 'text', text: 'trip validation failed — fix and call again:\n' + pre.errors.map((e) => '  - ' + e).join('\n') }],
      isError: true,
    };
  }

  // 2. Render, then re-validate the produced HTML (catches template bugs).
  const html = page(trip, args.design);
  const post = validate.validateHTML(html);
  if (post.errors.length) {
    return {
      content: [{ type: 'text', text: 'rendered page failed validation (template bug) — please report:\n' + post.errors.map((e) => '  - ' + e).join('\n') }],
      isError: true,
    };
  }

  // 3. Store; the model gets only the id.
  const id = ctx.registerArtifact(html);
  const slotCount = (trip.days || []).reduce((n, d) => n + (d.slots || []).length, 0);
  const summary = `${trip.title} — ${trip.days.length} 天 ${slotCount} 个行程点` +
    (post.warnings.length ? `（${post.warnings.length} 条警告）` : '');
  ctx.log(`rendered artifact ${id} (${html.length} bytes)`);

  const out = { artifactId: id, summary, warnings: post.warnings };
  if (pre.warnings.length) out.tripWarnings = pre.warnings;
  return {
    content: [{ type: 'text', text: JSON.stringify(out, null, 2) }],
    isError: false,
  };
}

module.exports = { name, description, inputSchema, execute, page };
