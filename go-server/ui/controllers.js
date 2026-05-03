/* ================================================================
   controllers.js — EMBFinder UI controllers
   One controller per concern.  app.js just boots them.
   ================================================================ */
'use strict';

// ─────────────────────────────────────────────────────────────────────────────
// Shared helpers
// ─────────────────────────────────────────────────────────────────────────────
const $  = id => document.getElementById(id);
const qs = sel => document.querySelector(sel);

// Toast notification
const Toast = (() => {
  let _tid;
  function show(msg, type = 'info') {
    const el = $('toast');
    el.textContent = msg;
    el.className = 'toast' + (type === 'err' ? ' toast--err' : type === 'success' ? ' toast--success' : '');
    clearTimeout(_tid);
    el.classList.remove('hidden');
    _tid = setTimeout(() => el.classList.add('hidden'), 3500);
  }
  return { show };
})();


// ─────────────────────────────────────────────────────────────────────────────
// SyncController  — SSE stream + Stop/Start Sync button
// ─────────────────────────────────────────────────────────────────────────────
const SyncController = (() => {
  let _es       = null;
  let _paused   = false;
  let _indexed  = 0;

  function start() {
    if (_es && _es.readyState !== EventSource.CLOSED) return;
    _es = new EventSource(API.streamUrl());

    _es.onopen = () => {
      $('dot').className       = 'dot dot--ok';
      $('statusTxt').textContent = `Online — ${_indexed.toLocaleString()} designs`;
    };

    _es.onmessage = e => {
      const d = JSON.parse(e.data);
      _indexed = d.total_indexed || 0;
      _paused  = !!d.user_paused;

      // Status dot
      $('dot').className       = 'dot dot--ok';
      $('statusTxt').textContent = `Online — ${_indexed.toLocaleString()} designs`;

      // Sync button label
      _renderSyncBtn();

      // Format counts sidebar
      if (d.counts) _renderCounts(d.counts);

      // Progress bar
      const running = d.running || (d.status && d.status !== 'Idle' && d.status !== 'idle');
      running ? _showProgress(d) : $('progressWrap').classList.add('hidden');

      // Notify search that the count changed
      document.dispatchEvent(new CustomEvent('emb:indexed', { detail: { count: _indexed } }));
    };

    _es.onerror = () => {
      _es.close(); _es = null;
      $('dot').className       = 'dot dot--err';
      $('statusTxt').textContent = 'Offline — retrying…';
      setTimeout(start, 3000);
    };
  }

  async function toggle() {
    const btn = $('syncToggleBtn');
    btn.disabled = true;
    try {
      const d = await API.get('/api/index/toggle');
      _paused = !!d.user_paused;
      _renderSyncBtn();
      Toast.show(_paused ? 'Sync paused' : 'Sync resumed');
    } catch { Toast.show('Failed to toggle sync', 'err'); }
    finally   { btn.disabled = false; }
  }

  async function clearAll() {
    if (!confirm('WARNING: This clears the entire local index.\n\nAre you sure?')) return;
    const btn = $('clearBtn');
    btn.disabled = true; btn.textContent = 'Clearing…';
    try {
      await API.delete('/api/clear');
      _indexed = 0;
      _renderCounts({});
      $('progressWrap').classList.add('hidden');
      $('dot').className        = 'dot';
      $('statusTxt').textContent = 'Cleared — 0 designs';
      document.dispatchEvent(new CustomEvent('emb:cleared'));
      Toast.show('Library cleared', 'success');
      DriveController.reload();
    } catch (e) { Toast.show('Clear failed: ' + e.message, 'err'); }
    finally     { btn.disabled = false; btn.textContent = 'Clear Data'; }
  }

  function getCount() { return _indexed; }

  // ── private ──────────────────────────────────────────────────────────────
  function _renderSyncBtn() {
    const btn = $('syncToggleBtn'), txt = $('syncToggleText');
    btn.classList.toggle('btn--success',  _paused);
    btn.classList.toggle('btn--outline', !_paused);
    txt.textContent = _paused ? 'Start Sync' : 'Stop Sync';
  }

  function _showProgress(d) {
    $('progressWrap').classList.remove('hidden');
    const proc  = d.processed || 0;
    const total = d.total || d.discovered || 0;
    const pct   = total > 0 ? Math.min(100, proc / total * 100).toFixed(1) : '0.0';
    $('progLabel').textContent = d.status || 'Syncing…';
    $('progFill').style.width  = pct + '%';
    $('progCount').textContent = pct + '%';
  }

  function _renderCounts(counts) {
    const n  = counts['emb'] || 0;
    const el = $('formatCounts');
    el.innerHTML = n === 0
      ? `<div class="stat-card stat-card--empty">
           <div class="section-label">EMB Designs</div>
           <div class="stat-num">0</div>
           <div class="txt-sm txt-muted">Library empty</div>
         </div>`
      : `<div class="stat-card">
           <div class="section-label">EMB Designs Indexed</div>
           <div class="stat-num txt-accent">${n.toLocaleString()}</div>
           <div class="txt-sm txt-muted">Ready for visual search</div>
         </div>`;
  }

  return { start, toggle, clearAll, getCount };
})();


// ─────────────────────────────────────────────────────────────────────────────
// DriveController  — load & toggle drive checkboxes
// In development mode the tests/ directory drive entry is surfaced;
// in production mode it is hidden automatically by the server (Go config.go
// MODE=production strips test paths), but we also guard here client-side.
// ─────────────────────────────────────────────────────────────────────────────
const DriveController = (() => {
  // Paths that should never appear in the drive list
  const EXCLUDED_PATTERNS = [/\/tests\//i, /\\tests\\/i, /test_data/i, /\/test$/i, /\\test$/i];

  function _isExcludedPath(path) {
    return EXCLUDED_PATTERNS.some(r => r.test(path));
  }

  async function reload() {
    try {
      const d = await API.get('/api/drives');
      _render(d.drives || []);
    } catch { /* silent on network error */ }
  }

  function _render(drives) {
    const list = $('driveList');
    if (!drives.length) {
      list.innerHTML = '<div class="txt-sm txt-muted">No drives found</div>';
      return;
    }

    list.innerHTML = drives
      .filter(dr => !_isExcludedPath(dr.path))
      .map(dr => {
        const ok    = dr.usable;
        const count = dr.indexed || 0;
        const badge = count > 0
          ? `<span class="drive-badge">${count.toLocaleString()}</span>`
          : '';
        const cls   = ok ? 'drive-item' : 'drive-item drive-item--disabled';
        const label = dr.label || dr.path;
        return `<label class="${cls}">
          <input type="checkbox" class="drive-check" data-path="${dr.path}"
                 ${dr.selected ? 'checked' : ''} ${ok ? '' : 'disabled'}>
          <span class="drive-label" title="${dr.path}">${label}</span>
          ${badge}
        </label>`;
      }).join('');

    list.querySelectorAll('.drive-check').forEach(cb =>
      cb.addEventListener('change', _onToggle)
    );
  }

  async function _onToggle(e) {
    const checked = e.target.checked;
    const path    = e.target.dataset.path;
    const msg     = checked
      ? `Add "${path}" and start indexing?`
      : `Remove "${path}" from the index?`;
    if (!confirm(msg)) { e.target.checked = !checked; return; }

    const selected = [...document.querySelectorAll('.drive-check:checked')]
      .map(c => c.dataset.path);
    try {
      await API.postJSON('/api/drives/select', { paths: selected });
      if (checked) {
        const r = await API.get('/api/index/start');
        if (r.status === 'started') { Toast.show('Scan started'); SyncController.start(); }
      } else {
        Toast.show('Removed from index');
      }
    } catch { Toast.show('Could not update drive selection', 'err'); }
  }

  return { reload };
})();


// ─────────────────────────────────────────────────────────────────────────────
// DropController  — drag-and-drop / file picker
// ─────────────────────────────────────────────────────────────────────────────
const DropController = (() => {
  let _file    = null;
  let _objUrl  = null;   // revoked on every new file to prevent memory leak

  function wire() {
    const dz    = $('dropZone');
    const input = $('fileInput');

    dz.addEventListener('click',     () => input.click());
    dz.addEventListener('dragenter', e => { e.preventDefault(); dz.classList.add('is-dragover'); });
    dz.addEventListener('dragover',  e => { e.preventDefault(); dz.classList.add('is-dragover'); });
    dz.addEventListener('dragleave', () => dz.classList.remove('is-dragover'));
    dz.addEventListener('drop', e => {
      e.preventDefault();
      dz.classList.remove('is-dragover');
      const f = e.dataTransfer.files[0];
      if (f) setFile(f);
    });
    input.addEventListener('change', e => { if (e.target.files[0]) setFile(e.target.files[0]); });
    $('clearFileBtn').addEventListener('click', e => { e.stopPropagation(); clearFile(); });
  }

  function setFile(f) {
    _revokeUrl();
    _file = f;

    const ext   = f.name.split('.').pop().toLowerCase();
    const isEmb = ext === 'emb';

    $('previewName').textContent = f.name;
    $('previewType').textContent = isEmb ? '.EMB — render preview via EmbEngine' : 'Ready to search';
    $('dzEmpty').style.display   = 'none';
    $('dzPreview').classList.add('is-visible');

    if (isEmb) {
      // SVG placeholder for EMB files (no bitmap thumbnail)
      $('previewThumb').src = `data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg'
        width='64' height='64' viewBox='0 0 24 24' fill='none'
        stroke='%232563eb' stroke-width='1.5'>
        <path d='M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z'/>
        <polyline points='14 2 14 8 20 8'/><line x1='16' y1='13' x2='8' y2='13'/>
        <line x1='16' y1='17' x2='8' y2='17'/></svg>`;
    } else {
      _objUrl = URL.createObjectURL(f);
      $('previewThumb').src = _objUrl;
    }

    $('searchBtn').disabled = false;

    // Auto-search if library is ready
    if (SyncController.getCount() > 0) {
      SearchController.run();
    }
  }

  function clearFile() {
    _revokeUrl();
    _file = null;
    $('previewThumb').src = '';
    $('dzEmpty').style.display = '';
    $('dzPreview').classList.remove('is-visible');
    $('fileInput').value = '';
    $('searchBtn').disabled = true;
    SearchController.clear();
  }

  function getFile()  { return _file; }
  function getObjUrl() { return _objUrl; }

  function _revokeUrl() {
    if (_objUrl) { URL.revokeObjectURL(_objUrl); _objUrl = null; }
  }

  return { wire, setFile, clearFile, getFile, getObjUrl };
})();


// ─────────────────────────────────────────────────────────────────────────────
// SearchController  — run search, render result cards (EMB only)
// ─────────────────────────────────────────────────────────────────────────────
const SearchController = (() => {
  let _timer = null;

  async function run() {
    const f = DropController.getFile();
    if (!f) return;

    const btn = $('searchBtn');
    const bar = $('searchingBar');
    const t0  = Date.now();

    // Live timer in searching bar
    let secs = 0;
    clearInterval(_timer);
    bar.innerHTML = `<div class="dot dot--pulse"></div><span id="searchTimerTxt">Searching… 0s</span>`;
    _timer = setInterval(() => {
      const el = $('searchTimerTxt');
      if (el) el.textContent = `Searching… ${++secs}s`;
    }, 1000);

    btn.disabled = true;
    bar.classList.add('is-visible');
    $('searchEmptyState').style.display = 'none';
    $('searchGridWrap').style.display   = 'none';
    $('resultsBar').style.display       = 'none';

    try {
      const fd = new FormData();
      fd.append('file',  f);
      fd.append('top_k', '200');
      const d = await API.post('/api/search', fd);
      if (d.error) { Toast.show(d.error, 'err'); _showEmpty(); return; }

      // Results from the server are already EMB-only — no client filtering needed
      const results = (d.results || []).filter(r =>
        /\.emb$/i.test(r.file_name || '')
      );

      _renderCards(results);
      const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
      $('resultsTitle').textContent = results.length
        ? `${results.length} matching EMB design${results.length !== 1 ? 's' : ''}`
        : 'No matching designs found';
      $('searchTime').textContent   = `${elapsed}s`;
      $('resultsBar').style.display = '';
      $('searchGridWrap').style.display = '';
    } catch (e) {
      Toast.show('Search failed: ' + e.message, 'err');
      _showEmpty();
    } finally {
      clearInterval(_timer);
      btn.disabled = false;
      bar.classList.remove('is-visible');
    }
  }

  function clear() {
    $('resultsBar').style.display     = 'none';
    $('searchGridWrap').style.display = 'none';
    $('searchEmptyState').style.display = '';
    $('resultsGrid').innerHTML = '';
  }

  // ── private ──────────────────────────────────────────────────────────────
  function _renderCards(results) {
    const grid = $('resultsGrid');
    grid.innerHTML = '';
    if (!results.length) {
      grid.innerHTML = `<div class="grid-empty">
        <svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1" style="opacity:.3;margin-bottom:12px"><circle cx="11" cy="11" r="8"/><path d="m21 21-4.35-4.35"/></svg>
        <div class="txt-bold">No EMB matches found</div>
        <div class="txt-sm txt-muted" style="margin-top:6px">Try a different image or index your drives first</div>
      </div>`;
      return;
    }

    results.forEach(item => {
      const card  = document.createElement('div');
      card.className = 'card';
      const pct   = (item.score * 100).toFixed(1);
      const sCls  = item.score >= 0.6 ? 'score--high' : item.score >= 0.3 ? 'score--mid' : '';
      const sizeKb = item.size_kb ? item.size_kb.toFixed(0) + ' KB' : '';
      card.innerHTML = `
        <div class="card__render">
          <img src="${API.previewUrl(item.id)}"
               onload="this.classList.add('loaded')"
               onerror="this.parentElement.classList.add('no-img')"
               loading="lazy" alt="${item.file_name}">
          <div class="card__no-img">No render</div>
          <div class="card__score-pill ${sCls}">${pct}%</div>
        </div>
        <div class="card__body">
          <div class="card__name" title="${item.file_name}">${item.file_name}</div>
          <div class="card__row">
            <span class="tag tag--sm">EMB</span>
            <span class="txt-11 txt-muted">${sizeKb}</span>
          </div>
        </div>`;
      card.onclick = () => ModalController.open(item, false);
      grid.appendChild(card);
    });
  }

  function _showEmpty() { $('searchEmptyState').style.display = ''; }

  return { run, clear };
})();


// ─────────────────────────────────────────────────────────────────────────────
// LibraryController  — paginated EMB browser
// ─────────────────────────────────────────────────────────────────────────────
const LibraryController = (() => {
  let _page = 1, _total = 0, _pages = 1, _filter = '', _debounce = null;

  async function load(page = 1, filter = '') {
    _page = page; _filter = filter;
    const grid = $('libGrid');
    grid.innerHTML = '<div class="grid-empty txt-muted" style="padding:60px 0">Loading…</div>';
    $('libPagination').innerHTML = '';

    try {
      let url = `/api/browse?page=${page}&page_size=48`;
      if (_filter) url += '&q=' + encodeURIComponent(_filter);
      const d = await API.get(url);
      _total = d.total || 0; _pages = d.pages || 1;
      $('libCount').textContent = _total.toLocaleString() + ' designs'
        + (_filter ? ` matching "${_filter}"` : '');
      grid.innerHTML = '';

      if (!d.items?.length) {
        grid.innerHTML = `<div class="grid-empty">
          <div class="txt-bold">No designs found</div>
          <div class="txt-sm txt-muted" style="margin-top:6px">
            ${_filter ? 'Try a different name' : 'Index your drives first'}
          </div>
        </div>`;
        return;
      }

      d.items.forEach(item => {
        const card = document.createElement('div');
        card.className = 'card';
        card.innerHTML = `
          <div class="card__render">
            ${item.has_preview
              ? `<img src="${API.previewUrl(item.id)}" onload="this.classList.add('loaded')"
                      onerror="this.parentElement.classList.add('no-img')"
                      loading="lazy" alt="${item.file_name}">`
              : ''}
            <div class="card__no-img">No render</div>
          </div>
          <div class="card__body">
            <div class="card__name" title="${item.file_name}">${item.file_name}</div>
            <div class="card__row">
              <span class="tag tag--sm">EMB</span>
              <span class="txt-11 txt-muted">${item.size_kb ? item.size_kb.toFixed(0) + ' KB' : ''}</span>
            </div>
          </div>`;
        card.onclick = () => ModalController.open(item, true);
        grid.appendChild(card);
      });

      _renderPagination();
    } catch (e) {
      grid.innerHTML = `<div class="grid-empty txt-muted">Error: ${e.message}</div>`;
    }
  }

  function wireSearch() {
    $('libSearchInput').addEventListener('input', e => {
      clearTimeout(_debounce);
      _debounce = setTimeout(() => load(1, e.target.value.trim()), 350);
    });
  }

  function _renderPagination() {
    if (_pages <= 1) { $('libPagination').innerHTML = ''; return; }
    let h = _page > 1
      ? `<button class="pag-btn" onclick="LibraryController.load(${_page - 1}, LibraryController.getFilter())">← Prev</button>` : '';
    const s = Math.max(1, _page - 3), e = Math.min(_pages, _page + 3);
    if (s > 1) h += `<button class="pag-btn" onclick="LibraryController.load(1,LibraryController.getFilter())">1</button><span class="pag-ellipsis">…</span>`;
    for (let i = s; i <= e; i++)
      h += `<button class="pag-btn${i === _page ? ' pag-btn--active' : ''}"
               onclick="LibraryController.load(${i},LibraryController.getFilter())">${i}</button>`;
    if (e < _pages) h += `<span class="pag-ellipsis">…</span><button class="pag-btn" onclick="LibraryController.load(${_pages},LibraryController.getFilter())">${_pages}</button>`;
    if (_page < _pages) h += `<button class="pag-btn" onclick="LibraryController.load(${_page + 1},LibraryController.getFilter())">Next →</button>`;
    $('libPagination').innerHTML = h;
  }

  function getFilter() { return _filter; }

  return { load, wireSearch, getFilter };
})();


// ─────────────────────────────────────────────────────────────────────────────
// ModalController  — EMB detail viewer
// Opens with: Open Folder button + Copy Path button only.
// "Use as Query" removed — user uploads their own image to search.
// ─────────────────────────────────────────────────────────────────────────────
const ModalController = (() => {
  let _item       = null;
  let _queryObjUrl = null;   // revoked when modal closes

  function open(item, isLibrary) {
    _item = item;
    _revokeQueryUrl();

    // ── Query pane (only in search mode) ───────────────────────────────────
    const qPane = $('modalQueryPane');
    if (!isLibrary && DropController.getFile()) {
      _queryObjUrl = URL.createObjectURL(DropController.getFile());
      $('modalQueryImg').src = _queryObjUrl;
      qPane.style.display = '';
    } else {
      qPane.style.display = 'none';
    }

    // ── Render pane ────────────────────────────────────────────────────────
    const render = $('modalRender');
    $('embViewer').classList.remove('no-render');
    render.classList.remove('loaded');
    render.src = API.previewUrl(item.id);
    _viewerZoom = 1;
    render.style.transform = '';

    // ── Match badge ────────────────────────────────────────────────────────
    const badge = $('matchBadge');
    if (!isLibrary && item.score) {
      const pct = (item.score * 100).toFixed(1);
      badge.textContent = pct + '% match';
      badge.className   = 'match-badge '
        + (item.score >= 0.6 ? 'match--high' : item.score >= 0.3 ? 'match--mid' : 'match--low');
      badge.style.display = '';
    } else {
      badge.style.display = 'none';
    }

    // ── Info panel ─────────────────────────────────────────────────────────
    $('modalTitle').textContent   = item.file_name;
    $('modalPath').textContent    = item.file_path || '';
    $('modalFormat').textContent  = (item.format || 'emb').toUpperCase();
    $('metaStitches').textContent = '—';
    $('metaTrims').textContent    = '—';
    $('metaColors').textContent   = '—';
    $('metaSize').textContent     = item.size_kb ? item.size_kb.toFixed(1) + ' KB' : '—';
    $('modalEngineStatus').textContent = '';
    $('metaLoading').style.display = '';

    $('modalBg').classList.add('is-open');
    _fetchInfo(item);
  }

  function close() {
    $('modalBg').classList.remove('is-open');
    _revokeQueryUrl();
    _item = null;
  }

  // ── Viewer zoom ───────────────────────────────────────────────────────────
  let _viewerZoom = 1;
  function zoom(factor) {
    _viewerZoom = factor === 1 ? 1 : Math.max(0.3, Math.min(4, _viewerZoom * factor));
    $('modalRender').style.transform = `scale(${_viewerZoom})`;
  }

  // ── Open folder in OS file manager ───────────────────────────────────────
  async function openFolder() {
    if (!_item) return;
    try {
      const res = await API.postJSON('/api/open-file', { id: _item.id, path: _item.file_path });
      if (res.error) Toast.show(res.error, 'err');
      else           Toast.show('Opened folder');
    } catch { Toast.show('Could not open folder', 'err'); }
  }

  // ── Copy path to clipboard ────────────────────────────────────────────────
  async function copyPath() {
    if (!_item?.file_path) return;
    try {
      await navigator.clipboard.writeText(_item.file_path);
      Toast.show('Path copied', 'success');
    } catch {
      // Fallback for non-https or older browsers
      const inp = document.createElement('input');
      inp.value = _item.file_path;
      document.body.appendChild(inp);
      inp.select();
      document.execCommand('copy');
      document.body.removeChild(inp);
      Toast.show('Path copied', 'success');
    }
  }

  // ── private ──────────────────────────────────────────────────────────────
  async function _fetchInfo(item) {
    try {
      const info = await API.postJSON('/api/emb-info', { id: item.id, path: item.file_path });
      $('metaLoading').style.display = 'none';
      if (info.error && !info.stitch_count) {
        $('modalEngineStatus').textContent = '⚠ ' + info.error; return;
      }
      $('metaStitches').textContent = info.stitch_count != null ? info.stitch_count.toLocaleString() : '—';
      $('metaTrims').textContent    = info.trim_count   != null ? info.trim_count.toLocaleString()   : '—';
      $('metaColors').textContent   = info.color_count  != null ? info.color_count.toLocaleString()  : '—';
      $('metaSize').textContent     = info.size_kb       ? info.size_kb + ' KB'                      : '—';
      $('modalEngineStatus').textContent = info.engine_ready ? '✓ Engine connected' : '○ Estimates only';
    } catch {
      $('metaLoading').style.display = 'none';
      $('modalEngineStatus').textContent = '⚠ Metadata unavailable';
    }
  }

  function _revokeQueryUrl() {
    if (_queryObjUrl) { URL.revokeObjectURL(_queryObjUrl); _queryObjUrl = null; }
  }

  return { open, close, zoom, openFolder, copyPath };
})();


// ─────────────────────────────────────────────────────────────────────────────
// TabController  — switch between Visual Search and EMB Library tabs
// ─────────────────────────────────────────────────────────────────────────────
const TabController = (() => {
  let _current = 'search';

  function switchTo(tab) {
    _current = tab;
    $('panelSearch').classList.toggle('hidden', tab !== 'search');
    $('panelLibrary').classList.toggle('hidden', tab !== 'library');
    $('tabSearch').classList.toggle('tab-btn--active',  tab === 'search');
    $('tabLibrary').classList.toggle('tab-btn--active', tab === 'library');
    if (tab === 'library') LibraryController.load(1, LibraryController.getFilter());
  }

  function current() { return _current; }
  return { switchTo, current };
})();
