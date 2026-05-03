//go:build !headless

// main_desktop.go — EMBFinder desktop build (Wails native window)
// This file is compiled in the default `go build` path.
// For server/CI/Docker builds that don't need a native window, use:
//   go build -tags headless
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
║    Desktop (Wails) build                    ║
╚═════════════════════════════════════════════╝`)

	startCore()

	// ── HTTP Routes ──────────────────────────────────────────────────────────
	mux := buildMux()

	// ── Config Route ─────────────────────────────────────────────────────────
	mux.HandleFunc("/config.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript")
		fmt.Fprintf(w, "window.API_BASE  = 'http://%s:%s';\n", Config.Host, Config.Port)
		fmt.Fprintf(w, "window.APP_MODE  = '%s';\n", Config.Mode)
	})

	uiFS, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("/", http.FileServer(http.FS(uiFS)))

	// ── Web server in background (for browser / network access) ──────────────
	go func() {
		log.Printf("[HTTP] Listening on http://%s:%s", Config.Host, Config.Port)
		if err := http.ListenAndServe(Config.Host+":"+Config.Port, cors(mux)); err != nil {
			log.Printf("[HTTP] Server stopped: %v", err)
		}
	}()

	// ── HEADLESS env override ─────────────────────────────────────────────────
	if os.Getenv("HEADLESS") == "1" || strings.ToLower(os.Getenv("HEADLESS")) == "true" {
		log.Printf("[App] HEADLESS=1 — running as HTTP server only")
		select {} // block forever
	}

	// ── Wails native window ───────────────────────────────────────────────────
	if err := wails.Run(&options.App{
		Title:  "EMBFinder — Local Visual Search",
		Width:  1280,
		Height: 800,
		AssetServer: &assetserver.Options{
			Assets:  uiFS,
			Handler: mux,
		},
		BackgroundColour: &options.RGBA{R: 240, G: 244, B: 248, A: 255},
		Bind:             []interface{}{},
	}); err != nil {
		log.Fatalf("[App] Wails fatal: %v", err)
	}
}

// ── Embedder lifecycle (desktop only) ────────────────────────────────────────

func autoStartEmbedder() {
	if embedderAlive() {
		log.Printf("[Embedder] Already running at %s", Config.EmbedderURL())
		return
	}

	script := findEmbedderScript()
	if script == "" {
		log.Printf("[Embedder] ERROR: embedder/main.py not found — indexing disabled")
		return
	}
	python := findPython()
	if python == "" {
		log.Printf("[Embedder] ERROR: Python 3.9+ not found — indexing disabled")
		return
	}

	embedderDir := filepath.Dir(script)
	log.Printf("[Embedder] Starting %s from %s", python, embedderDir)

	cmd := exec.Command(python, "-m", "uvicorn", "main:app",
		"--host", Config.EmbedderHost,
		"--port", Config.EmbedderPort,
		"--log-level", "warning",
		"--workers", "1", // single worker to prevent VRAM duplication
	)
	cmd.Dir = embedderDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CLIP_MODEL="+Config.CLIPModel,
	)

	if err := cmd.Start(); err != nil {
		log.Printf("[Embedder] Failed to start: %v", err)
		return
	}
	log.Printf("[Embedder] PID=%d — waiting for startup (model download may take a while)…", cmd.Process.Pid)

	deadline := time.Now().Add(600 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		if embedderAlive() {
			log.Printf("[Embedder] Ready at %s", Config.EmbedderURL())
			return
		}
	}
	log.Printf("[Embedder] WARNING: Startup timeout — model download may still be in progress")
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
		log.Printf("[EmbEngine] WARN: Offline at %s — EMB preview rendering unavailable", Config.EmbEngineURL())
		return
	}
	resp.Body.Close()
	log.Printf("[EmbEngine] Connected at %s", Config.EmbEngineURL())
}
