//go:build headless

// main_headless.go — EMBFinder server/headless build (no Wails, no CGO)
// This file is compiled when -tags headless is passed.
//
// Use this for:
//   - Linux .deb / .rpm packages
//   - Windows .exe (server mode, no native window)
//   - macOS .tar.gz (server mode)
//   - Docker / Docker Compose
//   - CI/CD pipelines
//
// Build: CGO_ENABLED=0 go build -tags headless -o embfinder .
// Run:   ./embfinder                         (opens browser at http://localhost:8765)
//        HEADLESS=1 ./embfinder              (pure background HTTP server)
package main

import (
	"embed"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os/exec"
	"runtime"
)

//go:embed ui
var uiFiles embed.FS

func main() {
	initConfig()

	fmt.Println(`
╔═════════════════════════════════════════════╗
║    EMBFinder — Embroidery Visual Search     ║
║    Server / Headless build                  ║
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

	var uiFS http.FileSystem
	if Config.Mode == "development" {
		uiFS = http.Dir("ui")
		log.Printf("[HTTP] Serving UI from filesystem (development mode)")
	} else {
		sub, _ := fs.Sub(uiFiles, "ui")
		uiFS = http.FS(sub)
		log.Printf("[HTTP] Serving UI from embedded files (production mode)")
	}
	mux.Handle("/", http.FileServer(uiFS))

	// Auto-open browser on non-headless runs
	url := fmt.Sprintf("http://%s:%s", Config.Host, Config.Port)
	go openBrowser(url)

	log.Printf("[HTTP] Listening on %s — open browser if it did not launch automatically", url)
	if err := http.ListenAndServe(Config.Host+":"+Config.Port, cors(mux)); err != nil {
		log.Fatalf("[HTTP] Fatal: %v", err)
	}
}

// openBrowser opens the default system browser on all three platforms.
func openBrowser(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default: // linux and others
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

// Stub: headless builds don't auto-start the Python embedder.
// Start it separately: cd embedder && python -m uvicorn main:app --port 8766
func autoStartEmbedder() {
	log.Printf("[Embedder] Headless build: start embedder manually on port %s", Config.EmbedderPort)
	log.Printf("[Embedder]   cd embedder && python -m uvicorn main:app --host %s --port %s --workers 1",
		Config.EmbedderHost, Config.EmbedderPort)
}

func checkEmbEngine() {
	// In headless mode the EmbEngine is managed by Docker Compose or the user
	log.Printf("[EmbEngine] Expected at %s", Config.EmbEngineURL())
}
