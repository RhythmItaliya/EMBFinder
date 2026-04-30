const $ = id => document.getElementById(id);
let queryFile    = null;
let indexedCount = 0;
let stateEventSource = null;

// Tracks which drive paths the user has checked (null = all selected by default)
let selectedDrives = null; // null means "all"

// ── API Helpers ───────────────────────────────────────────────
function getApiUrl(path) {
  const base = window.API_BASE || '';
  return base + path;
}

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

// ── Status & SSE Stream ───────────────────────────────────────

function startPoll() {
  if (stateEventSource) return;
  stateEventSource = new EventSource(getApiUrl('/api/index/state/stream'));
  
  stateEventSource.onmessage = (e) => {
    const d = JSON.parse(e.data);
    indexedCount = d.total_indexed || 0;

    $('statusTxt').textContent = 'Online — ' + indexedCount.toLocaleString() + ' designs';
    $('dot').className = 'dot dot--ok';

    $('resultsTitle').textContent = indexedCount > 0
      ? indexedCount.toLocaleString() + ' designs indexed — upload an image to search'
      : 'No designs indexed — select drives and click "Scan Library"';

    // Update button label based on context
    if (!queryFile) {
      $('searchBtn').textContent = indexedCount > 0 ? 'Search Library' : 'Scan Library';
      $('searchBtn').disabled = false;
    }

    if (d.counts) renderCounts(d.counts);
    updateSyncButton(d.user_paused);

    const active = d.running || (d.status && d.status !== 'Idle' && d.status !== 'idle');
    if (active) {
      showProgress(d);
    } else {
      $('progressWrap').classList.add('hidden');
    }
  };

  stateEventSource.onerror = () => {
    stateEventSource.close();
    stateEventSource = null;
    const txt = $('statusTxt').textContent;
    if (txt === 'Connecting...' || txt.startsWith('Offline')) {
      $('statusTxt').textContent = 'Offline — retrying...';
      $('dot').className = 'dot dot--err';
    }
    setTimeout(startPoll, 2000);
  };
}

function stopPoll() {
  // SSE stays connected to maintain heartbeat, no stop needed.
}

function showProgress(state) {
  $('progressWrap').classList.remove('hidden');
  const processed = state.processed || 0;
  const total     = state.scan_done ? (state.total || 0) : (state.total || state.discovered || 0);
  const p = total > 0 ? Math.min(100, (processed / total * 100)).toFixed(2) : '0.00';

  let label = state.status || 'Syncing Library...';
  if (state.running && !state.scan_done && total === 0) {
    label = 'Scanning drives...';
  } else if (state.running && !state.scan_done) {
    label = 'Scanning ' + total.toLocaleString();
  }

  $('progLabel').textContent = label;
  $('progFill').style.width  = p + '%';
  $('progCount').textContent = p + '% (' + processed.toLocaleString() + ' / ' + total.toLocaleString() + ')';
}

// ── Drive checkboxes ──────────────────────────────────────────
async function loadDrives() {
  try {
    const d = await apiGet('/api/drives');
    renderDrives(d.drives || []);
  } catch { /* silent */ }
}

function renderDrives(drives) {
  const list = $('driveList');
  if (!drives.length) {
    list.innerHTML = '<div class="txt-sm txt-muted">No drives found</div>';
    return;
  }

  list.innerHTML = drives.map(dr => {
    const canCheck = dr.usable;
    const checked  = dr.selected;
    const count    = dr.indexed || 0;
    const countBadge = count > 0
      ? '<span class="drive-badge">' + count.toLocaleString() + '</span>'
      : '';
    const disabledAttr = canCheck ? '' : ' disabled';
    const checkedAttr  = checked  ? ' checked' : '';
    const itemClass    = canCheck ? 'drive-item' : 'drive-item drive-item--disabled';

    return (
      '<label class="' + itemClass + '">' +
        '<input type="checkbox" class="drive-check" data-path="' + dr.path + '"' +
          checkedAttr + disabledAttr + '>' +
        '<span class="drive-label">' + dr.label + '</span>' +
        countBadge +
      '</label>'
    );
  }).join('');

  // Wire checkbox change events
  list.querySelectorAll('.drive-check').forEach(cb => {
    cb.addEventListener('change', onDriveToggle);
  });
}

async function onDriveToggle(e) {
  const isChecked = e.target.checked;
  const path = e.target.dataset.path;

  if (!isChecked) {
    if (!confirm('Stop indexing and remove all saved data for ' + path + '?')) {
      e.target.checked = true; // revert
      return;
    }
  } else {
    if (!confirm('Add ' + path + ' and start indexing it now?')) {
      e.target.checked = false; // revert
      return;
    }
  }

  // Collect all currently checked drive paths
  const checked = Array.from(
    document.querySelectorAll('.drive-check:checked')
  ).map(cb => cb.dataset.path);

  selectedDrives = checked;

  try {
    await apiPost('/api/drives/select', JSON.stringify({ paths: checked }), true);
    if (isChecked) {
      apiGet('/api/index/start').then(r => {
        if (r.status === 'started') {
           toast('Scan started for ' + path);
           startPoll();
        }
      });
    } else {
      toast('Removed ' + path + ' from index');
    }
  } catch (err) {
    toast('Could not save drive selection', true);
  }
}

// ── Actions ───────────────────────────────────────────────────
function wireDropZone() {
  const dz    = $('dropZone');
  const input = $('fileInput');
  dz.addEventListener('click', () => input.click());
  input.addEventListener('change', e => {
    if (e.target.files.length) setImage(e.target.files[0]);
  });
  dz.addEventListener('dragenter', e => {
    e.preventDefault();
    dz.classList.add('is-dragover');
  });
  dz.addEventListener('dragover', e => {
    e.preventDefault();
    dz.classList.add('is-dragover');
  });
  dz.addEventListener('dragleave', () => dz.classList.remove('is-dragover'));
  dz.addEventListener('drop', e => {
    e.preventDefault();
    dz.classList.remove('is-dragover');
    if (e.dataTransfer.files.length) setImage(e.dataTransfer.files[0]);
  });
}

function setImage(f) {
  queryFile = f;
  $('previewThumb').src = URL.createObjectURL(f);
  $('previewName').textContent = f.name;
  $('dzEmpty').style.display = 'none';
  $('dzPreview').classList.add('is-visible');
  $('searchBtn').textContent = 'Search Library';
  $('searchBtn').disabled = false;
  if (indexedCount > 0) doSearch();
}

function wireActions() {
  $('searchBtn').addEventListener('click', () => {
    if (!queryFile) {
      // No image loaded: trigger a library scan of selected drives
      const checked = Array.from(
        document.querySelectorAll('.drive-check:checked')
      ).map(cb => cb.dataset.path);

      if (checked.length === 0) {
        toast('Select at least one drive to scan', true);
        return;
      }

      apiGet('/api/index/start')
        .then(r => {
          if (r.status === 'no_drives') {
             toast(r.msg, true);
             return;
          }
          if (r.status === 'already_running') {
             toast('Sync is already running', true);
             return;
          }
          toast('Scan started for ' + checked.length + ' drive(s)');
          startPoll();
        })
        .catch(e => {
          toast('Could not start scan', true);
        });
    } else {
      doSearch();
    }
  });

  $('clearBtn').addEventListener('click', async () => {
    const msg = "WARNING: This will permanently clear the entire local database of indexed files.\n\nThe system will rescan from scratch on the next cycle.\n\nAre you sure?";
    if (!confirm(msg)) return;
    try {
      await apiGet('/api/clear');
      queryFile    = null;
      indexedCount = 0;
      
      // Force UI reset
      $('progressWrap').classList.add('hidden');
      renderCounts({}); // Set sidebar counts to 0
      
      $('resultsGrid').innerHTML = '';
      $('dzEmpty').style.display = 'flex';
      $('dzPreview').classList.remove('is-visible');
      $('previewThumb').src = '';
      $('searchBtn').textContent = 'Scan Library';
      $('searchBtn').disabled = false;
      $('resultsTitle').textContent = 'No designs indexed — select drives and click "Scan Library"';
      
      toast('Library cleared successfully', 'success');
      loadDrives();
    } catch (err) {
      toast('Failed to clear library', 'err');
    }
  });

  if ($('refreshBtn')) {
    $('refreshBtn').addEventListener('click', () => {
      loadDrives();
      toast('Sync state & drives refreshed');
    });
  }

  $('syncToggleBtn').addEventListener('click', async () => {
    try {
      const d = await apiGet('/api/index/toggle');
      updateSyncButton(d.user_paused);
      toast(d.user_paused ? 'Sync paused' : 'Sync resumed');
      console.log('[DEBUG] Sync toggled:', d.user_paused ? 'paused' : 'running');
    } catch {
      toast('Failed to toggle sync', true);
    }
  });

  $('modalBg').addEventListener('click', e => {
    if (e.target === $('modalBg')) closeModal();
  });
}

function updateSyncButton(isPaused) {
  const btn = $('syncToggleBtn');
  const txt = $('syncToggleText');
  if (isPaused) {
    btn.classList.add('btn--success');
    btn.classList.remove('btn--outline');
    txt.textContent = 'Start Sync';
  } else {
    btn.classList.remove('btn--success');
    btn.classList.add('btn--outline');
    txt.textContent = 'Stop Sync';
  }
}

// ── Search ────────────────────────────────────────────────────
async function doSearch() {
  if (!queryFile) return;
  const btn = $('searchBtn');
  const bar = $('searchingBar');
  const t0  = Date.now();
  try {
    btn.disabled = true;
    bar.classList.add('is-visible');
    const fd = new FormData();
    fd.append('file',  queryFile);
    fd.append('top_k', '40');
    const d = await apiPost('/api/search', fd, false);
    renderResults(d.results || []);
    $('searchTime').textContent = 'Search took ' + (Date.now() - t0) + 'ms';
  } catch (e) {
    toast('Search failed: ' + e.message, true);
  } finally {
    btn.disabled = false;
    bar.classList.remove('is-visible');
  }
}

function renderResults(res) {
  const grid = $('resultsGrid');
  grid.innerHTML = '';
  // Filter for 70%+ match only when searching (queryFile exists)
  // If no results meet threshold, show helpful message.
  const filtered = queryFile 
    ? res.filter(item => item.score >= 0.01) // Show all visible matches
    : res;

  if (!filtered.length) {
    const el = document.createElement('div');
    el.className = 'grid-empty';
    el.innerHTML = `
      <div class="txt-bold">No matches found in your library</div>
      <div class="txt-sm txt-muted" style="margin-top: 8px;">
        Try uploading a different image or click <b>"Refresh"</b> <br>
        to ensure your latest designs are indexed.
      </div>
    `;
    grid.appendChild(el);
    return;
  }

  filtered.forEach(item => {
    const card = document.createElement('div');
    card.className = 'card';
    const scorePct = (item.score * 100).toFixed(2);
    
    card.innerHTML = `
      <div class="card__thumbs">
        <div class="card__thumb" data-label="Render">
          <img src="${getApiUrl('/api/preview/' + item.id)}" 
               onload="this.classList.add('loaded')"
               onerror="this.parentElement.innerHTML='<span class=\'txt-xs txt-muted\'>No Render</span>'"
               loading="lazy">
        </div>
        <div class="card__thumb" data-label="Photo">
          <img src="${getApiUrl('/api/thumbnail/' + item.id)}" 
               onload="this.classList.add('loaded')"
               onerror="this.parentElement.innerHTML='<span class=\'txt-xs txt-muted\'>No Photo</span>'"
               loading="lazy">
        </div>
      </div>
      <div class="card__body">
        <div class="card__row">
          <span class="tag tag--sm">${item.format.toUpperCase()}</span>
          <span class="card__score">${scorePct}% match</span>
        </div>
      </div>
    `;
    card.onclick = () => openModal(item);
    grid.appendChild(card);
  });
}

// ── Modal ─────────────────────────────────────────────────────
function openModal(r) {
  const qImg = $('modalQueryImg');
  const dImg = $('modalImg');
  const qLabel = qImg.parentElement.querySelector('.section-label');
  const dLabel = dImg.parentElement.querySelector('.section-label');

  if (queryFile) {
    qLabel.textContent = 'Your Search';
    qImg.src = URL.createObjectURL(queryFile);
    
    dLabel.textContent = 'Matched Design';
    dImg.src = getApiUrl('/api/preview/' + r.id);
  } else {
    qLabel.textContent = 'Professional Render';
    qImg.src = getApiUrl('/api/preview/' + r.id);
    
    dLabel.textContent = 'Original Photo';
    dImg.src = getApiUrl('/api/thumbnail/' + r.id);
  }

  $('modalTitle').textContent = r.file_name;
  $('modalPath').textContent  = r.file_path;
  $('modalFormat').textContent = r.format.toUpperCase();
  $('modalSize').textContent  = r.size_kb.toFixed(1) + ' KB';
  $('modalBg').classList.add('is-open');
}
function closeModal() { $('modalBg').classList.remove('is-open'); }

// ── Counts ────────────────────────────────────────────────────
function renderCounts(counts) {
  const container = $('formatCounts');
  if (!container) return;
  
  // Only show EMB stats as requested
  const embCount = counts["emb"] || 0;
  
  if (embCount === 0 && !counts._cleared) {
    container.innerHTML = `
      <div class="card card--static btn--full" style="padding: 16px; border-color: var(--c-border); opacity: 0.6;">
        <div class="section-label" style="margin-bottom: 6px; font-size: 10px;">.EMB Designs Found</div>
        <div class="txt-bold" style="font-size: 24px; color: var(--c-muted);">0</div>
        <div class="txt-sm" style="margin-top: 4px;">Library is empty</div>
      </div>
    `;
    return;
  }
  
  container.innerHTML = `
    <div class="card card--static btn--full" style="padding: 16px; border-color: var(--c-accent); background: rgba(59, 130, 246, 0.03);">
      <div class="section-label" style="margin-bottom: 6px; font-size: 10px; color: var(--c-accent);">.EMB Designs Found</div>
      <div class="txt-bold" style="font-size: 24px; color: var(--c-accent); letter-spacing: -0.5px;">${embCount.toLocaleString()}</div>
      <div class="txt-sm txt-muted" style="margin-top: 4px; font-weight: 400;">Ready for visual search</div>
    </div>
  `;
}

// ── Toast ─────────────────────────────────────────────────────
function toast(msg, type = 'info') {
  const t = $('toast');
  t.textContent = msg;
  t.classList.remove('hidden', 'toast--err', 'toast--success');
  if (type === 'err') t.classList.add('toast--err');
  if (type === 'success') t.classList.add('toast--success');
  setTimeout(() => t.classList.add('hidden'), 3500);
}

// ── Boot ──────────────────────────────────────────────────────
(async function boot() {
  // Prevent browser from opening files dropped outside the zone
  window.addEventListener('dragover', e => e.preventDefault(), false);
  window.addEventListener('drop', e => e.preventDefault(), false);

  wireDropZone();
  wireActions();
  await loadDrives();
  startPoll();

  // Load latest designs to fill the grid initially
  apiGet('/api/latest').then(res => {
    if (!queryFile && res && res.length) {
      renderResults(res);
      $('resultsTitle').textContent = 'Latest indexed .EMB designs';
    }
  });

  // Refresh drive counts every 10s to update the indexed count badges
  setInterval(loadDrives, 10000);
})();
