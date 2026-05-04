/* ================================================================
   api.js — EMBFinder network layer
   All fetch calls go through here. Nothing else touches fetch().
   ================================================================ */
'use strict';

const API = (() => {
  // config.js injects window.API_BASE; fallback to same-origin
  const base = () => (window.API_BASE || '').replace(/\/$/, '');

  async function _req(method, path, body, isJSON) {
    const url  = base() + path;
    const opts = { method };
    if (body !== undefined) {
      opts.body = body;
      if (isJSON) opts.headers = { 'Content-Type': 'application/json' };
    }
    const r = await fetch(url, opts);
    if (!r.ok) {
      const text = await r.text().catch(() => '');
      throw new Error(`HTTP ${r.status} ${text}`.trim());
    }
    return r.json();
  }

  return {
    get:        path          => _req('GET',    path),
    post:       (path, body)  => _req('POST',   path, body, false),
    postJSON:   (path, data)  => _req('POST',   path, JSON.stringify(data), true),
    delete:     path          => _req('DELETE', path, '{}', true),

    previewUrl:   id => base() + '/api/preview/'   + id,
    thumbnailUrl: id => base() + '/api/thumbnail/' + id,

    streamUrl: () => base() + '/api/index/state/stream',

    // Fetch real stitch info for an EMB design (id or path)
    embInfo: (payload) => _req('POST', '/api/emb-info', JSON.stringify(payload), true),

    // Open TrueSizer / folder — return promises for spinner integration
    openTrueSizer: (payload) => _req('POST', '/api/open-truesizer', JSON.stringify(payload), true),
    openFolder:    (payload) => _req('POST', '/api/open-file',      JSON.stringify(payload), true),
  };
})();
