package main

import (
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
)

// appConfig holds the central application settings structure.
type appConfig struct {
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

	Config = appConfig{
		Host:         "127.0.0.1",
		Port:         "8765",
		EmbedderHost: "127.0.0.1",
		EmbedderPort: "8766",
		DBPath:       filepath.Join("data", "embfinder.db"),
		MaxWorkers:   runtime.NumCPU(),
	}

	if !isPortFree(Config.Host, Config.Port) {
		Config.Port = getFreePort(Config.Host)
	}
	if !isPortFree(Config.EmbedderHost, Config.EmbedderPort) {
		Config.EmbedderPort = getFreePort(Config.EmbedderHost)
	}

	if p := os.Getenv("PORT"); p != "" {
		Config.Port = p
	}
	if p := os.Getenv("DB_PATH"); p != "" {
		Config.DBPath = p
	}
	if p := os.Getenv("EMBEDDER_PORT"); p != "" {
		Config.EmbedderPort = p
	}

	debug.SetGCPercent(50)
}

// EmbedderURL constructs the full Python AI engine HTTP URL.
func (c *appConfig) EmbedderURL() string {
	if u := os.Getenv("EMBEDDER_URL"); u != "" {
		return u
	}
	return "http://" + c.EmbedderHost + ":" + c.EmbedderPort
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
