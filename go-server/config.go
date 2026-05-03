// config.go — EMBFinder central configuration
//
// Loads environment variables from .env (current directory) then ../.env
// (so running from go-server/ still picks up the project-root .env).
// All settings have safe built-in defaults; nothing crashes if .env is absent.
package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/joho/godotenv"
)

// appConfig is the single source of truth for all runtime settings.
// Set once in initConfig(), read-only afterwards.
type appConfig struct {
	// Runtime mode: "development" or "production"
	Mode string

	// Go HTTP server bind address and port
	Host string
	Port string

	// Python AI embedder address
	EmbedderHost string
	EmbedderPort string

	// SQLite database file path (absolute)
	DBPath string

	// CLIP model name forwarded to the embedder subprocess
	CLIPModel string
}

// Config is the global singleton. Do not mutate after initConfig() returns.
var Config appConfig

// initConfig loads .env, resolves all settings, checks for port conflicts,
// and tunes the Go runtime GC for a long-running desktop/server process.
func initConfig() {
	// ── 1. Load .env ────────────────────────────────────────────────────────
	// Try both the current working directory and the parent directory so the
	// server works whether launched from go-server/ or the project root.
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	// ── 2. Mode ─────────────────────────────────────────────────────────────
	mode := strings.ToLower(getEnv("MODE", "development"))

	// ── 3. Database path ─────────────────────────────────────────────────────
	// In production, use the platform's standard config directory so the DB
	// survives binary updates without the user having to move files.
	//   Linux:   ~/.config/EMBFinder/embfinder.db
	//   macOS:   ~/Library/Application Support/EMBFinder/embfinder.db
	//   Windows: %APPDATA%\EMBFinder\embfinder.db
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" {
		if mode == "production" {
			configDir, err := os.UserConfigDir()
			if err != nil {
				log.Fatalf("Cannot locate config directory: %v", err)
			}
			appDir := filepath.Join(configDir, "EMBFinder")
			if err := os.MkdirAll(appDir, 0o755); err != nil {
				log.Fatalf("Cannot create config directory %s: %v", appDir, err)
			}
			dbPath = filepath.Join(appDir, "embfinder.db")
		} else {
			// Development: store alongside the binary in ./data/
			if err := os.MkdirAll("data", 0o755); err != nil {
				log.Fatalf("Cannot create data directory: %v", err)
			}
			dbPath = filepath.Join("data", "embfinder.db")
		}
	}

	// ── 4. Assemble config with env-or-default for every field ───────────────
	Config = appConfig{
		Mode:         mode,
		Host:         getEnv("HOST", "127.0.0.1"),
		Port:         getEnv("PORT", "8765"),
		EmbedderHost: getEnv("EMBEDDER_HOST", "127.0.0.1"),
		EmbedderPort: getEnv("EMBEDDER_PORT", "8766"),
		DBPath:       dbPath,
		CLIPModel:    getEnv("CLIP_MODEL", "ViT-L-14"),
	}

	// ── 5. Port conflict resolution ───────────────────────────────────────────
	// If the configured port is already occupied, find the next free one.
	// This prevents a crash when restarting quickly after a previous instance
	// is still in TIME_WAIT.
	if !isPortFree(Config.Host, Config.Port) {
		old := Config.Port
		Config.Port = getFreePort(Config.Host)
		log.Printf("[Config] Port %s busy — using %s instead", old, Config.Port)
	}

	// ── 6. GC tuning ─────────────────────────────────────────────────────────
	// Reduce GC target percentage to keep the in-memory vector index lean.
	// A value of 50 means GC triggers when heap grows 50% past the last collection.
	// Default Go value is 100; lower = more GC runs but smaller resident memory.
	debug.SetGCPercent(50)

	log.Printf("[Config] Mode=%s  Host=%s:%s  DB=%s  CLIP=%s",
		strings.ToUpper(mode), Config.Host, Config.Port, Config.DBPath, Config.CLIPModel)
}

// ── URL helpers ───────────────────────────────────────────────────────────────

// EmbedderURL returns the full base URL of the Python AI embedding service.
// EMBEDDER_URL overrides the host+port combination if set.
func (c *appConfig) EmbedderURL() string {
	if u := os.Getenv("EMBEDDER_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://" + c.EmbedderHost + ":" + c.EmbedderPort
}

// EmbEngineURL returns the full base URL of the EMB rendering service.
func (c *appConfig) EmbEngineURL() string {
	if u := os.Getenv("EMB_ENGINE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return "http://127.0.0.1:8767"
}

// ── Runtime helpers ───────────────────────────────────────────────────────────

// MemoryCleanup forces an immediate GC cycle and returns freed memory to the OS.
// Call after bulk operations (e.g. after a full index rebuild) to trim peak RSS.
func MemoryCleanup() {
	runtime.GC()
	debug.FreeOSMemory()
}

// ── Port utilities ────────────────────────────────────────────────────────────

// isPortFree reports whether host:port is available for binding.
func isPortFree(host, port string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// getFreePort asks the OS to allocate a random free port on host.
// Panics if no port can be allocated (extremely unlikely in normal operation).
func getFreePort(host string) string {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		log.Fatalf("[Config] Cannot find a free port on %s: %v", host, err)
	}
	defer l.Close()
	return fmt.Sprintf("%d", l.Addr().(*net.TCPAddr).Port)
}

// ── Env helper ────────────────────────────────────────────────────────────────

// getEnv reads an environment variable and returns fallback if it is empty or unset.
func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
