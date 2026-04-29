/**
 * EMBFinder Electron wrapper
 * Starts the Go backend server then opens a native window.
 * Build: cd electron && npm install && npm run build-linux  (or build-win/build-mac)
 */
const { app, BrowserWindow, shell } = require('electron');
const { spawn, execSync } = require('child_process');
const path = require('path');
const http = require('http');

const PORT = 8765;
const SERVER_URL = `http://localhost:${PORT}`;

let backendProc = null;
let win = null;

// ── Find backend binary ───────────────────────────────────────────────────────
function backendBin() {
  // When packaged, binary is alongside electron app
  const candidates = [
    path.join(process.resourcesPath || '', 'embfinder'),
    path.join(process.resourcesPath || '', 'embfinder.exe'),
    path.join(__dirname, '..', 'go-server', 'embfinder'),
    path.join(__dirname, '..', 'go-server', 'embfinder.exe'),
  ];
  for (const c of candidates) {
    try { require('fs').accessSync(c); return c; } catch {}
  }
  return null;
}

// ── Start Go backend ──────────────────────────────────────────────────────────
function startBackend() {
  const bin = backendBin();
  if (!bin) {
    console.log('[Electron] Backend binary not found — using Docker or external server');
    return;
  }
  backendProc = spawn(bin, [], {
    env: { ...process.env, PORT: String(PORT), DB_PATH: path.join(app.getPath('userData'), 'embfinder.db') },
    stdio: 'inherit',
  });
  backendProc.on('exit', code => console.log('[Backend] exited', code));
}

// ── Wait for server ready ─────────────────────────────────────────────────────
function waitReady(cb, tries = 40) {
  http.get(SERVER_URL + '/api/status', res => {
    if (res.statusCode === 200) cb();
    else retry();
  }).on('error', retry);
  function retry() {
    if (tries <= 0) { cb(); return; } // open anyway
    setTimeout(() => waitReady(cb, tries - 1), 500);
  }
}

// ── Create window ─────────────────────────────────────────────────────────────
function createWindow() {
  win = new BrowserWindow({
    width: 1200,
    height: 760,
    minWidth: 780,
    minHeight: 520,
    title: 'EMBFinder',
    webPreferences: {
      nodeIntegration: false,
      contextIsolation: true,
      preload: path.join(__dirname, 'preload.js'),
    },
    // Native titlebar on all platforms
    titleBarStyle: process.platform === 'darwin' ? 'hiddenInset' : 'default',
  });

  win.loadURL(SERVER_URL);

  // Open external links in browser, not Electron
  win.webContents.setWindowOpenHandler(({ url }) => {
    shell.openExternal(url);
    return { action: 'deny' };
  });
}

// ── App lifecycle ─────────────────────────────────────────────────────────────
app.whenReady().then(() => {
  startBackend();
  waitReady(createWindow);
  app.on('activate', () => { if (BrowserWindow.getAllWindows().length === 0) createWindow(); });
});

app.on('window-all-closed', () => {
  if (backendProc) backendProc.kill();
  if (process.platform !== 'darwin') app.quit();
});
