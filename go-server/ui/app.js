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

  document.getElementById('refreshBtn').addEventListener('click', () => {
    DriveController.reload();
    if (TabController.current() === 'library') LibraryController.load(1, LibraryController.getFilter());
    Toast.show('Refreshed');
  });

  document.getElementById('syncToggleBtn').addEventListener('click', () => SyncController.toggle());
  document.getElementById('clearBtn')     .addEventListener('click', () => SyncController.clearAll());

  // ── Search button ─────────────────────────────────────────────────────────
  document.getElementById('searchBtn').addEventListener('click', () => {
    if (DropController.getFile()) {
      SearchController.run();
    } else {
      // Trigger a scan if no file selected
      const selected = [...document.querySelectorAll('.drive-check:checked')]
        .map(c => c.dataset.path);
      if (!selected.length) { Toast.show('Select at least one drive to scan', 'err'); return; }
      API.get('/api/index/start').then(r => {
        if (r.status === 'no_drives')       Toast.show(r.msg || 'No drives selected', 'err');
        else if (r.status === 'already_running') Toast.show('Sync already running');
        else { Toast.show('Scan started'); SyncController.start(); }
      }).catch(() => Toast.show('Could not start scan', 'err'));
    }
  });

  // ── Modal backdrop + close button ────────────────────────────────────────
  document.getElementById('modalBg').addEventListener('click', e => {
    if (e.target === document.getElementById('modalBg')) ModalController.close();
  });
  document.getElementById('modalCloseBtn').addEventListener('click', () => ModalController.close());

  // ── Modal action buttons ──────────────────────────────────────────────────
  document.getElementById('openFolderBtn').addEventListener('click', () => ModalController.openFolder());
  document.getElementById('copyPathBtn')  .addEventListener('click', () => ModalController.copyPath());

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
})();
