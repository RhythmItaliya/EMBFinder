// server.go — shared startup logic used by both desktop and headless builds.
// The main() function lives in main_desktop.go (!headless) or main_headless.go (headless).
// This file is compiled in BOTH build paths.
package main

import (
	"log"
	"net/http"
	"time"
)

// version and buildDate are injected at build time via ldflags:
//
//	-X main.version=1.2.3
//	-X main.buildDate=2026-05-03
var (
	version   = "dev"
	buildDate = "unknown"
)

// startCore initialises the database, loads the in-memory index, starts the
// filesystem watcher, and launches background goroutines.
// Called by both desktop and headless main().
func startCore() {
	log.Printf("[Core] EMBFinder %s (built %s)", version, buildDate)
	initPerfManager()

	// ── Database ─────────────────────────────────────────────────────────────
	if err := initDB(Config.DBPath); err != nil {
		log.Fatalf("[DB] Init failed: %v", err)
	}
	log.Printf("[DB] Path: %s", Config.DBPath)
	dbStartupCleanup() 

	// ── Load index from DB into memory ───────────────────────────────────────
	entries, _ := dbLoadAll()
	for _, e := range entries {
		globalIndex.Add(e)
	}
	log.Printf("[Index] Loaded %d designs from database", globalIndex.Count())
	// Refresh AFTER loading so the SSE broadcast shows the real count
	RefreshIdxStateCounts()


	// ── Drive detection ───────────────────────────────────────────────────────
	drives := autoLibPaths()
	initSelectedDrives()
	log.Printf("[Drives] Auto-detected: %d", len(drives))
	for _, d := range drives {
		log.Printf("[Drives]   → %s (%s)", d.Label, d.Path)
	}

	// ── Filesystem watcher ────────────────────────────────────────────────────
	StartWatcher()
	log.Printf("[Watcher] Background filesystem watcher started")

	// ── Python embedder ───────────────────────────────────────────────────────
	// autoStartEmbedder() is defined per build tag:
	//   desktop:  starts uvicorn subprocess automatically
	//   headless: logs a "start it yourself" message
	go autoStartEmbedder()

	// ── EmbEngine health check ────────────────────────────────────────────────
	go checkEmbEngine()

	// ── Autonomous background indexing loop ───────────────────────────────────
	go AutoIndexAllDrives()
	log.Printf("[Sync] Autonomous indexing goroutine started")

	// ── Periodic stats refresh ────────────────────────────────────────────────
	go func() {
		for {
			time.Sleep(5 * time.Second)
			RefreshIdxStateCounts()
		}
	}()

	// ── Stall watchdog: auto-recover stuck indexing ──────────────────────────
	go func() {
		for {
			time.Sleep(15 * time.Second)
			idxState.mu.Lock()
			running := idxState.Running
			paused := idxState.UserPaused
			last := idxState.LastProgressAt
			if running && !paused && !last.IsZero() && time.Since(last) > 2*time.Minute {
				idxState.Running = false
				idxState.ScanDone = true
				idxState.Status = "Auto-recovered from stalled scan"
				log.Printf("[Watchdog] Scan stall detected; force-stopped indexer for recovery")
			}
			idxState.mu.Unlock()
		}
	}()
}

// buildMux registers all /api/* routes and returns the mux.
// The caller adds /config.js and the static file server.
func buildMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/drives", hDriveList)
	mux.HandleFunc("/api/drives/select", hDriveSelect)
	mux.HandleFunc("/api/search", hSearch)
	mux.HandleFunc("/api/preview/", hPreview)
	mux.HandleFunc("/api/thumbnail/", hThumbnail)
	mux.HandleFunc("/api/index/state/stream", hIndexStateStream)
	mux.HandleFunc("/api/index/start", hIndexStart)
	mux.HandleFunc("/api/index/toggle", hToggleSync)
	mux.HandleFunc("/api/index/stop-all", hStopAllIndexing)
	mux.HandleFunc("/api/clear", hClear)
	mux.HandleFunc("/api/perf", hPerf)
	mux.HandleFunc("/api/perf/mode", hPerfMode)
	mux.HandleFunc("/api/latest", hLatest)
	mux.HandleFunc("/api/browse", hBrowseEMB)
	mux.HandleFunc("/api/open-file", hOpenFile)
	mux.HandleFunc("/api/emb-info", hEmbInfo)
	mux.HandleFunc("/api/open-truesizer", hOpenTrueSizer)
	mux.HandleFunc("/api/folders", hFolderList)
	mux.HandleFunc("/api/folders/rescan", hFolderRescan)
	mux.HandleFunc("/api/folders/stop", hFolderStop)
	mux.HandleFunc("/api/pick-folder", hPickFolder)
	mux.HandleFunc("/api/db/backup", hDbBackup)
	mux.HandleFunc("/api/db/repair", hDbRepair)
	return mux
}

// cors adds permissive CORS headers — defined in handlers.go
// embedderAlive checks if the Python AI service is reachable.
func embedderAlive() bool {
	resp, err := httpClient.Get(Config.EmbedderURL() + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}
