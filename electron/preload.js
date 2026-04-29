const { contextBridge } = require('electron');

contextBridge.exposeInMainWorld('embfinder', {
  platform: process.platform,
  arch: process.arch,
  version: '1.0.0',
});
