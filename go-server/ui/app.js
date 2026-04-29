const $ = id => document.getElementById(id);
let queryFile = null;
let indexedCount = 0;
let pollTimer = null;

// ── API Helpers ──────────────────────────────────────────────
async function apiGet(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error('API Error');
  return r.json();
}

async function apiPost(url, body) {
  const r = await fetch(url, { method: 'POST', body });
  if (!r.ok) throw new Error('API Error');
  return r.json();
}

// ── Status & Polling ──────────────────────────────────────────
async function refreshStatus() {
  try {
    const d = await apiGet('/api/index/state');
    indexedCount = d.total_indexed || 0;
    
    // UI Update - SUCCESS
    $('statusTxt').textContent = 'Online — ' + indexedCount.toLocaleString() + ' designs';
    $('dot').className = 'dot dot--ok';
    
    $('resultsTitle').textContent = indexedCount > 0
      ? indexedCount.toLocaleString() + ' designs indexed — upload an image to search'
      : 'No designs indexed — sync your library automatically';

    if (d.idx_state && d.idx_state.counts) renderCounts(d.idx_state.counts);
    else if (d.counts) renderCounts(d.counts);

    updateSyncButton(d.user_paused);

    if (d.running || d.user_paused || (d.status && d.status !== "Idle")) {
      startPoll();
      showProgress(d);
    } else {
      stopPoll();
      $('progressWrap').style.display = 'none';
    }
  } catch (err) {
    console.error('Status check failed:', err);
    // Only show offline if it consistently fails
    if ($('statusTxt').textContent === 'Connecting...') {
       $('statusTxt').textContent = 'Offline (Retrying...)';
       $('dot').className = 'dot dot--err';
    }
    // Still try to poll to recover
    startPoll();
  }
}

function startPoll() { if (!pollTimer) pollTimer = setInterval(refreshStatus, 2000); }
function stopPoll() { if (pollTimer) { clearInterval(pollTimer); pollTimer = null; } }

function showProgress(state) {
  $('progressWrap').style.display = 'block';
  const processed = state.processed || 0;
  const total = state.total || 0;
  const p = total > 0 ? Math.round((processed / total) * 100) : 0;
  
  $('progLabel').textContent = state.status || 'Syncing Library...';
  $('progFill').style.width = p + '%';
  $('progCount').textContent = `${p}% (${processed.toLocaleString()} / ${total.toLocaleString()})`;
}

// ── UI Actions ────────────────────────────────────────────────
function wireDropZone() {
  const dz = $('dropZone');
  const input = $('fileInput');

  dz.addEventListener('click', () => input.click());
  input.addEventListener('change', e => {
    if (e.target.files.length) setImage(e.target.files[0]);
  });

  dz.addEventListener('dragover', e => { e.preventDefault(); dz.style.borderColor = 'var(--c-accent)'; });
  dz.addEventListener('dragleave', () => dz.style.borderColor = '');
  dz.addEventListener('drop', e => {
    e.preventDefault();
    dz.style.borderColor = '';
    if (e.dataTransfer.files.length) setImage(e.dataTransfer.files[0]);
  });
}

function setImage(f) {
  queryFile = f;
  $('previewThumb').src = URL.createObjectURL(f);
  $('previewName').textContent = f.name;
  $('dzEmpty').style.display = 'none';
  $('dzPreview').classList.add('is-visible');
  $('searchBtn').disabled = false;
  if (indexedCount > 0) doSearch();
}

function wireActions() {
  $('searchBtn').addEventListener('click', doSearch);
  
  $('clearBtn').addEventListener('click', async () => {
    if (!confirm('Clear all indexed data? This cannot be undone.')) return;
    try {
      await apiGet('/api/clear');
      
      // Full UI & State Reset
      queryFile = null;
      indexedCount = 0;
      $('resultsGrid').innerHTML = '';
      $('dzEmpty').style.display = 'flex';
      $('dzPreview').classList.remove('is-visible');
      $('previewThumb').src = '';
      $('searchBtn').disabled = true;
      $('resultsTitle').textContent = 'No designs indexed — sync your library automatically';
      
      toast('Library cleared successfully');
      refreshStatus();
    } catch (e) {
      toast('Failed to clear library', 'err');
    }
  });

  $('syncToggleBtn').addEventListener('click', async () => {
    try {
      const d = await apiGet('/api/index/toggle');
      updateSyncButton(d.user_paused);
      toast(d.user_paused ? 'Synchronization paused' : 'Synchronization resumed');
    } catch (e) {
      toast('Failed to toggle synchronization', 'err');
    }
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
  const t0 = Date.now();

  try {
    btn.disabled = true;
    bar.classList.add('is-visible');
    
    const fd = new FormData();
    fd.append('file', queryFile);
    fd.append('top_k', '40');
    
    const d = await apiPost('/api/search', fd);
    renderResults(d.results || []);
    $('searchTime').textContent = `Search took ${Date.now() - t0}ms`;
  } catch (e) {
    toast('Search failed', 'err');
  } finally {
    btn.disabled = false;
    bar.classList.remove('is-visible');
  }
}

function renderResults(res) {
  const grid = $('resultsGrid');
  grid.innerHTML = '';
  if (!res.length) {
    grid.innerHTML = '<div style="grid-column:1/-1;text-align:center;padding:40px;color:var(--c-muted)">No similar designs found</div>';
    return;
  }
  res.forEach(r => {
    const el = document.createElement('div');
    el.className = 'card';
    el.innerHTML = `
      <div class="card__thumb">
        <img src="/api/preview/${r.id}" loading="lazy">
      </div>
      <div class="card__body">
        <div class="card__name" title="${r.file_name}">${r.file_name}</div>
        <div style="display:flex;justify-content:space-between;margin-top:4px">
           <span class="tag">${r.format.toUpperCase()}</span>
           <span style="font-size:10px;color:var(--c-muted)">${Math.round(r.score * 100)}% Match</span>
        </div>
      </div>
    `;
    el.onclick = () => openModal(r);
    grid.appendChild(el);
  });
}

// ── Modal ────────────────────────────────────────────────────
function openModal(r) {
  $('modalImg').src = `/api/preview/${r.id}`;
  $('modalTitle').textContent = r.file_name;
  $('modalPath').textContent = r.file_path;
  $('modalFormat').textContent = r.format.toUpperCase();
  $('modalSize').textContent = r.size_kb.toFixed(1) + ' KB';
  $('modalBg').classList.add('is-open');
}

$('modalBg').addEventListener('click', e => { if (e.target === $('modalBg')) closeModal(); });
function closeModal() { $('modalBg').classList.remove('is-open'); }

// ── Drives & Counts ──────────────────────────────────────────
async function loadDrives() {
  try {
    const d = await apiGet('/api/drives');
    const list = $('driveList');
    list.innerHTML = (d.drives || []).map(dr => `
      <div class="drive-item">
        <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2"><path d="M22 12H2M5.45 5.11 2 12v6a2 2 0 0 0 2 2h16a2 2 0 0 0 2-2v-6l-3.45-6.89A2 2 0 0 0 16.76 4H7.24a2 2 0 0 0-1.79 1.11z"/></svg>
        ${dr.label || dr.path}
      </div>
    `).join('');
    
    $('fmtWrap').innerHTML = (d.formats || []).map(f => `<span class="tag">${f}</span>`).join('');
  } catch {}
}

function renderCounts(counts) {
  const container = $('formatCounts');
  if (!container) return;
  const entries = Object.entries(counts || {});
  if (entries.length === 0) {
    container.innerHTML = '<span style="font-size:12px;color:var(--c-muted)">Analyzing library...</span>';
    return;
  }
  container.innerHTML = entries
    .sort((a,b) => b[1] - a[1])
    .map(([ext, count]) => `<span class="tag"><b>${ext.toUpperCase()}</b>: ${count.toLocaleString()}</span>`)
    .join('');
}

function toast(msg, type = '') {
  const t = $('toast');
  t.textContent = msg;
  t.style.background = type === 'err' ? 'var(--c-danger)' : 'var(--c-text)';
  t.style.display = 'block';
  setTimeout(() => t.style.display = 'none', 3000);
}

// ── Boot ──────────────────────────────────────────────────────
(async function boot() {
  wireDropZone();
  wireActions();
  await Promise.all([refreshStatus(), loadDrives()]);
})();
