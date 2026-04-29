// Minimal preload — no Node APIs exposed to renderer for security
const { contextBridge } = require('electron');
const process = require('process');

contextBridge.exposeInMainWorld('embfinder', {
  platform: process.platform,
  arch: process.arch,
  version: '1.0.0',
});
