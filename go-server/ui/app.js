'use strict';
// EMBFinder — app.js
// Zero external dependencies. Reads/writes only class names from style.css.

// ── Helpers ───────────────────────────────────────────────────
const $ = id => document.getElementById(id);
const esc = s => String(s || '').replace(/[&<>"]/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

/**
 * Wrapper for the native Fetch API to automatically handle JSON parsing and HTTP errors.
 * @param {string} url - The endpoint URL
 * @param {Object} [opts] - Fetch options
 * @returns {Promise<Object>} The parsed JSON response
 */
async function apiFetch(url, opts) {
  const r = await fetch(url, opts);
  if (!r.ok) throw new Error(r.statusText);
  return r.json();
}
const apiGet  = url => apiFetch(url);
const apiPost = (url, body) => apiFetch(url, { method: 'POST', body });
const apiDel  = url => apiFetch(url, { method: 'DELETE' });

/**
 * Displays a temporary popup notification to the user.
 * @param {string} msg - The message to display
 * @param {'err'|'ok'} type - The type of notification
 */
function toast(msg, type) {
  const dot = document.createElement('span');
  dot.style.cssText = `width:7px;height:7px;border-radius:50%;flex-shrink:0;background:${type === 'err' ? 'var(--c-danger)' : 'var(--c-success)'}`;
  const t = document.createElement('div');
  t.className = 'toast';
  t.append(dot, esc(msg));
  $('toastStack').appendChild(t);
  setTimeout(() => { t.style.opacity = '0'; t.style.transition = 'opacity .3s'; setTimeout(() => t.remove(), 320); }, 2800);
}

// ── State ─────────────────────────────────────────────────────
let queryFile = null;
let pollTimer = null;
let logLen    = 0;
let indexedCount = 0;

// ── Boot ──────────────────────────────────────────────────────
(async function boot() {
  wireDropZone();
  wirePaste();
  wireSidebar();
  wireActions();
  await Promise.all([refreshStatus(), loadDrives()]);
})();

setInterval(refreshStatus, 10000);

// ── Status ────────────────────────────────────────────────────
async function refreshStatus() {
  try {
    const d = await apiGet('/api/status');
    indexedCount = d.total_indexed || 0;
    $('statusTxt').textContent = indexedCount.toLocaleString() + ' indexed';
    const dot = $('dot');
    dot.className = 'dot ' + (d.embedder_ready ? 'dot--ok' : 'dot--err');
    $('resultsTitle').textContent = indexedCount > 0
      ? indexedCount.toLocaleString() + ' designs indexed — upload an image to search'
      : 'No designs indexed — use the panel to index your library';
    $('searchCount').textContent = indexedCount.toLocaleString();
    if (d.indexing) startPoll(); else stopPoll();
  } catch {
    $('statusTxt').textContent = 'Offline';
    $('dot').className = 'dot dot--err';
  }
}
$('refreshBtn').addEventListener('click', refreshStatus);

// ── Drives & formats ──────────────────────────────────────────
async function loadDrives() {
  try {
    const d = await apiGet('/api/drives');
    renderDrives(d.drives || []);
    renderFormats(d.formats || []);
  } catch { /* server warming up */ }
}

function renderDrives(drives) {
  const list = $('driveList');
  if (!drives.length) {
    list.innerHTML = '<div class="color-muted" style="font-size:11px;padding:4px 2px">No drives detected</div>';
    return;
  }
  list.innerHTML = '';
  drives.forEach((drv, i) => {
    const item = document.createElement('div');
    item.className = 'drive-item';
    item.dataset.path = drv.path;
    item.innerHTML =
      '<span class="drive-item__icon">' + driveIcon(drv.path) + '</span>' +
      '<span class="drive-item__label" title="' + esc(drv.path) + '">' + esc(drv.label) + '</span>';
    item.addEventListener('click', () => selectDrive(drv.path, item));
    list.appendChild(item);
    // Auto-select first drive
    if (i === 0) selectDrive(drv.path, item);
  });
}

function selectDrive(path, el) {
  document.querySelectorAll('.drive-item').forEach(i => i.classList.remove('is-selected'));
  el.classList.add('is-selected');
  $('folderInput').value = path;
  $('indexBtn').disabled = false;
  $('scanInfo').style.display = 'none';
  autoScan(path);
}

function driveIcon(path) {
  const isRemovable = path.includes('/media') || path.includes('/mnt') || path.includes('/Volumes') || /[a-z]:\\/i.test(path);
  if (isRemovable) return '<svg width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.4"><ellipse cx="7" cy="7" rx="6" ry="6"/><circle cx="7" cy="7" r="2.2"/></svg>';
  if (path.includes('home') || path === '/home') return '<svg width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M1 6l6-5 6 5v7H1z"/><path d="M5 13V9h4v4"/></svg>';
  return '<svg width="13" height="13" viewBox="0 0 14 14" fill="none" stroke="currentColor" stroke-width="1.4"><path d="M2 3h10l1.5 4H.5L2 3z"/><rect x=".5" y="7" width="13" height="5.5" rx="1"/></svg>';
}

function renderFormats(fmts) {
  const imgExts = new Set(['.jpg','.jpeg','.png','.webp','.gif','.bmp','.tiff','.tif','.heic','.avif']);
  const emb = fmts.filter(f => !imgExts.has(f)).sort();
  const img = fmts.filter(f => imgExts.has(f)).sort();
  $('fmtWrap').innerHTML = [...emb, ...img].map(f => `<span class="tag">${esc(f)}</span>`).join('');
}

// ── Auto scan ─────────────────────────────────────────────────
async function autoScan(folder) {
  if (!folder) return;
  try {
    const d = await apiGet('/api/scan?folder=' + encodeURIComponent(folder));
    showScanResult(d);
  } catch { /* silent */ }
}

// ── Manual scan ───────────────────────────────────────────────
$('scanBtn').addEventListener('click', async () => {
  const folder = $('folderInput').value.trim();
  if (!folder) return;
  $('scanBtn').textContent = '…'; $('scanBtn').disabled = true;
  try {
    const d = await apiGet('/api/scan?folder=' + encodeURIComponent(folder));
    showScanResult(d);
    $('indexBtn').disabled = !d.total_files;
  } catch (e) { toast(e.message, 'err'); }
  finally { $('scanBtn').textContent = 'Scan'; $('scanBtn').disabled = false; }
});

function showScanResult(d) {
  const box = $('scanInfo');
  box.style.display = 'block';
  if (d.error) {
    box.innerHTML = `<div class="info-box info-box--err">${esc(d.error)}</div>`;
    return;
  }
  const tags = Object.entries(d.formats || {})
    .map(([k, v]) => `<span class="tag">.${esc(k)} (${v})</span>`).join('');
  box.innerHTML = `<div class="info-box"><strong>${(d.total_files || 0).toLocaleString()} files found</strong><div class="tag-wrap" style="margin-top:6px">${tags || '<span class="color-muted" style="font-size:11px">No supported files</span>'}</div></div>`;
  $('indexBtn').disabled = !d.total_files;
}

$('folderInput').addEventListener('input', () => {
  $('indexBtn').disabled = !$('folderInput').value.trim();
  $('scanInfo').style.display = 'none';
});

// ── Index ─────────────────────────────────────────────────────
$('indexBtn').addEventListener('click', async () => {
  const folder = $('folderInput').value.trim();
  if (!folder) return;
  $('indexBtn').disabled = true;
  logLen = 0;
  $('logBox').innerHTML = '';
  $('progFill').style.width = '0';
  $('progressWrap').style.display = 'block';
  try {
    const fd = new FormData();
    fd.append('folder', folder);
    fd.append('force', $('forceCheck').checked ? 'true' : 'false');
    const d = await apiPost('/api/index', fd);
    if (d.error) { toast(d.error, 'err'); $('indexBtn').disabled = false; return; }
    startPoll();
  } catch (e) { toast(e.message, 'err'); $('indexBtn').disabled = false; }
});

function startPoll() { if (!pollTimer) pollTimer = setInterval(pollIndex, 600); }
function stopPoll()  { clearInterval(pollTimer); pollTimer = null; }

async function pollIndex() {
  try {
    const d = await apiGet('/api/index/state');
    const { progress = 0, total = 0, status, log = [] } = d;
    const pct = total > 0 ? Math.round(progress / total * 100) : 0;
    $('progFill').style.width = pct + '%';
    $('progLabel').textContent = status === 'done' ? '✓ Done' : `Indexing ${pct}%`;
    $('progCount').textContent = `${progress} / ${total}`;
    const box = $('logBox');
    log.slice(logLen).forEach(line => {
      const div = document.createElement('div');
      div.textContent = line;
      div.className = line.startsWith('OK') ? 'log--ok' : line.startsWith('FAIL') ? 'log--fail' : 'log--skip';
      box.appendChild(div);
    });
    logLen = log.length;
    if (logLen) box.scrollTop = box.scrollHeight;
    if (!d.running && status === 'done') {
      stopPoll();
      $('indexBtn').disabled = false;
      await refreshStatus();
      toast('Indexing complete ✓');
    }
  } catch { stopPoll(); $('indexBtn').disabled = false; }
}

// ── Clear ─────────────────────────────────────────────────────
$('clearBtn').addEventListener('click', async () => {
  if (!confirm('Clear all indexed designs? This cannot be undone.')) return;
  try {
    const d = await apiDel('/api/index');
    toast(`Cleared ${d.cleared || 0} designs`);
    $('grid').innerHTML = '';
    await refreshStatus();
  } catch (e) { toast(e.message, 'err'); }
});

// ── Drop zone ─────────────────────────────────────────────────
function wireDropZone() {
  const dz = $('dropZone');

  $('fileInput').addEventListener('change', e => {
    if (e.target.files[0]) setImage(e.target.files[0]);
  });

  dz.addEventListener('dragenter', e => { e.preventDefault(); dz.classList.add('is-over'); });
  dz.addEventListener('dragover',  e => { e.preventDefault(); dz.classList.add('is-over'); });
  dz.addEventListener('dragleave', e => { if (!dz.contains(e.relatedTarget)) dz.classList.remove('is-over'); });
  dz.addEventListener('drop', e => {
    e.preventDefault(); dz.classList.remove('is-over');
    const f = e.dataTransfer.files[0];
    if (f && f.type.startsWith('image/')) setImage(f);
  });

  // Whole-window drop
  window.addEventListener('dragover', e => e.preventDefault());
  window.addEventListener('drop', e => {
    e.preventDefault();
    const f = e.dataTransfer.files[0];
    if (f && f.type.startsWith('image/')) { setImage(f); toast('Image dropped'); }
  });

  $('changeImg').addEventListener('click', e => {
    e.stopPropagation();
    $('fileInput').click();
  });
}

// ── Paste ─────────────────────────────────────────────────────
function wirePaste() {
  document.addEventListener('paste', e => {
    const items = [...(e.clipboardData?.items || [])];
    const img = items.find(i => i.type.startsWith('image/'));
    if (img) { setImage(img.getAsFile()); toast('Image pasted ✓'); }
  });
}

// ── Set query image ───────────────────────────────────────────
function setImage(f) {
  queryFile = f;
  $('previewThumb').src = URL.createObjectURL(f);
  $('previewName').textContent = f.name || 'Pasted image';
  $('dzEmpty').style.display = 'none';
  $('dzPreview').classList.add('is-visible');
  $('searchBtn').disabled = false;
  // Auto-search if library is ready
  if (indexedCount > 0) doSearch();
}

// ── Sidebar toggle (mobile) ───────────────────────────────────
function wireSidebar() {
  $('sidebarToggle').addEventListener('click', () => {
    const sb = $('sidebar');
    const open = sb.classList.toggle('is-open');
    $('sidebarToggle').classList.toggle('is-active', open);
  });
}

// ── Wire misc buttons ─────────────────────────────────────────
function wireActions() {
  $('searchBtn').addEventListener('click', doSearch);
}

// ── Search ────────────────────────────────────────────────────
/**
 * Executes the visual search against the backend API using the uploaded image.
 * Triggers UI transitions and renders the resulting grid.
 * @returns {Promise<void>}
 */
async function doSearch() {
  if (!queryFile) return;
  $('searchBtn').disabled = true;
  $('searchingBar').classList.add('is-visible');
  $('grid').innerHTML = '';
  $('emptyState').style.display = 'none';
  $('resultsMeta').textContent = '';

  const t0 = Date.now();
  try {
    const fd = new FormData();
    fd.append('file', queryFile);
    fd.append('top_k', '30');
    const d = await apiPost('/api/search', fd);
    const ms = Date.now() - t0;

    if (d.error) { toast(d.error, 'err'); $('resultsTitle').textContent = d.error; return; }

    const results = d.results || [];
    $('resultsTitle').textContent = `${results.length} similar designs`;
    $('resultsMeta').textContent = `${(d.total_indexed || 0).toLocaleString()} searched · ${ms}ms`;

    if (!results.length) { $('emptyState').style.display = 'flex'; return; }
    renderGrid(results);
  } catch (e) {
    toast('Search failed: ' + e.message, 'err');
  } finally {
    $('searchBtn').disabled = false;
    $('searchingBar').classList.remove('is-visible');
  }
}

// ── Grid ──────────────────────────────────────────────────────
function renderGrid(results) {
  const grid = $('grid');
  grid.innerHTML = '';
  results.forEach(r => {
    const pct  = Math.round((r.score || 0) * 100);
    const sCls = pct >= 70 ? 'card__score--hi' : pct >= 45 ? 'card__score--md' : 'card__score--lo';
    const card = document.createElement('div');
    card.className = 'card';
    card.innerHTML =
      '<div class="card__thumb">' +
        (r.has_preview
          ? `<img src="/api/preview/${esc(r.id)}" loading="lazy" decoding="async" alt="">`
          : `<div class="no-thumb">${placeholderSvg(r.format)}<br>.${esc((r.format||'?').toUpperCase())}</div>`) +
        `<span class="card__score ${sCls}">${pct}%</span>` +
      '</div>' +
      '<div class="card__body">' +
        `<div class="card__name" title="${esc(r.file_name)}">${esc(r.file_name)}</div>` +
        `<div class="card__meta"><span class="tag">.${esc(r.format||'?')}</span><span>${(r.size_kb||0).toFixed(1)} KB</span></div>` +
      '</div>';
    card.addEventListener('click', () => openModal(r));
    grid.appendChild(card);
  });
}

function placeholderSvg(fmt) {
  const isImg = ['jpg','jpeg','png','webp','gif','bmp'].includes((fmt||'').toLowerCase());
  return isImg
    ? '<svg width="22" height="22" viewBox="0 0 22 22" fill="none" stroke="currentColor" stroke-width="1" opacity=".25"><rect x="3" y="3" width="16" height="16" rx="2"/><circle cx="8" cy="8.5" r="2"/><path d="M3 16l4-4 3 3 3-4 6 5"/></svg>'
    : '<svg width="22" height="22" viewBox="0 0 22 22" fill="none" stroke="currentColor" stroke-width="1" opacity=".25"><circle cx="11" cy="11" r="8"/><circle cx="11" cy="11" r="3.5"/><circle cx="11" cy="11" r="1.2" fill="currentColor"/></svg>';
}

// ── Modal ─────────────────────────────────────────────────────
function openModal(r) {
  const pct = Math.round((r.score || 0) * 100);
  const scoreClass = pct >= 70 ? 'color-ok' : pct >= 45 ? 'color-warn' : 'color-muted';
  $('modalBody').innerHTML =
    '<div class="modal__preview">' +
      (r.has_preview ? `<img src="/api/preview/${esc(r.id)}" alt="">` : placeholderSvg(r.format)) +
    '</div>' +
    `<div class="modal__title">${esc(r.file_name)}</div>` +
    '<div class="modal__kv">' +
      `<div class="modal__kv-item"><label>Match</label><span class="${scoreClass}">${pct}%</span></div>` +
      `<div class="modal__kv-item"><label>Format</label><span>.${esc((r.format||'?').toUpperCase())}</span></div>` +
      `<div class="modal__kv-item"><label>Size</label><span>${(r.size_kb||0).toFixed(1)} KB</span></div>` +
    '</div>' +
    '<div><label style="font-size:10px;text-transform:uppercase;letter-spacing:.05em;color:var(--c-muted)">File Path</label></div>' +
    `<div class="modal__path">${esc(r.file_path)}</div>`;
  $('modalBg').classList.add('is-open');
}

$('modalClose').addEventListener('click', closeModal);
$('modalBg').addEventListener('click', e => { if (e.target === $('modalBg')) closeModal(); });
document.addEventListener('keydown', e => { if (e.key === 'Escape') closeModal(); });
function closeModal() { $('modalBg').classList.remove('is-open'); }
