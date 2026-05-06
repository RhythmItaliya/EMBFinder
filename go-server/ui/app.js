/* ================================================================
   app.js — EMBFinder boot / orchestrator
   Wires event listeners and starts all controllers.
   No application logic lives here — see controllers.js.
   ================================================================ */
'use strict';

(function boot() {
  // ── Global drag guard (prevent browser from opening dropped files) ─────────
  window.addEventListener('dragover', e => e.preventDefault(), false);
  window.addEventListener('drop',    e => e.preventDefault(), false);

  // ── Keyboard shortcuts ────────────────────────────────────────────────────
  document.addEventListener('keydown', e => {
    if (e.key === 'Escape') ModalController.close();
  });

  // ── Header buttons ────────────────────────────────────────────────────────
  document.getElementById('tabSearch') .addEventListener('click', () => TabController.switchTo('search'));
  document.getElementById('tabLibrary').addEventListener('click', () => TabController.switchTo('library'));
  document.getElementById('tabFolders').addEventListener('click', () => {
    TabController.switchTo('folders');
    FolderController.refresh();
  });
  document.getElementById('addFolderBtn').addEventListener('click', () => FolderController.addFolder());
  document.getElementById('scanAllBtn').addEventListener('click', () => FolderController.scanAll());
  const repairBtn = document.getElementById('repairDbBtn');
  if (repairBtn) {
    repairBtn.addEventListener('click', async () => {
      repairBtn.disabled = true;
      repairBtn.textContent = 'Backing up…';
      try {
        const bk = await API.postJSON('/api/db/backup', {});
        if (bk.status !== 'ok') { Toast.show('Backup failed: ' + (bk.error || '?'), 'err'); return; }
        Toast.show('Backup saved: ' + bk.backup_path);
        repairBtn.textContent = 'Repairing…';
        const rp = await API.postJSON('/api/db/repair', {});
        Toast.show(`Repair done: removed ${rp.removed || 0} duplicates`, 'success');
        FolderController.refresh();
      } catch(e) {
        Toast.show('Repair failed: ' + e.message, 'err');
      } finally {
        repairBtn.disabled = false;
        repairBtn.textContent = '🛠 Repair DB';
      }
    });
  }


  document.getElementById('refreshBtn').addEventListener('click', () => {
    DriveController.reload();
    if (TabController.current() === 'library') LibraryController.load(1, LibraryController.getFilter());
    Toast.show('Refreshed');
  });

  const perfSelect = document.getElementById('perfModeSelect');
  if (perfSelect) {
    perfSelect.addEventListener('change', async (e) => {
      const mode = e.target.value;
      try {
        await API.postJSON('/api/perf/mode', { mode });
        Toast.show(`Mode set: ${mode}`);
      } catch {
        Toast.show('Failed to set mode', 'err');
      }
    });
  }

  document.getElementById('syncToggleBtn').addEventListener('click', () => SyncController.toggle());
  document.getElementById('clearBtn')     .addEventListener('click', () => SyncController.clearAll());

  // ── Search button ─────────────────────────────────────────────────────────
  document.getElementById('searchBtn').addEventListener('click', () => {
    if (DropController.getFile()) {
      SearchController.run();
    } else {
      // Trigger indexing if no file selected
      const selected = DriveController.getSelectedPaths();
      if (!selected.length) { Toast.show('Select at least one folder/drive to scan', 'err'); return; }
      API.postJSON('/api/index/start', { paths: selected, force: false }).then(r => {
        if (r.status === 'no_paths')       Toast.show(r.msg || 'No valid folders selected', 'err');
        else if (r.status === 'already_running') Toast.show('Sync already running');
        else if (r.status === 'queued') { Toast.show('Scan queued'); document.getElementById('statusTxt').textContent = 'Starting scan...'; }
        else { Toast.show('Scan started'); document.getElementById('statusTxt').textContent = 'Starting scan...'; SyncController.start(); }
      }).catch(() => Toast.show('Could not start scan', 'err'));
    }
  });

  // ── Modal backdrop + close button ────────────────────────────────────────
  document.getElementById('modalBg').addEventListener('click', e => {
    if (e.target === document.getElementById('modalBg')) ModalController.close();
  });
  document.getElementById('modalCloseBtn').addEventListener('click', () => ModalController.close());

  // ── Modal action buttons ──────────────────────────────────────────────────
  document.getElementById('openTruesizerBtn').addEventListener('click', () => ModalController.openInTrueSizer());
  document.getElementById('openFolderBtn')   .addEventListener('click', () => ModalController.openFolder());
  document.getElementById('copyPathBtn')     .addEventListener('click', () => ModalController.copyPath());

  // ── SSE: update search button text when count changes ────────────────────
  document.addEventListener('emb:indexed', e => {
    const count = e.detail.count;
    const btn   = document.getElementById('searchBtn');
    if (!DropController.getFile()) {
      btn.textContent = count > 0 ? 'Search Library' : 'Scan Library';
      btn.disabled    = false;
    }
  });

  // ── SSE: reset UI on clear ────────────────────────────────────────────────
  document.addEventListener('emb:cleared', () => {
    DropController.clearFile();
    document.getElementById('libSearchInput').value = '';
    document.getElementById('libGrid').innerHTML = '';
    document.getElementById('libCount').textContent = '';
    document.getElementById('libPagination').innerHTML = '';
  });

  // ── Library search box ────────────────────────────────────────────────────
  LibraryController.wireSearch();

  // ── Drop zone ─────────────────────────────────────────────────────────────
  DropController.wire();

  // ── Start ─────────────────────────────────────────────────────────────────
  DriveController.reload();
  SyncController.start();
  FolderController.init();
  TabController.restore();
  PerfController.init();
})();
