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

// appConfig holds the central application settings structure.
type appConfig struct {
	Mode         string
	Host         string
	Port         string
	EmbedderHost string
	EmbedderPort string
	DBPath       string
	MaxWorkers   int
}

var Config appConfig

// initConfig initializes configuration defaults, resolves port conflicts, and tunes GC.
func initConfig() {
	// 1. Try to load .env from current or parent directories
	_ = godotenv.Load(".env")
	_ = godotenv.Load("../.env")

	// 2. Setup Mode (development by default)
	mode := strings.ToLower(os.Getenv("MODE"))
	if mode == "" {
		mode = "development"
	}

	// 3. Determine safe Database Path
	dbPath := os.Getenv("DB_PATH")
	if dbPath == "" || dbPath == "/data/embfinder.db" { // override old docker path
		if mode == "production" {
			configDir, _ := os.UserConfigDir()
			appDir := filepath.Join(configDir, "EMBFinder")
			os.MkdirAll(appDir, 0755)
			dbPath = filepath.Join(appDir, "embfinder.db")
		} else {
			os.MkdirAll("data", 0755)
			dbPath = filepath.Join("data", "embfinder.db")
		}
	}

	Config = appConfig{
		Mode:         mode,
		Host:         "127.0.0.1",
		Port:         "8765",
		EmbedderHost: "127.0.0.1",
		EmbedderPort: "8766",
		DBPath:       dbPath,
		MaxWorkers:   runtime.NumCPU(),
	}

	if !isPortFree(Config.Host, Config.Port) {
		Config.Port = getFreePort(Config.Host)
	}

	if p := os.Getenv("PORT"); p != "" {
		Config.Port = p
	}
	if p := os.Getenv("EMBEDDER_PORT"); p != "" {
		Config.EmbedderPort = p
	}

	debug.SetGCPercent(50)
	log.Printf("Booting in %s mode...", strings.ToUpper(mode))
}

// EmbedderURL constructs the full Python AI engine HTTP URL.
func (c *appConfig) EmbedderURL() string {
	if u := os.Getenv("EMBEDDER_URL"); u != "" {
		return u
	}
	return "http://" + c.EmbedderHost + ":" + c.EmbedderPort
}

// EmbEngineURL constructs the URL for the Embroidery Render Engine.
func (c *appConfig) EmbEngineURL() string {
	if u := os.Getenv("EMB_ENGINE_URL"); u != "" {
		return u
	}
	return "http://127.0.0.1:8767"
}

// MemoryCleanup triggers garbage collection to aggressively return RAM to the OS.
func MemoryCleanup() {
	runtime.GC()
	debug.FreeOSMemory()
}

// isPortFree checks if a specific network interface and port are available.
func isPortFree(host, port string) bool {
	l, err := net.Listen("tcp", net.JoinHostPort(host, port))
	if err != nil {
		return false
	}
	l.Close()
	return true
}

// getFreePort asks the OS for a random open port on the specified host.
func getFreePort(host string) string {
	l, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		log.Fatalf("Could not find a free port: %v", err)
	}
	defer l.Close()
	return fmt.Sprintf("%d", l.Addr().(*net.TCPAddr).Port)
}
