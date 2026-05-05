/* ================================================================
   controllers.js — EMBFinder UI controllers
   ================================================================ */
'use strict';

const $ = id => document.getElementById(id);

// ── Toast ─────────────────────────────────────────────────────────────────────
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

// ── Shared Utilities ──────────────────────────────────────────────────────────
const Utils = {
  // Extract a human-readable folder label from a file path
  folderLabel: (path) => {
    if (!path) return '';
    const parts = path.split('/');
    if (parts.length < 3) return '';
    const p2 = parts[parts.length - 3] || '';
    const p1 = parts[parts.length - 2] || '';
    if (!p1) return '';
    return p2 && p2 !== p1 ? `${p2} › ${p1}` : p1;
  }
};

// ── SyncController ────────────────────────────────────────────────────────────
const SyncController = (() => {
  let _es      = null;
  let _paused  = false;
  let _indexed = 0;

  function start() {
    if (_es && _es.readyState !== EventSource.CLOSED) return;
    _es = new EventSource(API.streamUrl());

    _es.onopen = () => {
      $('dot').className        = 'dot dot--ok';
      $('statusTxt').textContent = `Online`;
    };

    _es.onmessage = e => {
      const d = JSON.parse(e.data);
      _indexed = d.total_indexed || 0;
      _paused  = !!d.user_paused;
      $('dot').className        = 'dot dot--ok';
      $('statusTxt').textContent = `Online`;
      _renderSyncBtn();
      if (d.counts) _renderCounts(d.counts);
      
      const status = (d.status || '').toLowerCase();
      const running = d.running || (status && status !== 'idle' && status !== 'done' && !status.includes('awaiting app window'));
      
      if (running) {
        _showProgress(d);
      } else {
        $('progressWrap').classList.add('hidden');
      }
      
      // Refresh folder list if on that tab or if a scan just finished
      if (typeof FolderController !== 'undefined') FolderController.refresh();
      
      document.dispatchEvent(new CustomEvent('emb:indexed', { detail: { count: _indexed } }));
    };

    _es.onerror = () => {
      _es.close(); _es = null;
      $('dot').className        = 'dot dot--err';
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

  function _renderSyncBtn() {
    const btn = $('syncToggleBtn'), txt = $('syncToggleText');
    btn.classList.toggle('btn--success',  _paused);
    btn.classList.toggle('btn--outline', !_paused);
    txt.textContent = _paused ? 'Start Sync' : 'Stop Sync';
  }

  function _showProgress(d) {
    $('progressWrap').classList.remove('hidden');
    const proc = d.processed || 0;
    const total = d.total || 0;
    const pct = total > 0 ? Math.min(100, (proc / total) * 100).toFixed(1) : '0';
    
    $('progFill').style.width = `${pct}%`;
    $('progCount').textContent = `${proc.toLocaleString()} / ${total.toLocaleString()} files`;
    $('progLabel').textContent = d.status || 'Syncing…';
    
    // Update global stats in header
    if (d.global_indexed !== undefined) $('globalIndexed').textContent = d.global_indexed.toLocaleString();
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

// ── DriveController ───────────────────────────────────────────────────────────
const DriveController = (() => {
  async function reload() {
    try {
      const d = await API.get('/api/drives');
      _render(d.drives || []);
    } catch { /* silent */ }
  }

  function _render(drives) {
    const list = $('driveList');
    if (!drives.length) {
      list.innerHTML = '<div class="txt-sm txt-muted">No drives found</div>';
      return;
    }
    list.innerHTML = drives.map(d => `
      <div class="drive-item">
        <div class="drive-icon">
          <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
            <path d="M22 12A10 10 0 1 1 12 2a10 10 0 0 1 10 10Z"/><path d="M12 12a3 3 0 1 0 0-6 3 3 0 0 0 0 6Z"/>
          </svg>
        </div>
        <div class="drive-info">
          <div class="drive-label" title="${d.path}">${d.label || d.path}</div>
          <div class="drive-path">${d.path}</div>
        </div>
        ${d.indexed ? `<div class="drive-badge">${d.indexed.toLocaleString()}</div>` : ''}
      </div>
    `).join('');
  }


  return { reload };
})();

// ── DropController ────────────────────────────────────────────────────────────
const DropController = (() => {
  let _file   = null;
  let _objUrl = null;

  function wire() {
    const dz    = $('dropZone');
    const input = $('fileInput');
    const overlay = $('globalDropZone');

    dz.addEventListener('click', () => input.click());

    // File input fallback
    input.addEventListener('change', e => { if (e.target.files[0]) setFile(e.target.files[0]); });
    $('clearFileBtn').addEventListener('click', e => { e.stopPropagation(); clearFile(); });

    // --- Global Drag & Drop ---
    let dragCounter = 0;
    window.addEventListener('dragenter', e => {
      e.preventDefault();
      dragCounter++;
      if (overlay) overlay.classList.remove('hidden');
    });
    window.addEventListener('dragover', e => {
      e.preventDefault();
      // Drop effect
      e.dataTransfer.dropEffect = 'copy';
    });
    window.addEventListener('dragleave', () => {
      dragCounter--;
      if (dragCounter <= 0) {
        dragCounter = 0;
        if (overlay) overlay.classList.add('hidden');
      }
    });
    window.addEventListener('drop', e => {
      e.preventDefault();
      dragCounter = 0;
      if (overlay) overlay.classList.add('hidden');
      
      const f = e.dataTransfer.files[0];
      if (f) setFile(f);
    });

    // --- Global Paste (Ctrl+V) ---
    document.addEventListener('paste', e => {
      // Don't intercept if user is typing in a text input
      if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;
      
      const items = (e.clipboardData || e.originalEvent.clipboardData).items;
      for (let index in items) {
        const item = items[index];
        if (item.kind === 'file' && item.type.startsWith('image/')) {
          const blob = item.getAsFile();
          // generate a filename for the pasted image
          const ext = item.type.split('/')[1] || 'png';
          const file = new File([blob], `pasted-image.${ext}`, { type: item.type });
          setFile(file);
          break;
        }
      }
    });
  }

  function setFile(f) {
    _revokeUrl();
    _file = f;
    const isEmb = f.name.split('.').pop().toLowerCase() === 'emb';
    $('previewName').textContent = f.name;
    $('previewType').textContent = isEmb ? '.EMB — find similar designs' : 'Ready to search';
    $('dzEmpty').style.display   = 'none';
    $('dzPreview').classList.add('is-visible');
    if (isEmb) {
      $('previewThumb').src = `data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='64' height='64' viewBox='0 0 24 24' fill='none' stroke='%232563eb' stroke-width='1.5'><path d='M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z'/><polyline points='14 2 14 8 20 8'/></svg>`;
    } else {
      _objUrl = URL.createObjectURL(f);
      $('previewThumb').src = _objUrl;
    }
    $('searchBtn').disabled = false;
    if (SyncController.getCount() > 0) SearchController.run();
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

  function getFile() { return _file; }

  function _revokeUrl() {
    if (_objUrl) { URL.revokeObjectURL(_objUrl); _objUrl = null; }
  }

  return { wire, setFile, clearFile, getFile };
})();

// ── SearchController ──────────────────────────────────────────────────────────
const SearchController = (() => {
  let _timer = null;

  async function run() {
    const f = DropController.getFile();
    if (!f) return;

    const btn = $('searchBtn');
    const bar = $('searchingBar');
    const t0  = Date.now();
    let secs  = 0;

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

      const results = (d.results || []).filter(r => /\.emb$/i.test(r.file_name || ''));
      _renderCards(results);

      const elapsed = ((Date.now() - t0) / 1000).toFixed(1);
      $('resultsTitle').textContent  = results.length
        ? `${results.length} matching EMB design${results.length !== 1 ? 's' : ''}`
        : 'No matching designs found';
      $('searchTime').textContent    = `${elapsed}s`;
      $('resultsBar').style.display  = '';
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
    $('resultsBar').style.display      = 'none';
    $('searchGridWrap').style.display  = 'none';
    $('searchEmptyState').style.display = '';
    $('resultsGrid').innerHTML = '';
  }

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
      const pct    = (item.score * 100).toFixed(1);
      const sCls   = item.score >= 0.6 ? 'score--high' : item.score >= 0.3 ? 'score--mid' : '';
      const sizeKb = item.size_kb ? item.size_kb.toFixed(0) + ' KB' : '';
      const folder = _folderLabel(item.file_path);
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
          ${folder ? `<div class="card__folder txt-11 txt-muted">${folder}</div>` : ''}
          <div class="card__row">
            <span class="tag tag--sm">EMB</span>
            <span class="txt-11 txt-muted">${sizeKb}</span>
            <span class="tag tag--sm tag--score ${sCls}">${pct}%</span>
          </div>
        </div>`;
      card.onclick = () => ModalController.open(item);
      grid.appendChild(card);
    });
  }

  function _showEmpty() { $('searchEmptyState').style.display = ''; }

  function _folderLabel(path) { return Utils.folderLabel(path); }

  return { run, clear };
})();

// ── FolderController ─────────────────────────────────────────────────────────
const FolderController = (() => {
  let _folders = [];

  function init() {
    refresh();
    setInterval(refresh, 5000); // Periodic refresh
  }

  async function refresh() {
    const grid = $('folderGrid');
    if (grid && !grid.children.length) {
      grid.innerHTML = '<div class="loading-placeholder" style="grid-column: 1/-1; padding: 100px 0;"><div class="pulse-circle"></div><div>Discovering embroidery folders…</div></div>';
    }
    try {
      _folders = await API.get('/api/folders') || [];
      _render();
    } catch (e) {
      console.error('Folder refresh failed:', e);
    }
  }

  function _render() {
    // Update Folder Summary
    const totalFolders = _folders.length;
    const indexedFolders = _folders.filter(f => f.status === 'Indexed' || f.indexed_files > 0).length;
    
    const totalEl = $('folderTotalCount');
    const indexedEl = $('folderIndexedCount');
    if (totalEl) totalEl.innerHTML = `<b>${totalFolders}</b> folders discovered`;
    if (indexedEl) indexedEl.innerHTML = `<b>${indexedFolders}</b> indexed`;
    
    const grid = $('folderGrid');
    if (!grid) return;
    
    if (!_folders.length) {
      grid.innerHTML = '<div class="grid-empty"><div class="empty-title">No collections added.</div><div class="empty-sub">Click "Add Folder" to index your embroidery files.</div></div>';
      return;
    }

    grid.innerHTML = '';
    _folders.forEach(f => {
      const card = document.createElement('div');
      card.className = 'folder-card';
      
      const isOutdated = f.needs_rescan || (f.indexed_files < f.total_files && f.status !== 'In Progress');
      const isScanning = f.status === 'In Progress';
      
      let lineCls = 'status-line--ok';
      if (isScanning) lineCls = 'status-line--scanning';
      else if (isOutdated) lineCls = 'status-line--outdated';

      card.innerHTML = `
        <div class="folder-card__body">
          <span class="folder-card__name">${f.name}</span>
          <span class="folder-card__path">${f.path}</span>
          
          <div class="folder-card__stats">
            <div class="folder-card__stat">
              <span class="val">${f.total_files.toLocaleString()}</span>
              <span class="key">Total EMB</span>
            </div>
            <div class="folder-card__stat">
              <span class="val">${f.indexed_files.toLocaleString()}</span>
              <span class="key">Indexed</span>
            </div>
          </div>
        </div>
        
        <div class="folder-card__actions">
          <button class="btn btn--primary btn--sm" onclick="FolderController.rescan('${f.path}')" ${isScanning ? 'disabled' : ''}>
            ${isScanning ? 'Scanning...' : 'Scan Folder'}
          </button>
          <button class="btn btn--outline btn--sm" onclick="FolderController.open('${f.path}')">Explore</button>
        </div>
        
        <div class="status-line ${lineCls}"></div>
      `;
      grid.appendChild(card);
    });
  }

  async function addFolder() {
    const path = prompt("Enter folder path to add:");
    if (!path) return;
    
    Toast.show("Adding folder...");
    try {
      await API.post('/api/folders/rescan', { path }); 
      refresh();
    } catch (e) {
      Toast.show("Failed to add folder", "err");
    }
  }

  async function rescan(path) {
    Toast.show("Rescan queued for " + path);
    await API.post('/api/folders/rescan', { path });
    refresh();
  }

  function open(path) {
    API.post('/api/open-file', { path });
  }

  return { init, refresh, rescan, open, addFolder };
})();

// ── LibraryController ─────────────────────────────────────────────────────────
const LibraryController = (() => {
  let _page = 1, _pages = 1, _filter = '', _debounce = null;

  async function load(page = 1, filter = '') {
    _page = page; _filter = filter;
    const grid = $('libGrid');
    grid.innerHTML = '<div class="loading-placeholder" style="padding: 100px 0;"><div class="pulse-circle"></div><div>Loading library…</div></div>';
    $('libPagination').innerHTML = '';

    try {
      let url = `/api/browse?page=${page}&page_size=48`;
      if (_filter) url += '&q=' + encodeURIComponent(_filter);
      const d = await API.get(url);
      _pages = d.pages || 1;
      $('libCount').textContent = (d.total || 0).toLocaleString() + ' designs'
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
        const folder = Utils.folderLabel(item.file_path);
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
            ${folder ? `<div class="card__folder txt-11 txt-muted">${folder}</div>` : ''}
            <div class="card__row">
              <span class="tag tag--sm">EMB</span>
              <span class="txt-11 txt-muted">${item.size_kb ? item.size_kb.toFixed(0) + ' KB' : ''}</span>
            </div>
          </div>`;
        card.onclick = () => ModalController.open(item);
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
    let h = _page > 1 ? `<button class="pag-btn" onclick="LibraryController.load(${_page - 1}, LibraryController.getFilter())">← Prev</button>` : '';
    const s = Math.max(1, _page - 3), e = Math.min(_pages, _page + 3);
    if (s > 1) h += `<button class="pag-btn" onclick="LibraryController.load(1,LibraryController.getFilter())">1</button><span class="pag-ellipsis">…</span>`;
    for (let i = s; i <= e; i++)
      h += `<button class="pag-btn${i === _page ? ' pag-btn--active' : ''}" onclick="LibraryController.load(${i},LibraryController.getFilter())">${i}</button>`;
    if (e < _pages) h += `<span class="pag-ellipsis">…</span><button class="pag-btn" onclick="LibraryController.load(${_pages},LibraryController.getFilter())">${_pages}</button>`;
    if (_page < _pages) h += `<button class="pag-btn" onclick="LibraryController.load(${_page + 1},LibraryController.getFilter())">Next →</button>`;
    $('libPagination').innerHTML = h;
  }

  function getFilter() { return _filter; }
  return { load, wireSearch, getFilter };
})();

// ── ModalController ───────────────────────────────────────────────────────────
const ModalController = (() => {
  let _item = null;

  function open(item) {
    _item = item;

    // Render pane
    const render = $('modalRender');
    $('embViewer').classList.remove('no-render');
    render.classList.remove('loaded');
    render.src = API.previewUrl(item.id);
    _viewerZoom = 1;
    render.style.transform = '';

    // Match badge
    const badge = $('matchBadge');
    if (item.score) {
      const pct = (item.score * 100).toFixed(1);
      badge.textContent  = pct + '% match';
      badge.className    = 'match-badge ' + (item.score >= 0.6 ? 'match--high' : item.score >= 0.3 ? 'match--mid' : 'match--low');
      badge.style.display = '';
    } else {
      badge.style.display = 'none';
    }

    // Info
    $('modalTitle').textContent  = item.file_name;
    $('modalPath').textContent   = item.file_path || '';
    // Folder breadcrumb
    const parts = (item.file_path || '').split('/');
    const p2 = parts[parts.length - 3] || '';
    const p1 = parts[parts.length - 2] || '';
    const folderEl = $('modalFolder');
    if (folderEl) folderEl.textContent = (p2 && p2 !== p1) ? `${p2} › ${p1}` : p1;

    // Reset button states
    _resetBtn('openTruesizerBtn', 'Open in TrueSizer');
    _resetBtn('openFolderBtn',    'Open Folder');

    // Stitch info has been removed to rely exclusively on TrueSizer engine

    $('modalBg').classList.add('is-open');
  }

  // Reset stitch panel to loading spinner
  function _resetStitchInfo() {
    const loading = document.getElementById('stitchLoading');
    const grid    = document.getElementById('stitchGrid');
    const src     = document.getElementById('stitchSrc');
    if (loading) loading.classList.remove('hidden');
    if (grid)    grid.classList.add('hidden');
    if (src)     src.classList.add('hidden');
    ['infoStitches','infoTrims','infoColors','infoSize'].forEach(id => {
      const el = document.getElementById(id);
      if (el) el.textContent = '—';
    });
  }

  // Fetch real stitch info from emb-engine
  function _fetchStitchInfo(item) {
    const payload = item.id ? { id: item.id } : { path: item.file_path };
    API.embInfo(payload)
      .then(d => {
        const loading = document.getElementById('stitchLoading');
        const grid    = document.getElementById('stitchGrid');
        const srcEl   = document.getElementById('stitchSrc');
        if (loading) loading.classList.add('hidden');
        if (grid)    grid.classList.remove('hidden');

        const fmt = n => (n == null || n === undefined) ? '—' : Number(n).toLocaleString();
        const s = document.getElementById('infoStitches'); if (s) s.textContent = fmt(d.stitch_count);
        const t = document.getElementById('infoTrims');    if (t) t.textContent = fmt(d.trim_count);
        const c = document.getElementById('infoColors');   if (c) c.textContent = fmt(d.color_count);
        const z = document.getElementById('infoSize');     if (z) z.textContent = d.size_kb ? d.size_kb + ' KB' : '—';

        if (srcEl) {
          const label = d.source === 'pyembroidery' ? '✓ Accurate — read by pyembroidery'
                      : d.source === 'ole2_header'  ? '⚠ Estimated — OLE2 header'
                      : '— Source unknown';
          srcEl.textContent = label;
          srcEl.className   = 'stitch-info__src ' + (d.source === 'pyembroidery' ? 'src--ok' : 'src--warn');
          srcEl.classList.remove('hidden');
        }
      })
      .catch(() => {
        const loading = document.getElementById('stitchLoading');
        const grid    = document.getElementById('stitchGrid');
        if (loading) loading.classList.add('hidden');
        if (grid)    grid.classList.remove('hidden');
      });
  }

  function close() {
    $('modalBg').classList.remove('is-open');
    _item = null;
  }

  // Zoom
  let _viewerZoom = 1;
  function zoom(factor) {
    _viewerZoom = factor === 1 ? 1 : Math.max(0.3, Math.min(4, _viewerZoom * factor));
    $('modalRender').style.transform = `scale(${_viewerZoom})`;
  }

  // Open in TrueSizer — shows spinner until response
  async function openInTrueSizer() {
    if (!_item?.file_path) return;
    const btn = $('openTruesizerBtn');
    _setBusy(btn, 'Opening…');
    try {
      const res = await API.postJSON('/api/open-truesizer', { path: _item.file_path });
      if (res.error) Toast.show('TrueSizer: ' + res.error, 'err');
      else           Toast.show(`Opened in TrueSizer ✓`, 'success');
    } catch (e) { Toast.show('Could not open TrueSizer: ' + e.message, 'err'); }
    finally    { _resetBtn(btn, 'Open in TrueSizer'); }
  }

  // Open Folder — shows spinner until file manager opens
  async function openFolder() {
    if (!_item) return;
    const btn = $('openFolderBtn');
    _setBusy(btn, 'Opening…');
    try {
      const res = await API.postJSON('/api/open-file', { id: _item.id, path: _item.file_path });
      if (res.error) Toast.show(res.error, 'err');
      else           Toast.show('Folder opened', 'success');
    } catch { Toast.show('Could not open folder', 'err'); }
    finally  { _resetBtn(btn, 'Open Folder'); }
  }

  // Copy path
  async function copyPath() {
    if (!_item?.file_path) return;
    try {
      await navigator.clipboard.writeText(_item.file_path);
      Toast.show('Path copied', 'success');
    } catch {
      const inp = document.createElement('input');
      inp.value = _item.file_path;
      document.body.appendChild(inp);
      inp.select();
      document.execCommand('copy');
      document.body.removeChild(inp);
      Toast.show('Path copied', 'success');
    }
  }

  // Helpers
  function _setBusy(btn, label) {
    if (typeof btn === 'string') btn = $(btn);
    if (!btn) return;
    btn.disabled = true;
    btn.innerHTML = `<span class="btn-spinner"></span>${label}`;
  }

  function _resetBtn(btn, label) {
    if (typeof btn === 'string') btn = $(btn);
    if (!btn) return;
    btn.disabled = false;
    btn.textContent = label;
  }

  return { open, close, zoom, openFolder, copyPath, openInTrueSizer };
})();

// ── TabController ─────────────────────────────────────────────────────────────
const TabController = (() => {
  function switchTo(tab) {
    $('panelSearch').classList.toggle('hidden',  tab !== 'search');
    $('panelLibrary').classList.toggle('hidden', tab !== 'library');
    $('panelFolders').classList.toggle('hidden', tab !== 'folders');
    
    $('tabSearch').classList.toggle('tab-btn--active',  tab === 'search');
    $('tabLibrary').classList.toggle('tab-btn--active', tab === 'library');
    $('tabFolders').classList.toggle('tab-btn--active', tab === 'folders');
    
    if (tab === 'library') LibraryController.load(1, LibraryController.getFilter());
    if (tab === 'folders') FolderController.refresh();
  }
  function current() {
    if (!$('panelSearch').classList.contains('hidden')) return 'search';
    if (!$('panelLibrary').classList.contains('hidden')) return 'library';
    if (!$('panelFolders').classList.contains('hidden')) return 'folders';
    return '';
  }
  return { switchTo, current };
})();
