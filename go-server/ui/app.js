/* ================================================================
   EMBFinder — app.js  (Visual Search + EMB Library)
   ================================================================ */

const $ = id => document.getElementById(id);

// ── State ─────────────────────────────────────────────────────────────────────
let queryFile        = null;
let queryObjectURL   = null;   // track for memory-leak-free revocation
let modalObjectURL   = null;   // track modal query object URL
let indexedCount     = 0;
let stateEventSource = null;
let currentTab       = 'search';
let viewerZoom       = 1;
let modalItem        = null;

// Library state
let libPage = 1, libTotal = 0, libPages = 1, libFilter = '', libDebounce = null;

// ── API ───────────────────────────────────────────────────────────────────────
const getApiUrl = path => (window.API_BASE || '') + path;

async function apiGet(url) {
  const r = await fetch(getApiUrl(url));
  if (!r.ok) throw new Error('HTTP ' + r.status);
  return r.json();
}
async function apiPost(url, body, isJSON) {
  const opts = { method: 'POST', body };
  if (isJSON) opts.headers = { 'Content-Type': 'application/json' };
  const r = await fetch(getApiUrl(url), opts);
  if (!r.ok) throw new Error('HTTP ' + r.status);
  return r.json();
}

// ── Tab Switching ─────────────────────────────────────────────────────────────
function switchTab(tab) {
  currentTab = tab;
  $('panelSearch').classList.toggle('hidden', tab !== 'search');
  $('panelLibrary').classList.toggle('hidden', tab !== 'library');
  $('tabSearch').classList.toggle('tab-btn--active', tab === 'search');
  $('tabLibrary').classList.toggle('tab-btn--active', tab === 'library');
  if (tab === 'library') loadLibrary(1, libFilter);
}

// ── SSE ───────────────────────────────────────────────────────────────────────
function startPoll() {
  if (stateEventSource && stateEventSource.readyState !== EventSource.CLOSED) return;
  stateEventSource = new EventSource(getApiUrl('/api/index/state/stream'));

  stateEventSource.onmessage = e => {
    const d = JSON.parse(e.data);
    indexedCount = d.total_indexed || 0;

    $('statusTxt').textContent = 'Online — ' + indexedCount.toLocaleString() + ' designs';
    $('dot').className = 'dot dot--ok';

    if (!queryFile) {
      $('searchBtn').textContent = indexedCount > 0 ? 'Search Library' : 'Scan Library';
      $('searchBtn').disabled = false;
    }

    if (d.counts) renderCounts(d.counts);
    updateSyncButton(d.user_paused);

    const active = d.running || (d.status && d.status !== 'Idle' && d.status !== 'idle');
    if (active) showProgress(d);
    else $('progressWrap').classList.add('hidden');
  };

  stateEventSource.onerror = () => {
    stateEventSource.close();
    stateEventSource = null;
    $('statusTxt').textContent = 'Offline — retrying...';
    $('dot').className = 'dot dot--err';
    setTimeout(startPoll, 3000);
  };
}

function showProgress(state) {
  $('progressWrap').classList.remove('hidden');
  const processed = state.processed || 0;
  const total     = state.scan_done ? (state.total || 0) : (state.total || state.discovered || 0);
  const p = total > 0 ? Math.min(100, (processed / total * 100)).toFixed(1) : '0.0';
  let label = state.status || 'Syncing...';
  if (state.running && !state.scan_done && total === 0) label = 'Scanning drives...';
  $('progLabel').textContent = label;
  $('progFill').style.width  = p + '%';
  $('progCount').textContent = p + '%';
}

// ── Drives ────────────────────────────────────────────────────────────────────
async function loadDrives() {
  try {
    const d = await apiGet('/api/drives');
    renderDrives(d.drives || []);
  } catch { /* silent */ }
}

function renderDrives(drives) {
  const list = $('driveList');
  if (!drives.length) { list.innerHTML = '<div class="txt-sm txt-muted">No drives found</div>'; return; }
  list.innerHTML = drives.map(dr => {
    const canCheck   = dr.usable;
    const count      = dr.indexed || 0;
    const badge      = count > 0 ? `<span class="drive-badge">${count.toLocaleString()}</span>` : '';
    const cls        = canCheck ? 'drive-item' : 'drive-item drive-item--disabled';
    return `<label class="${cls}">
      <input type="checkbox" class="drive-check" data-path="${dr.path}"${dr.selected?' checked':''}${canCheck?'':' disabled'}>
      <span class="drive-label">${dr.label}</span>${badge}
    </label>`;
  }).join('');
  list.querySelectorAll('.drive-check').forEach(cb => cb.addEventListener('change', onDriveToggle));
}

async function onDriveToggle(e) {
  const isChecked = e.target.checked;
  const path = e.target.dataset.path;
  const msg = isChecked ? `Add ${path} and start indexing?` : `Stop indexing and remove data for ${path}?`;
  if (!confirm(msg)) { e.target.checked = !isChecked; return; }
  const checked = Array.from(document.querySelectorAll('.drive-check:checked')).map(c => c.dataset.path);
  try {
    await apiPost('/api/drives/select', JSON.stringify({ paths: checked }), true);
    if (isChecked) apiGet('/api/index/start').then(r => { if (r.status === 'started') { toast('Scan started'); startPoll(); } });
    else toast('Removed from index');
  } catch { toast('Could not update drive selection', 'err'); }
}

// ── Drop Zone ─────────────────────────────────────────────────────────────────
function wireDropZone() {
  const dz = $('dropZone'), input = $('fileInput');
  dz.addEventListener('click', () => input.click());
  input.addEventListener('change', e => { if (e.target.files.length) setQueryFile(e.target.files[0]); });
  dz.addEventListener('dragenter', e => { e.preventDefault(); dz.classList.add('is-dragover'); });
  dz.addEventListener('dragover',  e => { e.preventDefault(); dz.classList.add('is-dragover'); });
  dz.addEventListener('dragleave', () => dz.classList.remove('is-dragover'));
  dz.addEventListener('drop', e => {
    e.preventDefault(); dz.classList.remove('is-dragover');
    if (e.dataTransfer.files.length) setQueryFile(e.dataTransfer.files[0]);
  });
  $('clearFileBtn').addEventListener('click', e => { e.stopPropagation(); clearQueryFile(); });
}

function setQueryFile(f) {
  queryFile = f;
  const ext = f.name.split('.').pop().toLowerCase();
  const isEmb = ext === 'emb';
  $('previewName').textContent = f.name;
  $('previewType').textContent = isEmb ? '.EMB — renders via EmbEngine' : 'Ready to search';
  $('dzEmpty').style.display   = 'none';
  $('dzPreview').classList.add('is-visible');

  // Revoke previous object URL to free memory
  if (queryObjectURL) { URL.revokeObjectURL(queryObjectURL); queryObjectURL = null; }

  if (isEmb) {
    $('previewThumb').src = `data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='64' height='64' viewBox='0 0 24 24' fill='none' stroke='%232563eb' stroke-width='1.5'><path d='M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z'/><polyline points='14 2 14 8 20 8'/><line x1='16' y1='13' x2='8' y2='13'/><line x1='16' y1='17' x2='8' y2='17'/></svg>`;
  } else {
    queryObjectURL = URL.createObjectURL(f);
    $('previewThumb').src = queryObjectURL;
  }

  $('searchBtn').textContent = 'Search Library';
  $('searchBtn').disabled    = false;
  // Auto-search if library is indexed
  if (indexedCount > 0) doSearch();
}

function clearQueryFile() {
  // Revoke object URL to prevent memory leak
  if (queryObjectURL) { URL.revokeObjectURL(queryObjectURL); queryObjectURL = null; }
  if (modalObjectURL) { URL.revokeObjectURL(modalObjectURL); modalObjectURL = null; }
  queryFile = null;
  $('previewThumb').src = '';
  $('dzEmpty').style.display = '';
  $('dzPreview').classList.remove('is-visible');
  $('fileInput').value = '';
  $('searchBtn').textContent = indexedCount > 0 ? 'Search Library' : 'Scan Library';
  // Hide results, show empty state
  $('resultsBar').style.display = 'none';
  $('searchGridWrap').style.display = 'none';
  $('searchEmptyState').style.display = '';
  $('resultsGrid').innerHTML = '';
}

// ── Actions ───────────────────────────────────────────────────────────────────
function wireActions() {
  $('searchBtn').addEventListener('click', () => {
    if (!queryFile) {
      const checked = Array.from(document.querySelectorAll('.drive-check:checked')).map(c => c.dataset.path);
      if (!checked.length) { toast('Select at least one drive to scan', 'err'); return; }
      apiGet('/api/index/start')
        .then(r => {
          if (r.status === 'no_drives')      { toast(r.msg, 'err'); return; }
          if (r.status === 'already_running') { toast('Sync already running'); return; }
          toast('Scan started'); startPoll();
        }).catch(() => toast('Could not start scan', 'err'));
    } else { doSearch(); }
  });

  $('clearBtn').addEventListener('click', async () => {
    if (!confirm('WARNING: This clears the entire local database.\n\nAre you sure?')) return;
    const btn = $('clearBtn');
    btn.disabled = true; btn.textContent = 'Clearing…';
    try {
      await apiPost('/api/clear', '{}', true);
      queryFile = null; indexedCount = 0;
      libPage = 1; libFilter = ''; libTotal = 0;
      $('libSearchInput').value = ''; $('libGrid').innerHTML = '';
      $('libCount').textContent = ''; $('libPagination').innerHTML = '';
      $('progressWrap').classList.add('hidden');
      renderCounts({});
      clearQueryFile();
      toast('Library cleared', 'success');
      loadDrives();
    } catch (e) { toast('Clear failed: ' + e.message, 'err'); }
    finally { btn.disabled = false; btn.textContent = 'Clear Data'; }
  });

  $('refreshBtn').addEventListener('click', () => {
    loadDrives();
    if (currentTab === 'library') loadLibrary(libPage, libFilter);
    if (!stateEventSource || stateEventSource.readyState === EventSource.CLOSED) { stateEventSource = null; startPoll(); }
    toast('Refreshed');
  });

  $('syncToggleBtn').addEventListener('click', async () => {
    const btn = $('syncToggleBtn'); btn.disabled = true;
    try {
      const d = await apiGet('/api/index/toggle');
      updateSyncButton(d.user_paused);
      toast(d.user_paused ? 'Sync paused' : 'Sync resumed');
    } catch { toast('Failed to toggle sync', 'err'); }
    finally { btn.disabled = false; }
  });

  $('modalBg').addEventListener('click', e => { if (e.target === $('modalBg')) closeModal(); });

  $('openFolderBtn').addEventListener('click', async () => {
    if (!modalItem) return;
    try {
      const res = await apiPost('/api/open-file', JSON.stringify({ id: modalItem.id, path: modalItem.file_path }), true);
      if (res.error) toast(res.error, 'err');
      else toast('Opened: ' + res.path);
    } catch { toast('Could not open folder', 'err'); }
  });

  $('useAsQueryBtn').addEventListener('click', async () => {
    if (!modalItem) return;
    closeModal(); switchTab('search');
    try {
      const resp = await fetch(getApiUrl('/api/preview/' + modalItem.id));
      if (!resp.ok) throw new Error();
      const blob = await resp.blob();
      setQueryFile(new File([blob], modalItem.file_name + '.png', { type: 'image/png' }));
    } catch { toast('Could not load preview for search', 'err'); }
  });
}

function updateSyncButton(isPaused) {
  const btn = $('syncToggleBtn'), txt = $('syncToggleText');
  btn.classList.toggle('btn--success', isPaused);
  btn.classList.toggle('btn--outline',  !isPaused);
  txt.textContent = isPaused ? 'Start Sync' : 'Stop Sync';
}

// ── Search ────────────────────────────────────────────────────────────────────
let _searchTimer = null;

async function doSearch() {
  if (!queryFile) return;
  const btn = $('searchBtn'), bar = $('searchingBar');
  const t0 = Date.now();

  // Live seconds counter
  let secs = 0;
  clearInterval(_searchTimer);
  bar.innerHTML = '<div class="dot dot--pulse"></div><span id="searchTimerTxt">Searching… 0s</span>';
  _searchTimer = setInterval(() => {
    secs++;
    const el = document.getElementById('searchTimerTxt');
    if (el) el.textContent = `Searching… ${secs}s`;
  }, 1000);

  try {
    btn.disabled = true;
    bar.classList.add('is-visible');
    $('searchEmptyState').style.display = 'none';
    $('searchGridWrap').style.display   = 'none';
    $('resultsBar').style.display       = 'none';

    const fd = new FormData();
    fd.append('file', queryFile);
    fd.append('top_k', '200');
    const d = await apiPost('/api/search', fd, false);

    if (d.error) { toast(d.error, 'err'); $('searchEmptyState').style.display = ''; return; }

    // Server returns only EMB files with renders — no client-side filtering needed
    const results = d.results || [];
    renderResults(results);
    const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
    $('resultsTitle').textContent = results.length
      ? `${results.length} matching EMB designs`
      : 'No matching designs found';
    $('searchTime').textContent = `${elapsed}s`;
    $('resultsBar').style.display = '';
    $('searchGridWrap').style.display = '';
  } catch (e) {
    toast('Search failed: ' + e.message, 'err');
    $('searchEmptyState').style.display = '';
  } finally {
    clearInterval(_searchTimer);
    btn.disabled = false;
    bar.classList.remove('is-visible');
  }
}


function renderResults(res) {
  const grid = $('resultsGrid');
  grid.innerHTML = '';
  if (!res.length) {
    grid.innerHTML = `<div class="grid-empty">
      <svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" style="opacity:.3;margin-bottom:12px"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.35-4.35"/></svg>
      <div class="txt-bold">No matches found</div>
      <div class="txt-sm txt-muted" style="margin-top:6px">Try a different image or check that your drives are indexed</div>
    </div>`;
    return;
  }
  res.forEach(item => {
    const card = document.createElement('div');
    card.className = 'card';
    const pct = (item.score * 100).toFixed(1);
    const scoreClass = item.score >= 0.6 ? 'score--high' : item.score >= 0.3 ? 'score--mid' : '';
    card.innerHTML = `
      <div class="card__render">
        <img src="${getApiUrl('/api/preview/' + item.id)}"
             onload="this.classList.add('loaded')"
             onerror="this.parentElement.classList.add('no-img')"
             loading="lazy" alt="${item.file_name}">
        <div class="card__no-img">No render</div>
        <div class="card__score-pill ${scoreClass}">${pct}%</div>
      </div>
      <div class="card__body">
        <div class="card__name" title="${item.file_name}">${item.file_name}</div>
        <div class="card__row">
          <span class="tag tag--sm">EMB</span>
          <span class="txt-11 txt-muted">${item.size_kb.toFixed(0)} KB</span>
        </div>
      </div>`;
    card.onclick = () => openModal(item, false);
    grid.appendChild(card);
  });
}

// ── Library Browser ───────────────────────────────────────────────────────────
async function loadLibrary(page, filter) {
  libPage = page; libFilter = filter || '';
  const grid = $('libGrid');
  grid.innerHTML = '<div class="grid-empty txt-muted" style="padding:60px 0">Loading...</div>';
  $('libPagination').innerHTML = '';
  try {
    let url = `/api/browse?page=${page}&page_size=48`;
    if (libFilter) url += '&q=' + encodeURIComponent(libFilter);
    const d = await apiGet(url);
    libTotal = d.total || 0; libPages = d.pages || 1;
    $('libCount').textContent = libTotal.toLocaleString() + ' designs' + (libFilter ? ` matching "${libFilter}"` : '');
    grid.innerHTML = '';
    if (!d.items?.length) {
      grid.innerHTML = `<div class="grid-empty"><div class="txt-bold">No designs found</div><div class="txt-sm txt-muted" style="margin-top:6px">${libFilter ? 'Try a different name' : 'Index your drives first'}</div></div>`;
      return;
    }
    d.items.forEach(item => {
      const card = document.createElement('div');
      card.className = 'card';
      card.innerHTML = `
        <div class="card__render">
          ${item.has_preview
            ? `<img src="${getApiUrl('/api/preview/' + item.id)}" onload="this.classList.add('loaded')" onerror="this.parentElement.classList.add('no-img')" loading="lazy" alt="${item.file_name}">`
            : ''}
          <div class="card__no-img">No render</div>
        </div>
        <div class="card__body">
          <div class="card__name" title="${item.file_name}">${item.file_name}</div>
          <div class="card__row">
            <span class="tag tag--sm">EMB</span>
            <span class="txt-11 txt-muted">${item.size_kb.toFixed(0)} KB</span>
          </div>
        </div>`;
      card.onclick = () => openModal(item, true);
      grid.appendChild(card);
    });
    renderPagination(libPage, libPages);
  } catch (e) {
    grid.innerHTML = `<div class="grid-empty txt-muted">Error: ${e.message}</div>`;
  }
}

function renderPagination(cur, total) {
  if (total <= 1) { $('libPagination').innerHTML = ''; return; }
  let h = cur > 1 ? `<button class="pag-btn" onclick="loadLibrary(${cur-1},libFilter)">← Prev</button>` : '';
  const s = Math.max(1, cur-3), e = Math.min(total, cur+3);
  if (s > 1) h += `<button class="pag-btn" onclick="loadLibrary(1,libFilter)">1</button><span class="pag-ellipsis">…</span>`;
  for (let i = s; i <= e; i++) h += `<button class="pag-btn${i===cur?' pag-btn--active':''}" onclick="loadLibrary(${i},libFilter)">${i}</button>`;
  if (e < total) h += `<span class="pag-ellipsis">…</span><button class="pag-btn" onclick="loadLibrary(${total},libFilter)">${total}</button>`;
  if (cur < total) h += `<button class="pag-btn" onclick="loadLibrary(${cur+1},libFilter)">Next →</button>`;
  $('libPagination').innerHTML = h;
}

// ── Modal (EMB Viewer) ────────────────────────────────────────────────────────
function openModal(item, isLibrary) {
  modalItem = item;
  viewerZoom = 1;

  // Query pane: show only in search mode, revoke previous URL
  const qPane = $('modalQueryPane');
  if (modalObjectURL) { URL.revokeObjectURL(modalObjectURL); modalObjectURL = null; }
  if (!isLibrary && queryFile) {
    qPane.style.display = '';
    modalObjectURL = URL.createObjectURL(queryFile);
    $('modalQueryImg').src = modalObjectURL;
  } else {
    qPane.style.display = 'none';
  }

  // Render pane
  const render = $('modalRender');
  $('embViewer').classList.remove('no-render');
  render.classList.remove('loaded');
  render.src = getApiUrl('/api/preview/' + item.id);
  $('renderLabel').textContent = 'Embroidery Render';

  // Match badge
  const badge = $('matchBadge');
  if (!isLibrary && item.score) {
    badge.textContent = (item.score * 100).toFixed(1) + '% match';
    badge.className = 'match-badge ' + (item.score >= 0.6 ? 'match--high' : item.score >= 0.3 ? 'match--mid' : 'match--low');
    badge.style.display = '';
  } else {
    badge.style.display = 'none';
  }

  // Info panel
  $('modalTitle').textContent   = item.file_name;
  $('modalPath').textContent    = item.file_path;
  $('modalFormat').textContent  = (item.format || 'emb').toUpperCase();
  $('metaStitches').textContent = '—';
  $('metaTrims').textContent    = '—';
  $('metaColors').textContent   = '—';
  $('metaSize').textContent     = item.size_kb ? item.size_kb.toFixed(1) + ' KB' : '—';
  $('modalEngineStatus').textContent = '';
  $('metaLoading').style.display = '';
  $('useAsQueryBtn').style.display = isLibrary ? '' : 'none';

  $('modalBg').classList.add('is-open');

  // Async: fetch EMB metadata
  fetchEmbInfo(item);
}

async function fetchEmbInfo(item) {
  try {
    const info = await apiPost('/api/emb-info',
      JSON.stringify({ id: item.id, path: item.file_path }), true);
    $('metaLoading').style.display = 'none';
    if (info.error && !info.stitch_count) { $('modalEngineStatus').textContent = '⚠ ' + info.error; return; }
    $('metaStitches').textContent = info.stitch_count ? info.stitch_count.toLocaleString() : '—';
    $('metaTrims').textContent    = info.trim_count   != null ? info.trim_count.toLocaleString() : '—';
    $('metaColors').textContent   = info.color_count  != null ? info.color_count.toLocaleString() : '—';
    $('metaSize').textContent     = info.size_kb ? info.size_kb + ' KB' : '—';
    $('modalEngineStatus').textContent = info.engine_ready ? '✓ Wilcom Engine' : '○ Estimates only';
  } catch {
    $('metaLoading').style.display = 'none';
    $('modalEngineStatus').textContent = '⚠ Metadata unavailable';
  }
}

function closeModal() {
  $('modalBg').classList.remove('is-open');
  modalItem = null;
}

// ── Viewer Zoom ───────────────────────────────────────────────────────────────
function zoomViewer(factor) {
  if (factor === 1) { viewerZoom = 1; }
  else { viewerZoom = Math.max(0.3, Math.min(4, viewerZoom * factor)); }
  $('modalRender').style.transform = `scale(${viewerZoom})`;
}

// ── Counts ────────────────────────────────────────────────────────────────────
function renderCounts(counts) {
  const el = $('formatCounts');
  const n  = counts['emb'] || 0;
  if (n === 0) {
    el.innerHTML = `<div class="stat-card stat-card--empty"><div class="section-label">.EMB Designs</div><div class="stat-num">0</div><div class="txt-sm">Library empty</div></div>`;
  } else {
    el.innerHTML = `<div class="stat-card"><div class="section-label">.EMB Designs Found</div><div class="stat-num txt-accent">${n.toLocaleString()}</div><div class="txt-sm txt-muted">Ready for visual search</div></div>`;
  }
}

// ── Toast ─────────────────────────────────────────────────────────────────────
function toast(msg, type = 'info') {
  const t = $('toast');
  t.textContent = msg;
  t.className = 'toast' + (type === 'err' ? ' toast--err' : type === 'success' ? ' toast--success' : '');
  clearTimeout(t._tid);
  t._tid = setTimeout(() => t.classList.add('hidden'), 3500);
}

// ── Boot ──────────────────────────────────────────────────────────────────────
(async function boot() {
  window.addEventListener('dragover', e => e.preventDefault(), false);
  window.addEventListener('drop',     e => e.preventDefault(), false);

  $('libSearchInput').addEventListener('input', e => {
    clearTimeout(libDebounce);
    libDebounce = setTimeout(() => loadLibrary(1, e.target.value), 350);
  });

  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') closeModal();
  });

  wireDropZone();
  wireActions();
  await loadDrives();
  startPoll();
  // Do NOT auto-load designs — grid stays empty until user uploads
})();
