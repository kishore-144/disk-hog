'use strict';
// disk-hog dashboard. Plain JS, no build step: the server embeds this file.

const $ = (s, el = document) => el.querySelector(s);
const $$ = (s, el = document) => [...el.querySelectorAll(s)];
const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const S = {
  info: null, token: '', result: null, items: [], byId: new Map(), selected: new Set(),
  filter: { q: '', age: 0, min: 0, scope: '', risks: new Set(['safe', 'moderate', 'review']), cats: new Set() },
  sort: { key: 'size', dir: -1 }, shown: 300, scanning: false, cleaning: false,
  explorePath: '', dupes: null, dupeSel: new Set(), catLabels: {},
};

// ---- formatting --------------------------------------------------------------
function size(n) {
  if (n == null || n < 0) return '?';
  if (n < 1024) return n + ' B';
  const u = ['KB', 'MB', 'GB', 'TB', 'PB'];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < u.length - 1);
  return (n >= 100 ? n.toFixed(0) : n >= 10 ? n.toFixed(1) : n.toFixed(2)) + ' ' + u[i];
}
function ago(ms) {
  if (!ms) return '—';
  const d = (Date.now() - ms) / 1000;
  if (d < 3600) return 'now';
  if (d < 172800) return Math.round(d / 3600) + 'h';
  if (d < 86400 * 60) return Math.round(d / 86400) + 'd';
  if (d < 86400 * 730) return Math.round(d / 86400 / 30) + 'mo';
  return (d / 86400 / 365).toFixed(1) + 'y';
}
const num = (n) => Number(n || 0).toLocaleString();
const home = () => (S.info && S.info.home) || '';
function short(p) { const h = home(); return h && (p === h || p.startsWith(h + '/') || p.startsWith(h + '\\')) ? '~' + p.slice(h.length) : p; }
const catColor = (c) => `var(--c-${c}, var(--accent))`;
const riskBadge = (r) => `<span class="badge ${esc(r)}">${esc(r)}</span>`;

function toast(msg, err) {
  const d = document.createElement('div');
  d.textContent = msg;
  if (err) d.className = 'err';
  $('#toast').append(d);
  setTimeout(() => d.remove(), err ? 7000 : 4000);
}

// ---- API ---------------------------------------------------------------------
async function api(path, opts = {}) {
  const headers = { 'X-Disk-Hog-Token': S.token };
  if (opts.body !== undefined) headers['Content-Type'] = 'application/json';
  const res = await fetch(path, { method: opts.method || (opts.body !== undefined ? 'POST' : 'GET'), headers, body: opts.body !== undefined ? JSON.stringify(opts.body) : undefined });
  if (res.status === 401) {
    const t = prompt('This dashboard needs the access token.\nRun `disk-hog token` on the machine and paste it here:');
    if (t) { S.token = t.trim(); try { localStorage.setItem('dh-token', S.token); } catch {} return api(path, opts); }
  }
  const text = await res.text();
  const data = text ? JSON.parse(text) : null;
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

// ---- theme and tabs ------------------------------------------------------------
function initTheme() {
  let t = null;
  try { t = localStorage.getItem('dh-theme'); } catch {}
  if (t) document.documentElement.dataset.theme = t;
  $('#theme').onclick = () => {
    const dark = matchMedia('(prefers-color-scheme: dark)').matches;
    const cur = document.documentElement.dataset.theme || (dark ? 'dark' : 'light');
    const next = cur === 'dark' ? 'light' : 'dark';
    document.documentElement.dataset.theme = next;
    try { localStorage.setItem('dh-theme', next); } catch {}
  };
}

const loaders = {};
function showTab(name) {
  $$('#tabs button').forEach((b) => b.classList.toggle('on', b.dataset.tab === name));
  $$('main > section').forEach((s) => (s.hidden = s.id !== 'tab-' + name));
  try { localStorage.setItem('dh-tab', name); } catch {}
  if (location.hash !== '#' + name) history.replaceState(null, '', '#' + name);
  loaders[name] && loaders[name]();
}

// ---- overview ----------------------------------------------------------------------
async function loadDisks() {
  try {
    const disks = await api('/api/disks');
    $('#disks').innerHTML = (disks || []).map((d) => {
      const pct = d.total ? (d.used / d.total) * 100 : 0;
      const color = pct > 90 ? 'var(--review)' : pct > 75 ? 'var(--moderate)' : 'var(--safe)';
      return `<div class="disk"><div class="top"><span class="ellipsis mono" title="${esc(d.device)}">${esc(d.mount)}</span>
        <span class="small muted nowrap">${size(d.avail)} free of ${size(d.total)}${d.fstype ? ' · ' + esc(d.fstype) : ''}</span></div>
        <div class="bar"><span style="width:${pct.toFixed(1)}%;background:${color}"></span></div></div>`;
    }).join('') || '<div class="muted small">No filesystems reported.</div>';
  } catch (e) { $('#disks').textContent = e.message; }
}

function renderSummary(sum) {
  if (!sum) { $('#summary-card').hidden = true; $('#no-scan').hidden = S.scanning; return; }
  $('#no-scan').hidden = true;
  $('#summary-card').hidden = false;
  $('#total').textContent = size(sum.total);
  const r = { safe: 0, moderate: 0, review: 0 };
  for (const c of sum.summary || []) { r.safe += c.safe; r.moderate += c.moderate; r.review += c.review; }
  const t = sum.total || 1;
  $('#risk-bar').innerHTML = ['safe', 'moderate', 'review'].map((k) => `<span title="${k}: ${size(r[k])}" style="width:${(r[k] / t) * 100}%;background:var(--${k})"></span>`).join('');
  $('#risk-legend').innerHTML = [
    ['safe', 'regenerated automatically'], ['moderate', 'reinstall or rebuild needed'], ['review', 'look before deleting'],
  ].map(([k, d]) => `<span class="row"><span class="dot" style="background:var(--${k})"></span><b>${size(r[k])}</b> <span class="muted">${k} — ${d}</span></span>`).join('');
  $('#cats').innerHTML = (sum.summary || []).map((c) => `<div class="card cat" data-cat="${esc(c.category)}" style="--c:${catColor(c.category)}">
      <div class="small muted">${esc(c.label)}</div><div class="size">${size(c.size)}</div><div class="small muted">${num(c.count)} item${c.count === 1 ? '' : 's'}</div></div>`).join('');
  $$('#cats .cat').forEach((el) => (el.onclick = () => { S.filter.cats = new Set([el.dataset.cat]); S.filter.risks = new Set(['safe', 'moderate', 'review']); syncFilterUI(); showTab('cleanup'); }));
  const st = sum.stats || {};
  $('#scan-meta').textContent = `Scanned ${num(st.dirs)} folders and ${num(st.files)} files (${size(st.bytes)}) in ${(sum.duration || 0).toFixed(1)}s` +
    (st.errors ? ` · ${num(st.errors)} unreadable` : '') + (sum.canceled ? ' · canceled (partial)' : '') +
    ` · ${(sum.roots || []).map(short).join(', ')}`;
  $('#last-scan').textContent = sum.finished ? 'Last scan ' + ago(new Date(sum.finished).getTime()) + ' ago' : '';
  if ($('#last-scan').textContent.includes('now ago')) $('#last-scan').textContent = 'Scanned just now';
}

function scanProgress(p) {
  setScanning(true);
  $('#scan-progress').innerHTML = `<span class="spin"></span> ${num(p.dirs)} folders · ${num(p.files)} files · ${size(p.bytes)} · <b>${num(p.found)}</b> found · ${p.elapsed.toFixed(1)}s<br><span class="mono">${esc(short(p.current || ''))}</span>`;
}

function setScanning(on) {
  S.scanning = on;
  $('#scan-btn').disabled = on;
  $('#scan-cancel').hidden = !on;
  $('#live').textContent = on ? 'scanning…' : 'live';
  $('#live').classList.toggle('live', on);
  if (!on) $('#scan-progress').textContent = '';
}

async function startScan() {
  const roots = $('#roots').value.split('\n').map((s) => s.trim()).filter(Boolean);
  try {
    await api('/api/scan', { body: {
      roots, includeGlobal: $('#opt-global').checked, oneFileSystem: !$('#opt-xfs').checked,
      minItemSize: $('#opt-min').value, largeFileSize: $('#opt-large').value,
    } });
    setScanning(true);
    $('#scan-progress').innerHTML = '<span class="spin"></span> starting…';
  } catch (e) { toast(e.message, true); }
}

// ---- results and cleanup --------------------------------------------------------------
async function loadResults() {
  const res = await api('/api/results');
  S.result = res;
  S.items = (res && res.items) || [];
  S.byId = new Map(S.items.map((c) => [c.id, c]));
  for (const id of [...S.selected]) if (!S.byId.has(id)) S.selected.delete(id);
  renderSummary(res && { total: res.total, summary: res.summary, stats: res.stats, duration: res.duration, finished: res.finished, canceled: res.canceled, roots: res.roots });
  syncFilterUI();
  renderItems();
}

function syncFilterUI() {
  const cats = (S.info && S.info.categories) || [];
  const present = new Set(S.items.map((c) => c.category));
  $('#f-cat').innerHTML = cats.filter((c) => present.has(c.ID)).map((c) => {
    const n = S.items.filter((i) => i.category === c.ID).reduce((a, i) => a + i.size, 0);
    return `<button class="chip ${S.filter.cats.has(c.ID) ? 'on' : ''}" data-cat="${esc(c.ID)}"><span class="dot" style="background:${catColor(c.ID)}"></span>${esc(c.Label)} <span class="muted">${size(n)}</span></button>`;
  }).join('') + (S.filter.cats.size ? '<button class="chip" data-cat="">✕ all categories</button>' : '');
  $$('#f-cat .chip').forEach((b) => (b.onclick = () => {
    const c = b.dataset.cat;
    if (!c) S.filter.cats.clear(); else if (S.filter.cats.has(c)) S.filter.cats.delete(c); else S.filter.cats.add(c);
    S.shown = 300; syncFilterUI(); renderItems();
  }));
  const riskInfo = { safe: 'Safe', moderate: 'Moderate', review: 'Review' };
  $('#f-risk').innerHTML = Object.entries(riskInfo).map(([k, l]) => `<button class="chip ${S.filter.risks.has(k) ? 'on' : ''}" data-risk="${k}"><span class="dot" style="background:var(--${k})"></span>${l}</button>`).join('');
  $$('#f-risk .chip').forEach((b) => (b.onclick = () => {
    const r = b.dataset.risk;
    S.filter.risks.has(r) ? S.filter.risks.delete(r) : S.filter.risks.add(r);
    S.shown = 300; syncFilterUI(); renderItems();
  }));
}

function visibleItems() {
  const f = S.filter, q = f.q.toLowerCase(), now = Date.now();
  let list = S.items.filter((c) =>
    f.risks.has(c.risk) && (!f.cats.size || f.cats.has(c.category)) && c.size >= f.min &&
    (!f.scope || (f.scope === 'global') === !!c.global) &&
    (!f.age || !c.touched || now - c.touched >= f.age * 86400000) &&
    (!q || c.path.toLowerCase().includes(q) || c.name.toLowerCase().includes(q) || c.rule.includes(q)));
  const { key, dir } = S.sort;
  const val = { size: (c) => c.size, age: (c) => -(c.touched || 0), path: (c) => c.path, rule: (c) => c.name }[key];
  list.sort((a, b) => { const x = val(a), y = val(b); return (x < y ? -1 : x > y ? 1 : 0) * dir; });
  return list;
}

function renderItems() {
  if (!S.result) { $('#items').innerHTML = '<tr><td colspan="6" class="empty">Run a scan first.</td></tr>'; updateSelbar(); return; }
  const list = visibleItems();
  const rows = list.slice(0, S.shown).map((c) => {
    const proj = c.project ? `<div class="small muted ellipsis">project ${esc(short(c.project))}</div>` : '';
    const tip = [c.note, c.regen && 'Regenerate: ' + c.regen].filter(Boolean).join('\n');
    return `<tr data-id="${c.id}" class="${S.selected.has(c.id) ? 'sel' : ''}">
      <td><input type="checkbox" ${S.selected.has(c.id) ? 'checked' : ''} aria-label="select"></td>
      <td class="num"><b>${size(c.size)}</b><div class="small muted">${num(c.files)} files</div></td>
      <td><div class="row"><span class="dot" style="background:${catColor(c.category)}"></span>${esc(c.name)} ${riskBadge(c.risk)}${c.contents ? ' <span class="badge review">empties</span>' : ''}</div>${tip ? `<div class="small muted ellipsis" title="${esc(tip)}">${esc(c.note || 'Regenerate: ' + c.regen)}</div>` : ''}</td>
      <td class="path"><div class="p" title="${esc(c.path)}"><bdi>${esc(short(c.path))}</bdi></div>${proj}</td>
      <td class="num hide-sm" title="${c.touched ? new Date(c.touched).toLocaleString() : ''}">${ago(c.touched)}</td>
      <td class="row-actions"><button class="icon-btn" data-act="reveal" title="Show in file manager">📂</button><button class="icon-btn" data-act="copy" title="Copy path">⧉</button><button class="icon-btn" data-act="ignore" title="Never show this again">⊘</button></td></tr>`;
  });
  $('#items').innerHTML = rows.join('') || '<tr><td colspan="6" class="empty">Nothing matches these filters.</td></tr>';
  const total = list.reduce((a, c) => a + c.size, 0);
  const small = ((S.result && S.result.small) || []).reduce((a, s) => [a[0] + s.count, a[1] + s.size], [0, 0]);
  $('#items-more').innerHTML = `${num(list.length)} items · ${size(total)}` +
    (list.length > S.shown ? ` · <button class="btn sm" id="more">Show ${num(Math.min(500, list.length - S.shown))} more</button>` : '') +
    (small[0] ? ` · plus ${num(small[0])} items under the minimum size (${size(small[1])})` : '');
  const more = $('#more');
  if (more) more.onclick = () => { S.shown += 500; renderItems(); };
  $('#sel-all').checked = list.length > 0 && list.every((c) => S.selected.has(c.id));
  updateSelbar();
}

function updateSelbar() {
  const sel = [...S.selected].map((id) => S.byId.get(id)).filter(Boolean);
  const bytes = sel.reduce((a, c) => a + c.size, 0);
  $('#sel-info').innerHTML = sel.length ? `<b>${num(sel.length)}</b> selected · <b>${size(bytes)}</b>` : 'Nothing selected';
  const ro = S.info && S.info.readOnly;
  for (const id of ['#act-trash', '#act-delete', '#act-auto']) $(id).disabled = !sel.length || ro || S.cleaning;
  if (ro) $('#sel-info').innerHTML += ' <span class="badge review">read-only mode</span>';
}

function onItemsClick(e) {
  const tr = e.target.closest('tr[data-id]');
  if (!tr) return;
  const c = S.byId.get(+tr.dataset.id);
  const act = e.target.closest('[data-act]');
  if (act) {
    if (act.dataset.act === 'reveal') api('/api/reveal', { body: { path: c.path } }).catch((x) => toast(x.message, true));
    if (act.dataset.act === 'copy') navigator.clipboard.writeText(c.path).then(() => toast('Copied'));
    if (act.dataset.act === 'ignore') ignorePath(c.path);
    return;
  }
  if (e.target.tagName === 'A') return;
  S.selected.has(c.id) ? S.selected.delete(c.id) : S.selected.add(c.id);
  tr.classList.toggle('sel', S.selected.has(c.id));
  $('input', tr).checked = S.selected.has(c.id);
  updateSelbar();
}

async function ignorePath(p) {
  try {
    await api('/api/ignore', { body: { path: p, on: true } });
    toast('Ignored ' + short(p));
    await loadResults();
  } catch (e) { toast(e.message, true); }
}

function confirmDialog({ title, sub, body, ok, danger }) {
  return new Promise((resolve) => {
    const d = $('#confirm');
    $('#c-title').textContent = title;
    $('#c-sub').innerHTML = sub || '';
    $('#c-body').innerHTML = body || '';
    $('#c-ok').textContent = ok || 'Confirm';
    $('#c-ok').className = 'btn ' + (danger ? 'danger' : 'primary');
    const done = (v) => { d.close(); resolve(v); };
    $('#c-ok').onclick = () => done(true);
    $('#c-cancel').onclick = () => done(false);
    d.oncancel = () => resolve(false);
    d.showModal();
  });
}

async function cleanSelected(mode) {
  const sel = [...S.selected].map((id) => S.byId.get(id)).filter(Boolean).sort((a, b) => b.size - a.size);
  if (!sel.length) return;
  const bytes = sel.reduce((a, c) => a + c.size, 0);
  const plan = sel.map((c) => {
    const m = c.contents ? 'delete' : mode === 'auto' ? (c.risk === 'safe' ? 'delete' : 'trash') : mode;
    return { c, m };
  });
  const del = plan.filter((p) => p.m === 'delete').reduce((a, p) => a + p.c.size, 0);
  const tr = bytes - del;
  const words = { auto: 'Clean', trash: 'Move to Trash', delete: 'Delete permanently' };
  const review = sel.filter((c) => c.risk === 'review').length;
  const ok = await confirmDialog({
    title: `${words[mode]}: ${num(sel.length)} item${sel.length === 1 ? '' : 's'}, ${size(bytes)}`,
    sub: (del ? `<b>${size(del)}</b> deleted permanently (frees space now). ` : '') +
      (tr ? `<b>${size(tr)}</b> moved to the trash (restorable; frees space once the trash is emptied).` : '') +
      (review ? `<div class="notice small" style="margin-top:8px">${review} item${review === 1 ? ' is' : 's are'} marked <b>review</b>: not regenerable. Make sure you don't need ${review === 1 ? 'it' : 'them'}.</div>` : ''),
    body: '<ul>' + plan.slice(0, 200).map(({ c, m }) => `<li>${m === 'delete' ? '🗑' : '♻'} ${esc(size(c.size))} — ${esc(short(c.path))}</li>`).join('') + (plan.length > 200 ? `<li>… ${plan.length - 200} more</li>` : '') + '</ul>',
    ok: words[mode], danger: mode === 'delete',
  });
  if (!ok) return;
  S.cleaning = true; updateSelbar();
  try {
    const r = await api('/api/clean', { body: { ids: sel.map((c) => c.id), mode } });
    const failed = r.results.filter((x) => !x.ok);
    let msg = `Freed ${size(r.freed)}`;
    if (r.moved) msg += `, moved ${size(r.moved)} to the trash`;
    toast(msg + (failed.length ? ` · ${failed.length} failed` : ''));
    for (const f of failed.slice(0, 3)) toast(short(f.path || '') + ': ' + f.error, true);
    for (const x of r.results) if (x.ok) S.selected.delete(x.id);
    await loadResults();
    loadDisks();
  } catch (e) { toast(e.message, true); }
  S.cleaning = false; updateSelbar();
}

// ---- explore ------------------------------------------------------------------------
async function loadExplore(path = S.explorePath) {
  try {
    const v = await api('/api/tree?path=' + encodeURIComponent(path));
    S.explorePath = v.path;
    const roots = v.roots || [];
    const root = roots.find((r) => v.path === r || v.path.startsWith(r.endsWith('/') ? r : r + '/')) || '';
    let crumbs = roots.length > 1 ? '<button data-p="">roots</button> /' : '';
    if (v.path) {
      const rel = v.path.slice(root.length).split(/[\\/]/).filter(Boolean);
      let acc = root;
      crumbs += `<button data-p="${esc(root)}">${esc(short(root))}</button>`;
      for (const part of rel) { acc += (acc.endsWith('/') ? '' : '/') + part; crumbs += `/<button data-p="${esc(acc)}">${esc(part)}</button>`; }
    }
    $('#crumbs').innerHTML = crumbs;
    $$('#crumbs button').forEach((b) => (b.onclick = () => loadExplore(b.dataset.p)));
    $('#ex-info').textContent = `${size(v.size)} · ${num(v.files)} files`;
    const max = Math.max(1, ...(v.kids || []).map((k) => k.size), v.self, v.small);
    const bar = (n, color) => `<div class="bar thin"><span style="width:${((n / max) * 100).toFixed(1)}%;background:${color}"></span></div>`;
    let rows = (v.kids || []).map((k) => {
      const c = k.cand ? S.byId.get(k.cand) : null;
      const color = c ? catColor(c.category) : 'var(--muted)';
      const tag = k.cand ? ` <span class="badge ${esc(k.risk)}">${esc(k.rule)}</span>` : k.rule ? ` <span class="badge review">cleaned / below min</span>` : '';
      return `<tr data-path="${esc(k.path)}" class="${k.more ? 'folder' : ''}">
        <td class="ellipsis">${k.more ? '📁' : k.cand ? '🧹' : '📄'} ${esc(k.name)}${tag}</td><td>${bar(k.size, color)}</td>
        <td class="num">${size(k.size)}</td><td class="num hide-sm">${num(k.files)}</td>
        <td class="row-actions">${k.cand ? `<button class="btn sm" data-sel="${k.cand}">Select</button>` : ''}<button class="icon-btn" data-reveal title="Show in file manager">📂</button></td></tr>`;
    });
    if (v.self > 0) rows.push(`<tr><td class="muted">(files directly here)</td><td>${bar(v.self, 'var(--border)')}</td><td class="num">${size(v.self)}</td><td></td><td></td></tr>`);
    if (v.small > 0) rows.push(`<tr><td class="muted">(${num(v.smallN)} folders under 1 MB)</td><td>${bar(v.small, 'var(--border)')}</td><td class="num">${size(v.small)}</td><td></td><td></td></tr>`);
    $('#ex-rows').innerHTML = rows.join('') || '<tr><td colspan="5" class="empty">Empty</td></tr>';
  } catch (e) {
    $('#crumbs').innerHTML = '';
    $('#ex-rows').innerHTML = `<tr><td colspan="5" class="empty">${esc(e.message)}</td></tr>`;
  }
}

function onExploreClick(e) {
  const tr = e.target.closest('tr[data-path]');
  if (!tr) return;
  const sel = e.target.closest('[data-sel]');
  if (sel) { S.selected.add(+sel.dataset.sel); toast('Selected — see the Cleanup tab'); updateSelbar(); return; }
  if (e.target.closest('[data-reveal]')) { api('/api/reveal', { body: { path: tr.dataset.path } }).catch((x) => toast(x.message, true)); return; }
  if (tr.classList.contains('folder')) loadExplore(tr.dataset.path);
}

// ---- duplicates -------------------------------------------------------------------------
async function loadDupes() {
  try { S.dupes = await api('/api/dupes'); } catch { S.dupes = null; }
  renderDupes();
}

function renderDupes() {
  const d = S.dupes;
  $('#d-tools').hidden = !d || !d.groups || !d.groups.length;
  if (!d) { $('#d-groups').innerHTML = '<div class="empty">Find files with identical contents. Hard links are not counted: they take no extra space.</div>'; return; }
  const groups = d.groups || [];
  $('#d-summary').innerHTML = `<b>${size(d.wasted)}</b> wasted in ${num(groups.length)} groups <span class="muted small">(${(d.roots || []).map(short).join(', ')}, ≥ ${size(d.minSize)})</span>`;
  $('#d-groups').innerHTML = groups.slice(0, 400).map((g) => `<div class="group" data-g="${g.id}">
      <div class="gh"><b>${size(g.size)}</b> × ${g.files.length} <span class="muted small">${size(g.wasted)} wasted</span><span class="right mono small muted">${esc(g.hash.slice(0, 12))}</span></div>
      ${g.files.map((f) => `<label class="gf"><input type="checkbox" data-f="${f.id}" ${S.dupeSel.has(f.id) ? 'checked' : ''}><span class="p" title="${esc(f.path)}">${esc(short(f.path))}</span><span class="small muted nowrap">${ago(f.mtime)}</span></label>`).join('')}
    </div>`).join('') + (groups.length > 400 ? `<div class="muted small">… ${groups.length - 400} smaller groups not shown</div>` : '') ||
    `<div class="empty">No duplicates ≥ ${size(d.minSize)} found.</div>`;
  updateDupeSel();
}

function updateDupeSel() {
  let bytes = 0;
  for (const g of (S.dupes && S.dupes.groups) || []) for (const f of g.files) if (S.dupeSel.has(f.id)) bytes += g.size;
  const n = S.dupeSel.size;
  $('#d-trash').disabled = $('#d-delete').disabled = !n;
  $('#d-trash').textContent = n ? `Trash ${n} (${size(bytes)})` : 'Trash selected';
  $('#d-delete').textContent = n ? `Delete ${n} (${size(bytes)})` : 'Delete selected';
}

function dupeKeep(which) {
  S.dupeSel.clear();
  for (const g of (S.dupes && S.dupes.groups) || []) {
    const sorted = [...g.files].sort((a, b) => a.mtime - b.mtime);
    const keep = which === 'old' ? sorted[0] : sorted[sorted.length - 1];
    for (const f of g.files) if (f !== keep) S.dupeSel.add(f.id);
  }
  renderDupes();
}

async function dupeClean(mode) {
  const ids = [...S.dupeSel];
  const ok = await confirmDialog({
    title: `${mode === 'delete' ? 'Delete' : 'Trash'} ${ids.length} duplicate file${ids.length === 1 ? '' : 's'}?`,
    sub: 'At least one copy of every group is always kept.', ok: mode === 'delete' ? 'Delete' : 'Move to Trash', danger: mode === 'delete',
  });
  if (!ok) return;
  try {
    const r = await api('/api/dupes/clean', { body: { ids, mode } });
    const failed = r.results.filter((x) => !x.ok);
    toast(`${r.results.length - failed.length} removed${mode === 'delete' ? ', freed ' + size(r.freed) : ''}${failed.length ? ' · ' + failed.length + ' failed' : ''}`, failed.length > 0);
    S.dupeSel.clear();
    loadDupes(); loadDisks();
  } catch (e) { toast(e.message, true); }
}

// ---- system and containers -------------------------------------------------------------------
async function loadSystem(refresh) {
  $('#sys-items').innerHTML = '<div class="empty"><span class="spin"></span> Measuring…</div>';
  try {
    const r = await api('/api/system' + (refresh ? '?refresh=1' : ''));
    $('#sys-items').innerHTML = (r.items || []).map((it) => `<div class="item">
      <div class="row"><b>${esc(it.title)}</b><span class="pill">${esc(it.group)}</span><span class="right"><b>${it.size < 0 ? '—' : (it.approx ? '≥ ' : '') + size(it.size)}</b></span></div>
      ${it.note ? `<div class="small muted">${esc(it.note)}</div>` : ''}
      <div class="row"><pre class="cmd grow">${esc(it.command)}</pre><button class="icon-btn" data-copy="${esc(it.command)}" title="Copy command">⧉</button></div></div>`).join('') ||
      '<div class="empty">Nothing to suggest on this system.</div>';
  } catch (e) { $('#sys-items').innerHTML = `<div class="notice err">${esc(e.message)}</div>`; }
}

async function loadDocker() {
  $('#dk').innerHTML = '<div class="empty"><span class="spin"></span></div>';
  try {
    const r = await api('/api/docker');
    let html = (r.errors || []).map((e) => `<div class="notice small">${esc(e)}</div>`).join('');
    html += (r.engines || []).map((u) => {
      if (u.error) return `<div class="notice err">${esc(u.engine)}: ${esc(u.error)}</div>`;
      const rows = [
        ['Stopped containers', `${u.stopped} of ${u.containers}`, u.stoppedSize, 'containers'],
        ['Dangling images', `${u.dangling}`, u.danglingSize, 'dangling'],
        ['Unused images', `${u.unusedImages} of ${u.images} (${size(u.imagesSize)} total)`, u.unusedSize, 'images'],
        ['Build cache', size(u.buildCache) + ' total', u.buildReclaim, 'build'],
        ['Unused volumes', `${u.unusedVolumes} of ${u.volumes}`, u.unusedVolSize, 'volumes'],
      ];
      return `<h3>${esc(u.engine)} <span class="muted mono">${esc(u.socket)}</span></h3><table><tbody>${rows.map(([n, d, b, w]) => `<tr>
        <td>${n}${w === 'volumes' ? ' <span class="badge review">data loss</span>' : ''}</td><td class="muted small">${d}</td><td class="num"><b>${size(b)}</b></td>
        <td class="row-actions"><button class="btn sm ${w === 'volumes' ? 'danger' : ''}" data-prune="${w}" data-sock="${esc(u.socket)}" ${b > 0 || w === 'containers' ? '' : 'disabled'}>Prune</button></td></tr>`).join('')}</tbody></table>`;
    }).join('');
    $('#dk').innerHTML = html || '<div class="empty">No Docker or Podman engine reachable.</div>';
  } catch (e) { $('#dk').innerHTML = `<div class="notice err">${esc(e.message)}</div>`; }
}

async function prune(btn) {
  const what = btn.dataset.prune;
  const text = { containers: 'Remove all stopped containers?', dangling: 'Remove dangling (untagged) images?', images: 'Remove every image not used by a container?', build: 'Clear the unused build cache?', volumes: 'Remove every volume not used by a container? Data in them is lost.' }[what];
  if (!(await confirmDialog({ title: text, ok: 'Prune', danger: what === 'volumes' || what === 'images' }))) return;
  btn.disabled = true;
  try {
    const r = await api('/api/docker/prune', { body: { socket: btn.dataset.sock, what } });
    toast('Reclaimed ' + size(r.reclaimed));
    loadDocker(); loadDisks();
  } catch (e) { toast(e.message, true); btn.disabled = false; }
}

// ---- history and settings ---------------------------------------------------------------------
async function loadHistory() {
  const r = await api('/api/history');
  $('#h-freed').innerHTML = `Freed in total: <b>${size(r.freed)}</b>`;
  $('#h-rows').innerHTML = (r.entries || []).slice(0, 500).map((e) => `<tr><td class="nowrap small">${new Date(e.time).toLocaleString()}</td>
    <td>${esc(e.mode)}</td><td class="num">${size(e.size)}</td><td class="path"><div class="p" title="${esc(e.path)}"><bdi>${esc(short(e.path))}</bdi></div></td>
    <td>${e.ok ? '<span class="badge safe">ok</span>' : `<span class="badge review" title="${esc(e.error)}">failed</span>`}</td></tr>`).join('') ||
    '<tr><td colspan="5" class="empty">Nothing cleaned yet.</td></tr>';
}

async function loadSettings() {
  const i = S.info;
  $('#cfg-path').textContent = i.configPath;
  $('#cfg').textContent = JSON.stringify(i.config, null, 2);
  $('#keep-marker').textContent = i.keepMarker;
  const ig = await api('/api/ignores');
  $('#ig-list').innerHTML = (ig || []).map((p) => `<div class="row item"><span class="mono small grow ellipsis">${esc(short(p))}</span><button class="btn sm" data-unignore="${esc(p)}">Remove</button></div>`).join('') || '<div class="small muted">No ignored paths.</div>';
  renderRules();
}

function renderRules() {
  const q = $('#r-q').value.toLowerCase();
  $('#r-rows').innerHTML = (S.info.rules || []).filter((r) => !q || (r.ID + r.Name + r.Category).toLowerCase().includes(q)).map((r) => `<tr>
    <td class="mono small">${esc(r.ID)}</td><td>${esc(r.Name)}</td><td><span class="dot" style="background:${catColor(r.Category)}"></span> ${esc(S.catLabels[r.Category] || r.Category)}</td>
    <td>${riskBadge(r.Risk)}</td><td class="small muted">${esc(r.Kind)}</td><td class="small muted hide-sm">${esc(r.Regen || r.Note || '')}</td></tr>`).join('');
}

// ---- live events --------------------------------------------------------------------------------
function connectEvents() {
  const es = new EventSource('/api/events' + (S.token && S.info.needsToken ? '?token=' + encodeURIComponent(S.token) : ''));
  es.onopen = () => { if (!S.scanning) $('#live').textContent = 'live'; };
  es.onerror = () => { $('#live').textContent = 'reconnecting…'; };
  es.addEventListener('status', (e) => {
    const st = JSON.parse(e.data);
    if (st.scan) scanProgress(st.scan); else setScanning(false);
    if (st.dupes) dupesProgress(st.dupes);
  });
  es.addEventListener('scan.progress', (e) => scanProgress(JSON.parse(e.data)));
  es.addEventListener('scan.done', async (e) => {
    const s = JSON.parse(e.data);
    setScanning(false);
    toast(s.canceled ? 'Scan canceled' : `Scan done: ${size(s.total)} reclaimable`);
    await loadResults();
    if (!$('#tab-explore').hidden) loadExplore('');
  });
  es.addEventListener('clean.progress', (e) => {
    const p = JSON.parse(e.data);
    $('#sel-info').innerHTML = `<span class="spin"></span> ${p.done + 1}/${p.total} · ${esc(short(p.path))}`;
  });
  es.addEventListener('dupes.progress', (e) => dupesProgress(JSON.parse(e.data)));
  es.addEventListener('dupes.done', () => { $('#d-progress').textContent = ''; $('#d-run').disabled = false; $('#d-cancel').hidden = true; loadDupes(); });
}

function dupesProgress(p) {
  $('#d-run').disabled = true;
  $('#d-cancel').hidden = false;
  $('#d-progress').innerHTML = `<span class="spin"></span> ${esc(p.phase)} · ${num(p.files)} candidate files` + (p.toHash ? ` · hashed ${num(p.hashed)}/${num(p.toHash)} · ${size(p.bytes)} read` : '') + ` · ${p.elapsed.toFixed(1)}s`;
}

// ---- wiring -------------------------------------------------------------------------------------
function wire() {
  $$('#tabs button').forEach((b) => (b.onclick = () => showTab(b.dataset.tab)));
  $('#scan-btn').onclick = startScan;
  $('#scan-cancel').onclick = () => api('/api/scan/cancel', { body: {} });
  $('#goto-cleanup').onclick = () => showTab('cleanup');
  $('#sel-safe').onclick = () => { S.filter.risks = new Set(['safe']); S.filter.cats.clear(); syncFilterUI(); showTab('cleanup'); for (const c of visibleItems()) S.selected.add(c.id); renderItems(); };

  let qt;
  $('#f-q').oninput = (e) => { clearTimeout(qt); qt = setTimeout(() => { S.filter.q = e.target.value; S.shown = 300; renderItems(); }, 120); };
  $('#f-age').onchange = (e) => { S.filter.age = +e.target.value; renderItems(); };
  $('#f-min').onchange = (e) => { S.filter.min = +e.target.value; renderItems(); };
  $('#f-scope').onchange = (e) => { S.filter.scope = e.target.value; renderItems(); };
  $$('th.sortable').forEach((th) => (th.onclick = () => {
    const k = th.dataset.sort;
    S.sort = { key: k, dir: S.sort.key === k ? -S.sort.dir : k === 'path' || k === 'rule' ? 1 : -1 };
    renderItems();
  }));
  $('#items').onclick = onItemsClick;
  $('#sel-all').onchange = (e) => { for (const c of visibleItems()) e.target.checked ? S.selected.add(c.id) : S.selected.delete(c.id); renderItems(); };
  $('#act-clear').onclick = () => { S.selected.clear(); renderItems(); };
  $('#act-auto').onclick = () => cleanSelected('auto');
  $('#act-trash').onclick = () => cleanSelected('trash');
  $('#act-delete').onclick = () => cleanSelected('delete');

  $('#ex-rows').onclick = onExploreClick;

  $('#d-run').onclick = async () => {
    try {
      await api('/api/dupes', { body: { roots: $('#d-roots').value.split(',').map((s) => s.trim()).filter(Boolean), minSize: $('#d-min').value, skipDeps: !$('#d-all').checked } });
      S.dupeSel.clear();
      dupesProgress({ phase: 'starting', files: 0, elapsed: 0 });
    } catch (e) { toast(e.message, true); }
  };
  $('#d-cancel').onclick = () => api('/api/dupes/cancel', { body: {} });
  $('#d-groups').onchange = (e) => { const id = +e.target.dataset.f; if (!id) return; e.target.checked ? S.dupeSel.add(id) : S.dupeSel.delete(id); updateDupeSel(); };
  $('#d-keep-old').onclick = () => dupeKeep('old');
  $('#d-keep-new').onclick = () => dupeKeep('new');
  $('#d-clear').onclick = () => { S.dupeSel.clear(); renderDupes(); };
  $('#d-trash').onclick = () => dupeClean('trash');
  $('#d-delete').onclick = () => dupeClean('delete');

  $('#sys-refresh').onclick = () => loadSystem(true);
  $('#dk-refresh').onclick = loadDocker;
  $('#tab-system').onclick = (e) => {
    const c = e.target.closest('[data-copy]');
    if (c) navigator.clipboard.writeText(c.dataset.copy).then(() => toast('Command copied'));
    const p = e.target.closest('[data-prune]');
    if (p) prune(p);
  };

  $('#ig-add').onclick = async () => {
    const p = $('#ig-path').value.trim();
    if (!p) return;
    await ignorePath(p.startsWith('~') ? home() + p.slice(1) : p);
    $('#ig-path').value = '';
    loadSettings();
  };
  $('#ig-list').onclick = async (e) => {
    const b = e.target.closest('[data-unignore]');
    if (!b) return;
    await api('/api/ignore', { body: { path: b.dataset.unignore, on: false } });
    toast('Will show again after the next scan');
    loadSettings();
  };
  $('#r-q').oninput = renderRules;

  let sysLoaded = false;
  loaders.explore = () => loadExplore(S.explorePath);
  loaders.dupes = () => { if (!S.dupes) loadDupes(); };
  loaders.system = () => { if (!sysLoaded) { sysLoaded = true; loadSystem(false); loadDocker(); } };
  loaders.history = loadHistory;
  loaders.settings = loadSettings;
  loaders.cleanup = renderItems;
  loaders.overview = loadDisks;
}

async function init() {
  initTheme();
  wire();
  try { S.token = localStorage.getItem('dh-token') || ''; } catch {}
  try {
    S.info = await api('/api/info');
  } catch (e) {
    document.body.innerHTML = `<main><div class="card notice err">Cannot reach disk-hog: ${esc(e.message)}</div></main>`;
    return;
  }
  if (S.info.token) S.token = S.info.token;
  for (const c of S.info.categories || []) S.catLabels[c.ID] = c.Label;
  $('#host').textContent = `${S.info.hostname} · ${S.info.os}/${S.info.arch} · ${S.info.version}`;
  $('#roots').value = (S.info.roots || []).join('\n');
  $('#opt-global').checked = S.info.config.includeGlobal;
  $('#opt-xfs').checked = !S.info.config.oneFileSystem;
  const large = S.info.config.largeFileSize;
  if (![...$('#opt-large').options].some((o) => o.value === large)) $('#opt-large').add(new Option(large, large, true, true)); else $('#opt-large').value = large;
  const min = S.info.config.minItemSize;
  if (![...$('#opt-min').options].some((o) => o.value === min)) $('#opt-min').add(new Option(min, min, true, true)); else $('#opt-min').value = min;
  await loadResults().catch((e) => toast(e.message, true));
  connectEvents();
  let tab = location.hash.slice(1);
  if (!tab) { try { tab = localStorage.getItem('dh-tab'); } catch {} }
  showTab($(`#tabs [data-tab="${tab}"]`) ? tab : 'overview');
  setInterval(() => { if (!document.hidden && !$('#tab-overview').hidden) loadDisks(); }, 30000);
}

init();
