package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

//go:embed ui
var uiFiles embed.FS

func main() {
	initConfig()

	fmt.Println(`
╔═════════════════════════════════════════════╗
║    EMBFinder — Embroidery Visual Search     ║
║    Version 1.0.0-beta | Local-First AI      ║
╚═════════════════════════════════════════════╝`)

	// ── Database ──────────────────────────────────────────────────────────────
	if err := initDB(Config.DBPath); err != nil {
		log.Fatalf("DB init: %v", err)
	}
	log.Printf("Database: %s", Config.DBPath)
	RefreshIdxStateCounts()

	// ── Load existing embeddings from SQLite into memory ──────────────────────
	entries, _ := dbLoadAll()
	for _, e := range entries {
		globalIndex.Add(e)
	}
	log.Printf("Loaded %d designs into memory index", globalIndex.Count())

	// ── Drive detection ────────────────────────────────────────────────────────
	drives := autoLibPaths()
	log.Printf("Auto-detected drives: %d", len(drives))
	for _, d := range drives {
		log.Printf("  -> %s (%s)", d.Label, d.Path)
	}

	// ── Filesystem watcher (real-time new-file detection) ──────────────────────
	log.Printf("Starting background filesystem watcher...")
	StartWatcher()

	// ── Python embedder (MobileCLIP-B via uvicorn) ────────────────────────────
	go autoStartEmbedder()

	// ── EmbEngine health check ────────────────────────────────────────────────
	go checkEmbEngine()

	// ── AUTO-SYNC: this is the perpetual background indexing loop ─────────────
	// It waits for the first UI heartbeat, then starts scanning all drives.
	// Without this goroutine, nothing ever gets indexed.
	go AutoIndexAllDrives()
	log.Printf("Autonomous Sync: goroutine started — waiting for app window...")

	// ── HTTP Routes ────────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/api/drives", hDriveList)
	mux.HandleFunc("/api/drives/select", hDriveSelect)
	mux.HandleFunc("/api/search", hSearch)
	mux.HandleFunc("/api/preview/", hPreview)
	mux.HandleFunc("/api/thumbnail/", hThumbnail)
	mux.HandleFunc("/api/index/state/stream", hIndexStateStream)
	mux.HandleFunc("/api/index/toggle", hToggleSync)
	mux.HandleFunc("/api/index/start", hIndexStart)
	mux.HandleFunc("/api/clear", hClear)
	mux.HandleFunc("/api/latest", hLatest)
	mux.HandleFunc("/api/browse", hBrowseEMB)
	mux.HandleFunc("/api/open-file", hOpenFile)
	mux.HandleFunc("/api/emb-info", hEmbInfo)

	// Embedded UI
	uiFS, _ := fs.Sub(uiFiles, "ui")

	// ── Config Route ──────────────────────────────────────────────────────────
	// Inject the actual listening port into the UI so the desktop app can 
	// connect to the high-performance background server (supporting SSE/Streaming)
	mux.HandleFunc("/config.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		// API_BASE  → used by api.js to prefix all fetch() calls
		// APP_MODE  → used by DriveController to hide dev-only paths in production
		fmt.Fprintf(w, "window.API_BASE  = 'http://%s:%s';\n", Config.Host, Config.Port)
		fmt.Fprintf(w, "window.APP_MODE  = '%s';\n", Config.Mode)
	})

	mux.Handle("/", http.FileServer(http.FS(uiFS)))

	// ── Web Server (Background) ────────────────────────────────────────────────
	// Run the standard web server in the background for mobile/network access
	go func() {
		log.Printf("Listening on http://%s:%s", Config.Host, Config.Port)
		if err := http.ListenAndServe(Config.Host+":"+Config.Port, cors(mux)); err != nil {
			log.Printf("Web server stopped: %v", err)
		}
	}()

	// ── Headless mode (skip Wails desktop window) ─────────────────────────────
	if os.Getenv("HEADLESS") == "1" || strings.ToLower(os.Getenv("HEADLESS")) == "true" {
		log.Printf("HEADLESS=1: skipping Wails desktop app")
		select {}
	}

	// ── Wails Native App (Foreground) ──────────────────────────────────────────
	// Launch the beautiful native desktop window
	err := wails.Run(&options.App{
		Title:  "EMBFinder — Local Visual Search",
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets:  uiFS,
			Handler: mux, // Pass all /api/ requests directly to our Go router!
		},
		BackgroundColour: &options.RGBA{R: 255, G: 255, B: 255, A: 255},
		Bind:             []interface{}{},
	})

	if err != nil {
		log.Fatalf("Fatal error starting Wails desktop app: %v", err)
	}
}

// ── Embedder lifecycle ────────────────────────────────────────────────────────

func autoStartEmbedder() {
	if embedderAlive() {
		log.Printf("Python embedder: already running at %s", Config.EmbedderURL())
		return
	}

	script := findEmbedderScript()
	if script == "" {
		log.Printf("[ERROR] Python embedder script not found — indexing disabled")
		return
	}

	python := findPython()
	if python == "" {
		log.Printf("[ERROR] Python 3.9+ not found — indexing disabled")
		return
	}

	embedderDir := filepath.Dir(script)
	log.Printf("Python embedder: starting %s from %s", python, embedderDir)

	cmd := exec.Command(python, "-m", "uvicorn", "main:app",
		"--host", Config.EmbedderHost,
		"--port", Config.EmbedderPort,
		"--log-level", "warning",
	)
	cmd.Dir = embedderDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CLIP_MODEL="+Config.CLIPModel, // forwarded from .env / CLIP_MODEL env var
	)

	if err := cmd.Start(); err != nil {
		log.Printf("Python embedder: failed to start: %v", err)
		return
	}
	log.Printf("Python embedder: pid=%d — waiting for startup...", cmd.Process.Pid)

	deadline := time.Now().Add(600 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if embedderAlive() {
			log.Printf("\033[32mSUCCESS: DeepScan AI Engine is ready and online!\033[0m")
			return
		}
	}
	log.Printf("\033[33mWARNING: Python embedder startup timeout! (Initial model download may still be in progress)\033[0m")
}

func embedderAlive() bool {
	resp, err := httpClient.Get(Config.EmbedderURL() + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func findEmbedderScript() string {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)
	candidates := []string{
		filepath.Join(exeDir, "../embedder/main.py"),
		filepath.Join(exeDir, "../../embedder/main.py"),
		"embedder/main.py",
		"../embedder/main.py",
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}
	return ""
}

func findPython() string {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = append(names, "py")
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			cmd := exec.Command(p, "-c", "import sys; sys.exit(0 if sys.version_info >= (3,9) else 1)")
			if err := cmd.Run(); err == nil {
				return p
			}
		}
	}
	return ""
}

func checkEmbEngine() {
	time.Sleep(5 * time.Second)
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(Config.EmbEngineURL() + "/health")
	if err != nil || resp.StatusCode != 200 {
		log.Printf("[WARN] EmbEngine offline at %s — EMB preview rendering unavailable", Config.EmbEngineURL())
		return
	}
	resp.Body.Close()
	log.Printf("[OK] EmbEngine connected at %s", Config.EmbEngineURL())
}
