package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// ── GET /api/status ───────────────────────────────────────────────────────────
func hStatus(w http.ResponseWriter, r *http.Request) {
	embedOK := clipReady // prefer local CLIP
	if !embedOK {
		if resp, err := httpClient.Get(Config.EmbedderURL() + "/health"); err == nil {
			embedOK = resp.StatusCode == 200
			resp.Body.Close()
		}
	}
	embEngineOK := false
	if resp, err := httpClient.Get(embEngineSvcURL() + "/health"); err == nil {
		embEngineOK = resp.StatusCode == 200
		resp.Body.Close()
	}
	writeJSON(w, map[string]interface{}{
		"status":         "ok",
		"total_indexed":  globalIndex.Count(),
		"embedder_ready": embedOK,
		"clip_local":     clipReady,
		"emb-engine_ready":   embEngineOK,
		"indexing":       idxState.Running,
		"auto_paths":     autoLibPaths(),
	})
}

// ── GET /api/scan?folder=... ──────────────────────────────────────────────────
func hScan(w http.ResponseWriter, r *http.Request) {
	folder := r.URL.Query().Get("folder")
	if folder == "" {
		writeJSON(w, errJSON("folder required"))
		return
	}
	if _, err := os.Stat(folder); err != nil {
		writeJSON(w, errJSON("not found: "+folder))
		return
	}
	files := findFiles(folder)
	counts := map[string]int{}
	for _, f := range files {
		ext := strings.TrimPrefix(strings.ToLower(filepath.Ext(f)), ".")
		counts[ext]++
	}
	writeJSON(w, map[string]interface{}{
		"folder": folder, "total_files": len(files), "formats": counts,
	})
}

// ── POST /api/index ───────────────────────────────────────────────────────────
func hIndex(w http.ResponseWriter, r *http.Request) {
	if idxState.Running {
		http.Error(w, "already indexing", 409)
		return
	}
	r.ParseMultipartForm(1 << 20)
	folder := r.FormValue("folder")
	force := r.FormValue("force") == "true"
	if folder == "" {
		writeJSON(w, errJSON("folder required"))
		return
	}
	if _, err := os.Stat(folder); err != nil {
		writeJSON(w, errJSON("folder not found"))
		return
	}
	StartIndexing(folder, force)
	writeJSON(w, map[string]string{"status": "started"})
}

// ── GET /api/index/state ──────────────────────────────────────────────────────
func hIndexState(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, idxState.snap())
}

// ── DELETE /api/index ─────────────────────────────────────────────────────────
func hClear(w http.ResponseWriter, r *http.Request) {
	if idxState.Running {
		http.Error(w, "cannot clear while indexing", 409)
		return
	}
	n := dbClear()
	globalIndex.Clear()
	writeJSON(w, map[string]interface{}{"cleared": n})
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

	topK := 24
	if k := r.FormValue("top_k"); k != "" {
		if n, e := strconv.Atoi(k); e == nil && n > 0 {
			topK = n
		}
	}

	// ── Embed query image ─────────────────────────────────────────────────────
	var vec []float32

	// Strategy 1: local CLIP ONNX (fastest, no IPC)
	vec, err = EmbedImageBytes(imgBytes)
	if err != nil {
		// Strategy 2: Python embedder (fallback, ~5ms extra over local)
		vec, err = callEmbedImage(imgBytes, header.Filename)
		if err != nil {
			writeJSON(w, errJSON("embedding failed: "+err.Error()))
			return
		}
	}

	// ── Search in-memory index ────────────────────────────────────────────────
	results := globalIndex.Search(vec, topK)

	writeJSON(w, map[string]interface{}{
		"results":       results,
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

// ── Python embedder calls (used during indexing + search fallback) ────────────

func callEmbedImage(imgBytes []byte, name string) ([]float32, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", name)
	fw.Write(imgBytes)
	w.Close()

	resp, err := httpClient.Post(Config.EmbedderURL()+"/embed-image", w.FormDataContentType(), &buf)
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
