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
	"time"
)

//go:embed ui
var uiFiles embed.FS

func main() {
	initConfig() // Loads all local config + aggressive GC tuning

	fmt.Println(`
╔═════════════════════════════════════════════╗
║    EMBFinder — Embroidery Visual Search     ║
║    Version 1.0.0-beta | Local-First AI      ║
╚═════════════════════════════════════════════╝`)

	// ── Database ────────────────────────────────────────────────────────────────
	if err := initDB(Config.DBPath); err != nil {
		log.Fatalf("DB init: %v", err)
	}
	log.Printf("Database: %s", Config.DBPath)

	// ── Load index from SQLite into memory ────────────────────────────────────
	entries, _ := dbLoadAll()
	for _, e := range entries {
		globalIndex.Add(e)
	}
	log.Printf("Loaded %d designs into memory index", globalIndex.Count())

	// ── Drive detection ────────────────────────────────────────────────────────
	log.Printf("Auto-detected drives: %d", len(autoLibPaths()))

	// ── Auto-start Python embedder (MobileCLIP-B) ──────────────────────────────
	go autoStartEmbedder()

	// ── Validate Wilcom Automation Engine ──────────────────────────────────────
	go checkWilcom()

	// ── Local CLIP ONNX (Disabled) ─────────────────────────────
	// We use the Python embedder (ViT-L/14, 768-dim) exclusively for maximum accuracy.
	// Mixing local ONNX (512-dim) with Python (768-dim) breaks cosine similarity.
	// go func() { InitCLIP() }()

	// ── HTTP Routes ────────────────────────────────────────────────────────────
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", hStatus)
	mux.HandleFunc("/api/drives", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]interface{}{
			"drives":  autoLibPaths(),
			"formats": AllSupportedFormats(),
		})
	})
	mux.HandleFunc("/api/scan", hScan)
	mux.HandleFunc("/api/search", hSearch)
	mux.HandleFunc("/api/preview/", hPreview)
	mux.HandleFunc("/api/index/state", hIndexState)
	mux.HandleFunc("/api/index", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			hIndex(w, r)
		case http.MethodDelete:
			hClear(w, r)
		default:
			http.Error(w, "method not allowed", 405)
		}
	})

	// Serve embedded UI
	uiFS, _ := fs.Sub(uiFiles, "ui")
	mux.Handle("/", http.FileServer(http.FS(uiFS)))

	log.Printf("Listening on http://%s:%s", Config.Host, Config.Port)
	if err := http.ListenAndServe(Config.Host+":"+Config.Port, cors(mux)); err != nil {
		log.Fatal(err)
	}
}

// autoStartEmbedder locates and starts the Python embedder as a subprocess.
// It waits for it to become healthy before returning.
// If already running (e.g. started externally), it does nothing.
func autoStartEmbedder() {
	// Check if already running
	if embedderAlive() {
		log.Printf("Python embedder: already running at %s", Config.EmbedderURL())
		return
	}

	// Locate the embedder directory relative to the binary or common paths
	script := findEmbedderScript()
	if script == "" {
		log.Printf("\033[31mCRITICAL ERROR: Python embedder script (main.py) not found!\033[0m")
		log.Printf("\033[31mIndexing features will be COMPLETELY disabled.\033[0m")
		return
	}

	python := findPython()
	if python == "" {
		log.Printf("\033[31mCRITICAL ERROR: Python 3.9+ is strictly required but was not found on your system.\033[0m")
		log.Printf("\033[33mACTION REQUIRED: Install Python and ensure it is added to your system PATH.\033[0m")
		return
	}

	embedderDir := filepath.Dir(script)
	log.Printf("Python embedder: starting  %s  from  %s", python, embedderDir)

	cmd := exec.Command(python, "-m", "uvicorn", "main:app",
		"--host", Config.EmbedderHost,
		"--port", Config.EmbedderPort,
		"--log-level", "warning",
	)
	cmd.Dir = embedderDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(),
		"CLIP_MODEL=MobileCLIP-B",
		"CLIP_PRETRAINED=datacompdr",
	)

	if err := cmd.Start(); err != nil {
		log.Printf("Python embedder: failed to start: %v", err)
		return
	}
	log.Printf("Python embedder: pid=%d — waiting for startup…", cmd.Process.Pid)

	// Wait up to 120s for the model to load (MobileCLIP-B first download)
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(2 * time.Second)
		if embedderAlive() {
			log.Printf("\033[32mSUCCESS: MobileCLIP-B Engine is ready and online!\033[0m")
			return
		}
	}

	log.Printf("\033[33mWARNING: Python embedder startup timeout! Image vectorization may fail.\033[0m")
	log.Printf("\033[33mACTION REQUIRED: Ensure Python 3.9+ is installed and 'pip install -r embedder/requirements.txt' was run.\033[0m")
}

func embedderAlive() bool {
	resp, err := httpClient.Get(Config.EmbedderURL() + "/health")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

// findEmbedderScript searches for embedder/main.py relative to the binary.
func findEmbedderScript() string {
	exe, _ := os.Executable()
	exeDir := filepath.Dir(exe)

	candidates := []string{
		filepath.Join(exeDir, "../embedder/main.py"),
		filepath.Join(exeDir, "../../embedder/main.py"),
		// Dev layout: binary in go-server/, embedder is sibling
		filepath.Join(exeDir, "../embedder/main.py"),
		// Running from repo root
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

// findPython returns the first available Python 3 interpreter that is 3.9 or higher.
func findPython() string {
	names := []string{"python3", "python"}
	if runtime.GOOS == "windows" {
		names = append(names, "py")
	}
	for _, name := range names {
		if p, err := exec.LookPath(name); err == nil {
			// Validate version >= 3.9
			cmd := exec.Command(p, "-c", "import sys; sys.exit(0 if sys.version_info >= (3,9) else 1)")
			if err := cmd.Run(); err == nil {
				return p
			}
		}
	}
	return ""
}

// checkWilcom pings the Wilcom service to ensure it's responsive.
func checkWilcom() {
	time.Sleep(5 * time.Second) // Give it a moment to boot if running locally

	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get(wilcomSvcURL() + "/health")
	if err != nil || resp.StatusCode != 200 {
		log.Printf("\033[33mWARNING: Wilcom Automation Server is offline or unreachable at %s\033[0m", wilcomSvcURL())
		log.Printf("\033[33m-> Reading .EMB files will be skipped. Ensure the wilcom server is running if needed.\033[0m")
		return
	}
	if resp != nil {
		resp.Body.Close()
	}
	log.Printf("\033[32mSUCCESS: Wilcom Automation Server is connected and ready!\033[0m")
}
