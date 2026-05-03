package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
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

// ── DELETE /api/index (also accepts GET for browser compat) ──────────────────────
// hClear atomically stops indexing, wipes the DB, then resets all counters.
// Algorithm:
//   1. Acquire state lock, set Running=false, Status="Clearing", reset counters.
//   2. Sleep 150ms — gives the background indexer goroutine one iteration cycle
//      to notice Running=false and return cleanly (prevents a race where the goroutine
//      tries to dbUpsert into a just-truncated table).
//   3. Truncate SQLite via a single DELETE (O(1) in WAL mode).
//   4. Clear the in-memory index (O(N) slice nil).
//   5. Re-read format counts from DB (now all zero) and broadcast via SSE.
func hClear(w http.ResponseWriter, r *http.Request) {
	// Step 1 — Signal the indexer to stop
	idxState.mu.Lock()
	idxState.Running    = false
	idxState.UserPaused = true  // Keep paused so AutoIndex won’t restart immediately
	idxState.Status     = "Clearing data..."
	atomic.StoreInt32(&idxState.Progress,   0)
	atomic.StoreInt32(&idxState.Discovered, 0)
	atomic.StoreInt32(&idxState.Total,      0)
	idxState.ScanDone = false
	idxState.mu.Unlock()

	// Step 2 — Let the indexer goroutine notice the stop flag
	time.Sleep(200 * time.Millisecond)

	// Step 3 — Wipe database
	if err := dbClearAll(); err != nil {
		log.Printf("[DB] Failed to clear designs: %v", err)
		// Un-pause so user can try again
		idxState.mu.Lock()
		idxState.UserPaused = false
		idxState.Status     = "Idle"
		idxState.mu.Unlock()
		http.Error(w, "database busy, try again", 500)
		return
	}

	// Step 4 — Wipe memory index
	globalIndex.Clear()

	// Step 5 — Reset state and notify SSE clients
	idxState.mu.Lock()
	idxState.UserPaused = false   // Allow auto-sync to restart on next cycle
	idxState.Status     = "Idle"
	idxState.mu.Unlock()

	RefreshIdxStateCounts()
	log.Printf("[Clear] Database and memory index wiped successfully")
	writeJSON(w, map[string]interface{}{"status": "cleared"})
}

// hToggleSync pauses or resumes the background auto-indexing loop.
// When resuming (un-pausing), it sends a signal to the triggerScan channel
// so the loop wakes up immediately instead of waiting up to 5 minutes.
func hToggleSync(w http.ResponseWriter, r *http.Request) {
	idxState.mu.Lock()
	idxState.UserPaused = !idxState.UserPaused
	paused := idxState.UserPaused
	if !paused {
		idxState.Status = "Resuming sync..."
	} else {
		idxState.Status = "Sync paused"
	}
	idxState.mu.Unlock()

	// Wake the background loop immediately when resuming
	if !paused {
		select {
		case triggerScan <- struct{}{}:
		default:
		}
	}

	writeJSON(w, map[string]interface{}{"user_paused": paused})
}

// ── POST /api/search ──────────────────────────────────────────────────────────
// Hot path — optimized for minimal latency:
//  1. Read uploaded bytes directly
//  2. If .emb file: route through emb-engine for a visual preview first
//  3. Try local CLIP ONNX (zero IPC overhead, ~200ms on CPU)
//  4. Fall back to Python embedder only if CLIP not ready
//  5. Parallel cosine search on in-memory index
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
	rawBytes, _ := io.ReadAll(file)

	topK := 100
	if k := r.FormValue("top_k"); k != "" {
		if n, e := strconv.Atoi(k); e == nil && n > 0 {
			topK = n
		}
	}

	// ── Detect file type and prepare image bytes for embedding ────────────────
	ext := strings.ToLower(filepath.Ext(header.Filename))
	imgBytes := rawBytes

	if ext == ".emb" {
		// .EMB file: write to a temp file so emb-engine can render it
		tmp, tmpErr := os.CreateTemp("", "embsearch-*.emb")
		if tmpErr == nil {
			tmp.Write(rawBytes)
			tmp.Close()
			defer os.Remove(tmp.Name())

			png := callEmbEnginePreview(tmp.Name())
			if png != nil {
				imgBytes = png
				ext = ".png"
			} else {
				writeJSON(w, errJSON("could not render .EMB file — ensure EmbEngine is running"))
				return
			}
		} else {
			writeJSON(w, errJSON("could not create temp file for .EMB"))
			return
		}
	}

	// ── Embed query image ─────────────────────────────────────────────────────
	var vecs [][]float32
	
	// Force python embedder so we get multi-crop + OpenAI weights
	vecs, err = callEmbedImageMulti(imgBytes, header.Filename)
	if err != nil {
		writeJSON(w, errJSON("embedding failed: "+err.Error()))
		return
	}

	// ── Search in-memory index ────────────────────────────────────────────────
	// Filter: only return ".emb" files that have a valid visual render.
	// We aggregate scores across all patches/crops using a majority vote strategy.
	
	scoreMap := make(map[string]float64)
	bestResult := make(map[string]SearchResult)
	
	for _, v := range vecs {
		// Search deep (1000) so we can accurately sum scores across all crops before truncating
		results := globalIndex.Search(v, 1000, "emb")
		for _, r := range results {
			if r.HasPreview {
				if r.Score > scoreMap[r.ID] {
					scoreMap[r.ID] = r.Score
					bestResult[r.ID] = r
				}
			}
		}
	}
	
	filtered := make([]SearchResult, 0, len(bestResult))
	for id, r := range bestResult {
		r.Score = scoreMap[id] // Updated aggregated score
		filtered = append(filtered, r)
	}
	
	// Sort aggregated results by score descending
	sort.Slice(filtered, func(i, j int) bool {
		return filtered[i].Score > filtered[j].Score
	})
	
	// Truncate to topK
	if len(filtered) > topK {
		filtered = filtered[:topK]
	}

	writeJSON(w, map[string]interface{}{
		"results":       filtered,
		"query":         header.Filename,
		"total_indexed": globalIndex.Count(),
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
	// Use dbThumbnail: returns sidecar garment photo if available, else render PNG
	img, err := dbThumbnail(parts[2])
	if err != nil || len(img) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=604800")
	w.Write(img)
}

// ── Python embedder calls (used during indexing + search fallback) ────────────


func callEmbedImageMulti(imgBytes []byte, name string) ([][]float32, error) {
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
		Embeddings [][]float32 `json:"embeddings"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if len(r.Embeddings) == 0 {
		return nil, fmt.Errorf("no embeddings returned")
	}
	return r.Embeddings, nil
}

// callEmbedAugmented calls the /embed-augmented endpoint, which returns embeddings
// for 6 augmented views of the image (flip, ±5° rotations, ±15% brightness).
// Used for sidecar photo indexing to create a variation-invariant representation.
func callEmbedAugmented(imgBytes []byte, name string) ([][]float32, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", name)
	fw.Write(imgBytes)
	w.Close()

	resp, err := httpClient.Post(Config.EmbedderURL()+"/embed-augmented", w.FormDataContentType(), &buf)
	if err != nil {
		// Fall back to regular multi-crop if augmented endpoint not available
		return callEmbedImageMulti(imgBytes, name)
	}
	defer resp.Body.Close()
	var r struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if len(r.Embeddings) == 0 {
		return callEmbedImageMulti(imgBytes, name)
	}
	return r.Embeddings, nil
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

// ── GET /api/browse ───────────────────────────────────────────────────────────
// Paginated EMB library browser with optional search filter.
func hBrowseEMB(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := 1
	pageSize := 48
	filter := strings.TrimSpace(q.Get("q"))

	if p, err := strconv.Atoi(q.Get("page")); err == nil && p > 0 {
		page = p
	}
	if ps, err := strconv.Atoi(q.Get("page_size")); err == nil && ps > 0 && ps <= 200 {
		pageSize = ps
	}
	offset := (page - 1) * pageSize

	var rows *sql.Rows
	var countRow *sql.Row
	var err error
	var total int

	if filter != "" {
		like := "%" + filter + "%"
		countRow = db.QueryRow(`SELECT COUNT(*) FROM designs WHERE format='emb' AND file_name LIKE ?`, like)
		countRow.Scan(&total)
		rows, err = db.Query(
			`SELECT id, file_path, file_name, format, size_kb, (preview_png IS NOT NULL) 
			 FROM designs WHERE format='emb' AND file_name LIKE ? 
			 ORDER BY file_name LIMIT ? OFFSET ?`, like, pageSize, offset)
	} else {
		countRow = db.QueryRow(`SELECT COUNT(*) FROM designs WHERE format='emb'`)
		countRow.Scan(&total)
		rows, err = db.Query(
			`SELECT id, file_path, file_name, format, size_kb, (preview_png IS NOT NULL) 
			 FROM designs WHERE format='emb' 
			 ORDER BY indexed_at DESC LIMIT ? OFFSET ?`, pageSize, offset)
	}
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var items []SearchResult
	for rows.Next() {
		var res SearchResult
		rows.Scan(&res.ID, &res.FilePath, &res.FileName, &res.Format, &res.SizeKB, &res.HasPreview)
		items = append(items, res)
	}
	writeJSON(w, map[string]interface{}{
		"items":     items,
		"total":     total,
		"page":      page,
		"page_size": pageSize,
		"pages":     (total + pageSize - 1) / pageSize,
	})
}

// ── POST /api/open-file ───────────────────────────────────────────────────────
// Opens the file's containing folder in the OS file manager.
func hOpenFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}

	filePath := body.Path
	if filePath == "" && body.ID != "" {
		var err error
		filePath, err = dbGetPath(body.ID)
		if err != nil || filePath == "" {
			writeJSON(w, errJSON("design not found"))
			return
		}
	}

	if filePath == "" {
		writeJSON(w, errJSON("no path provided"))
		return
	}

	dir := filepath.Dir(filePath)
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// Explorer highlights the specific file
		cmd = exec.Command("explorer", "/select,"+filepath.ToSlash(filePath))
	case "darwin":
		cmd = exec.Command("open", "-R", filePath)
	default: // linux
		cmd = exec.Command("xdg-open", dir)
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[OpenFile] Failed: %v", err)
		writeJSON(w, errJSON("could not open folder: "+err.Error()))
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "path": dir})
}

// ── POST /api/emb-info ────────────────────────────────────────────────────────
// Returns stitch/color/trim metadata for an indexed EMB by ID or path.
// Proxies to emb-engine /info endpoint.
func hEmbInfo(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID   string `json:"id"`
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}

	filePath := body.Path
	if filePath == "" && body.ID != "" {
		var err error
		filePath, err = dbGetPath(body.ID)
		if err != nil || filePath == "" {
			writeJSON(w, errJSON("design not found"))
			return
		}
	}
	if filePath == "" {
		writeJSON(w, errJSON("no path or id provided"))
		return
	}

	// Proxy to emb-engine /info
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("file_path", filePath)
	mw.Close()

	resp, err := httpClient.Post(Config.EmbEngineURL()+"/info", mw.FormDataContentType(), &buf)
	if err != nil {
		// emb-engine offline: return best-effort info from DB
		var sizeKB float64
		db.QueryRow("SELECT size_kb FROM designs WHERE file_path=?", filePath).Scan(&sizeKB)
		writeJSON(w, map[string]interface{}{
			"file_name":    filepath.Base(filePath),
			"format":       strings.TrimPrefix(strings.ToLower(filepath.Ext(filePath)), "."),
			"size_kb":      sizeKB,
			"engine_ready": false,
			"error":        "emb-engine offline",
		})
		return
	}
	defer resp.Body.Close()
	// Stream the emb-engine JSON response directly to the client
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

