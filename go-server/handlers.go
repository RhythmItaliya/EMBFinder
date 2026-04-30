package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// ── Shared persistent HTTP client (connection pooling to Python embedder) ─────
var httpClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 10,
		DisableCompression:  true, // vectors are already compressed
	},
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func errJSON(msg string) map[string]string { return map[string]string{"error": msg} }

// ── GET /api/index/state/stream ───────────────────────────────────────────────
func hIndexStateStream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	for {
		select {
		case <-r.Context().Done():
			return
		default:
			RegisterHeartbeat()
			snap := idxState.snap()
			snap["total_indexed"] = globalIndex.Count()

			snap["counts"] = dbGetFormatCounts()

			b, err := json.Marshal(snap)
			if err != nil {
				return
			}

			fmt.Fprintf(w, "data: %s\n\n", string(b))
			flusher.Flush()

			time.Sleep(1 * time.Second)
		}
	}
}

// ── DELETE /api/index ─────────────────────────────────────────────────────────
// hClear wipes the database and in-memory index.
// It does NOT pause sync — AutoIndexAllDrives will restart on next loop.
func hClear(w http.ResponseWriter, r *http.Request) {
	idxState.mu.Lock()
	idxState.Running = false 
	idxState.Status = "Clearing data..."
	// Reset all counters immediately so UI doesn't stay stuck at 100%
	atomic.StoreInt32(&idxState.Progress, 0)
	atomic.StoreInt32(&idxState.Discovered, 0)
	atomic.StoreInt32(&idxState.Total, 0)
	idxState.ScanDone = false
	idxState.mu.Unlock()

	// Give the loop a moment to notice the stop signal
	time.Sleep(100 * time.Millisecond)

	if err := dbClearAll(); err != nil {
		log.Printf("[DB] Failed to clear designs: %v", err)
		http.Error(w, "database busy, try again", 500)
		return
	}
	globalIndex.Clear()
	
	idxState.mu.Lock()
	idxState.Status = "Idle"
	idxState.mu.Unlock()
	
	RefreshIdxStateCounts()
	writeJSON(w, map[string]interface{}{"status": "cleared"})
}

func hToggleSync(w http.ResponseWriter, r *http.Request) {
	idxState.mu.Lock()
	idxState.UserPaused = !idxState.UserPaused
	paused := idxState.UserPaused
	idxState.mu.Unlock()
	writeJSON(w, map[string]interface{}{"user_paused": paused})
}

// ── POST /api/search ──────────────────────────────────────────────────────────
// Hot path — optimized for minimal latency:
//  1. Read uploaded bytes directly
//  2. Try local CLIP ONNX (zero IPC overhead, ~200ms on CPU)
//  3. Fall back to Python embedder only if CLIP not ready
//  4. Parallel cosine search on in-memory index
func hSearch(w http.ResponseWriter, r *http.Request) {
	if globalIndex.Count() == 0 {
		writeJSON(w, errJSON("no designs indexed — please index your library first"))
		return
	}

	r.ParseMultipartForm(32 << 20)
	file, header, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, errJSON("file required"))
		return
	}
	defer file.Close()
	imgBytes, _ := io.ReadAll(file)

	topK := 100
	if k := r.FormValue("top_k"); k != "" {
		if n, e := strconv.Atoi(k); e == nil && n > 0 {
			topK = n
		}
	}

	// ── Embed query image ─────────────────────────────────────────────────────
	var vec []float32
	vec, err = EmbedImageBytes(imgBytes)
	if err != nil {
		vec, err = callEmbedImage(imgBytes, header.Filename)
		if err != nil {
			writeJSON(w, errJSON("embedding failed: "+err.Error()))
			return
		}
	}

	// ── Search in-memory index ────────────────────────────────────────────────
	// Filter: only return ".emb" files that have a valid visual render.
	// This prevents random photos or corrupted files from cluttering the results.
	results := globalIndex.Search(vec, topK, "emb") 
	
	filtered := make([]SearchResult, 0, len(results))
	for _, r := range results {
		if r.HasPreview {
			filtered = append(filtered, r)
		}
	}

	writeJSON(w, map[string]interface{}{
		"results":       filtered,
		"query":         header.Filename,
		"total_indexed": globalIndex.Count(),
		"clip_local":    clipReady,
	})
}

// ── GET /api/preview/{id} ─────────────────────────────────────────────────────
func hPreview(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	png, err := dbPreview(parts[2])
	if err != nil || len(png) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=604800") // 1 week
	w.Write(png)
}
// ── GET /api/thumbnail/{id} ───────────────────────────────────────────────────
func hThumbnail(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	if len(parts) < 3 {
		http.NotFound(w, r)
		return
	}
	id := parts[2]
	filePath, err := dbGetPath(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}

	// Look for common sidecar images (e.g. s1.EMB -> s1.jpg)
	exts := []string{".jpg", ".jpeg", ".png", ".webp", ".bmp"}
	base := strings.TrimSuffix(filePath, filepath.Ext(filePath))
	
	for _, ext := range exts {
		// Try case-sensitive first
		candidate := base + ext
		if _, err := os.Stat(candidate); err == nil {
			http.ServeFile(w, r, candidate)
			return
		}
		// Try other casing (e.g. .JPG instead of .jpg)
		candidate = base + strings.ToUpper(ext)
		if _, err := os.Stat(candidate); err == nil {
			http.ServeFile(w, r, candidate)
			return
		}
	}

	http.NotFound(w, r)
}

// ── Python embedder calls (used during indexing + search fallback) ────────────

func callEmbedImage(imgBytes []byte, name string) ([]float32, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", name)
	fw.Write(imgBytes)
	w.Close()

	resp, err := httpClient.Post(Config.EmbedderURL()+"/embed", w.FormDataContentType(), &buf)
	if err != nil {
		return nil, fmt.Errorf("embedder unreachable: %w", err)
	}
	defer resp.Body.Close()
	var r struct {
		Embedding []float32 `json:"embedding"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	return r.Embedding, nil
}

func cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func hLatest(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id, file_path, file_name, format, size_kb 
	                      FROM designs WHERE format='emb' ORDER BY indexed_at DESC LIMIT 50`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var out []SearchResult
	for rows.Next() {
		var res SearchResult
		rows.Scan(&res.ID, &res.FilePath, &res.FileName, &res.Format, &res.SizeKB)
		res.HasPreview = true
		out = append(out, res)
	}
	writeJSON(w, out)
}
