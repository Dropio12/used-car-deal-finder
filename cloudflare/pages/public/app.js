// carbuyer dashboard: calls /api/deals (or the daily snapshot) and renders
// the table. Listing text is seller-controlled, so it is only ever inserted
// with textContent, and links are limited to http(s).
const form = document.getElementById('filters');
const status = document.getElementById('status');
const table = document.getElementById('deals');
const tbody = table.querySelector('tbody');

const money = (n) => (n == null ? '-' : Math.round(n).toLocaleString('fr-CA') + ' $');
const km = (n) => (n == null ? '-' : Math.round(n).toLocaleString('fr-CA'));

function cell(text, cls) {
  const td = document.createElement('td');
  td.textContent = text ?? '-';
  if (cls) td.className = cls;
  return td;
}

function safeUrl(u) {
  try {
    const url = new URL(u);
    return url.protocol === 'https:' || url.protocol === 'http:' ? url.href : null;
  } catch {
    return null;
  }
}

function row(d) {
  const s = d.score;
  const pct = s.discountPct;
  const tr = document.createElement('tr');
  tr.append(
    cell(`${pct > 0 ? '+' : ''}${pct.toFixed(1)} %`, 'num ' + (pct > 0 ? 'good' : pct < 0 ? 'bad' : '')),
    cell(d.year),
    cell(`${d.make ?? ''} ${d.model ?? ''}`.trim()),
    cell(d.trimText, 'trim'),
    cell(money(d.price), 'num'),
    cell(money(s.baseline), 'num'),
    cell(km(d.km), 'num'),
    cell(`${s.basis} · ${s.n} comps`),
    cell(d.sellerType === 'PrivateSeller' ? 'private' : 'dealer'),
    cell(d.city),
  );
  if (s.damaged) {
    const f = document.createElement('span');
    f.className = 'flag';
    f.textContent = 'damaged';
    tr.children[2].append(f);
  }
  const link = document.createElement('td');
  const href = safeUrl(d.url);
  if (href) {
    const a = document.createElement('a');
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.textContent = 'open';
    link.append(a);
  }
  tr.append(link);
  return tr;
}

function params() {
  const q = new URLSearchParams();
  for (const k of ['make', 'model', 'seller', 'minComps', 'limit']) {
    const v = form.elements[k].value.trim();
    if (v) q.set(k, v);
  }
  return q;
}

// The snapshot holds the top deals of all cars; filter it in the page the
// same way the API filters (case and punctuation ignored).
function matches(d, q) {
  const norm = (s) => (s ?? '').toLowerCase().replace(/[^\p{L}\p{N}]/gu, '');
  const seller = { dealer: 'Dealer', private: 'PrivateSeller' }[q.get('seller')];
  return (!q.get('make') || norm(d.make) === norm(q.get('make')))
    && (!q.get('model') || norm(d.model) === norm(q.get('model')))
    && (!seller || d.sellerType === seller);
}

async function load() {
  const q = params();
  const snapshot = form.elements.source.value === 'snapshot';
  status.textContent = 'Loading...';
  try {
    const res = await fetch(snapshot ? '/api/snapshots/latest' : `/api/deals?${q}`, {
      headers: { Accept: 'application/json' },
    });
    const type = res.headers.get('Content-Type') || '';
    if (!type.includes('json')) {
      throw new Error(`HTTP ${res.status}, not JSON (signed out of Cloudflare Access? reload the page)`);
    }
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);

    let deals = body.deals || [];
    if (snapshot) deals = deals.filter((d) => matches(d, q)).slice(0, Number(q.get('limit')) || 25);
    tbody.replaceChildren(...deals.map(row));
    table.hidden = deals.length === 0;
    const when = new Date(body.generatedAt || body.createdAt).toLocaleString();
    status.textContent = deals.length
      ? `${deals.length} of ${snapshot ? deals.length : body.matched} priced matches · ` +
        `${body.scored} of ${body.comps} stored cars priced · ${snapshot ? 'snapshot of' : 'scored'} ${when}`
      : `No priced cars match (${body.scored ?? 0} of ${body.comps ?? 0} stored cars could be priced). ` +
        'Try a lower "Min comps".';
  } catch (e) {
    table.hidden = true;
    status.textContent = `Could not load deals: ${e.message}`;
  }
}

// Keep the filters in the address bar so a view can be bookmarked.
function syncUrl() {
  const q = params();
  if (form.elements.source.value === 'snapshot') q.set('source', 'snapshot');
  history.replaceState(null, '', q.toString() ? `?${q}` : location.pathname);
}

for (const [k, v] of new URLSearchParams(location.search)) {
  if (form.elements[k]) form.elements[k].value = v;
}
form.addEventListener('submit', (e) => {
  e.preventDefault();
  syncUrl();
  load();
});
load();

// ---------------------------------------------------------------- new deals
// Snipe alerts the Worker recorded when the crawler pushed a new (or cheaper)
// listing priced well under its baseline. Refreshed every 2 minutes while
// the tab is visible.
const alertsStatus = document.getElementById('alerts-status');
const alertsList = document.getElementById('alerts-list');
const DAY = 24 * 3600 * 1000;

function ago(iso) {
  const ms = Date.now() - new Date(iso).getTime();
  if (!Number.isFinite(ms)) return '';
  const min = Math.round(ms / 60000);
  if (min < 1) return 'just now';
  if (min < 60) return `${min} min ago`;
  const h = Math.round(min / 60);
  return h < 48 ? `${h} h ago` : `${Math.round(h / 24)} days ago`;
}

function line(cls, text) {
  const el = document.createElement('span');
  el.className = cls;
  el.textContent = text;
  return el;
}

function alertCard(d) {
  const s = d.score || {};
  const li = document.createElement('li');
  const fresh = Date.now() - new Date(d.alertedAt).getTime() < DAY;
  li.className = 'alert' + (fresh ? ' fresh' : '');
  const pct = line('pct', `-${Number(s.discountPct).toFixed(1)} %`);
  if (fresh) pct.append(line('badge', 'new'));
  const car = [d.year, d.make, d.model, d.trimText].filter(Boolean).join(' ');
  li.append(
    pct,
    line('car', car || 'car'),
    line('meta', `${money(d.price)} vs ${money(s.baseline)} · ${s.n ?? '?'} comps · ${km(d.km)} km`),
    line('meta', [d.sellerType === 'PrivateSeller' ? 'private' : 'dealer', d.city, `alerted ${ago(d.alertedAt)}`]
      .filter(Boolean).join(' · ')),
  );
  if (s.damaged) li.append(line('meta flag', 'damaged'));
  const href = safeUrl(d.url);
  if (href) {
    const a = document.createElement('a');
    a.href = href;
    a.target = '_blank';
    a.rel = 'noopener noreferrer';
    a.textContent = 'open listing';
    li.append(a);
  }
  return li;
}

async function loadAlerts() {
  try {
    const res = await fetch('/api/alerts?limit=12', { headers: { Accept: 'application/json' } });
    const type = res.headers.get('Content-Type') || '';
    if (!type.includes('json')) throw new Error(`HTTP ${res.status} (signed out? reload the page)`);
    const body = await res.json();
    if (!res.ok) throw new Error(body.error || `HTTP ${res.status}`);
    const alerts = body.alerts || [];
    alertsList.replaceChildren(...alerts.map(alertCard));
    alertsList.hidden = alerts.length === 0;
    const r = body.rule || {};
    const rule = r.enabled === false ? 'alerts are off'
      : `at least ${r.minDiscountPct} % under the baseline, ${r.minComps}+ comps`;
    alertsStatus.textContent = alerts.length
      ? `${rule} · checked ${new Date().toLocaleTimeString()}`
      : `None yet (${rule}). New listings are checked each time the crawler pushes.`;
  } catch (e) {
    alertsList.hidden = true;
    alertsStatus.textContent = `Could not load new deals: ${e.message}`;
  }
}

loadAlerts();
setInterval(() => {
  if (document.visibilityState === 'visible') loadAlerts();
}, 2 * 60 * 1000);
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') loadAlerts();
});
