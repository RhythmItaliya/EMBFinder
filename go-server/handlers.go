package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
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
	"sync"
	"sync/atomic"
	"time"
)

var (
	embedderMu    sync.Mutex
	searchWaiters int32
	indexQueueMu  sync.Mutex
	indexQueue    []indexRequest
	queueStarted  bool
)

type indexRequest struct {
	paths []string
	force bool
}

func enqueueIndex(paths []string, force bool) {
	if len(paths) == 0 {
		return
	}
	indexQueueMu.Lock()
	indexQueue = append(indexQueue, indexRequest{paths: paths, force: force})
	if !queueStarted {
		queueStarted = true
		go runIndexQueue()
	}
	indexQueueMu.Unlock()
}

func indexQueueLen() int {
	indexQueueMu.Lock()
	defer indexQueueMu.Unlock()
	return len(indexQueue)
}

func clearIndexQueue() int {
	indexQueueMu.Lock()
	defer indexQueueMu.Unlock()
	n := len(indexQueue)
	indexQueue = nil
	queueStarted = false
	return n
}

func removeIndexQueuePath(path string) int {
	indexQueueMu.Lock()
	defer indexQueueMu.Unlock()
	removed := 0
	filteredQueue := make([]indexRequest, 0, len(indexQueue))
	for _, req := range indexQueue {
		filteredPaths := make([]string, 0, len(req.paths))
		for _, p := range req.paths {
			if p == path {
				removed++
				continue
			}
			filteredPaths = append(filteredPaths, p)
		}
		if len(filteredPaths) > 0 {
			req.paths = filteredPaths
			filteredQueue = append(filteredQueue, req)
		}
	}
	indexQueue = filteredQueue
	if len(indexQueue) == 0 {
		queueStarted = false
	}
	return removed
}

func runIndexQueue() {
	for {
		indexQueueMu.Lock()
		if len(indexQueue) == 0 {
			queueStarted = false
			indexQueueMu.Unlock()
			return
		}
		req := indexQueue[0]
		indexQueueMu.Unlock()

		idxState.mu.RLock()
		running := idxState.Running
		idxState.mu.RUnlock()
		if running {
			time.Sleep(500 * time.Millisecond)
			continue
		}

		filtered := make([]string, 0, len(req.paths))
		for _, p := range req.paths {
			if isSelectedRoot(p) || isSelectedPath(p) {
				filtered = append(filtered, p)
			}
		}
		if len(filtered) > 0 {
			StartIndexingMulti(filtered, req.force)
		}

		indexQueueMu.Lock()
		if len(indexQueue) > 0 {
			indexQueue = indexQueue[1:]
		}
		indexQueueMu.Unlock()
	}
}

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

// ── POST /api/index/start ────────────────────────────────────────────────────
// Starts indexing for specific paths, or all known folders if no paths provided.
func hIndexStart(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	idxState.mu.RLock()
	running := idxState.Running
	idxState.mu.RUnlock()

	var body struct {
		Paths []string `json:"paths"`
		Force bool     `json:"force"`
	}
	if r.Method == http.MethodPost && r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	seen := make(map[string]bool)
	paths := make([]string, 0, len(body.Paths))
	for _, p := range body.Paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			paths = append(paths, p)
		}
	}

	// If no explicit paths were provided, index selected roots only.
	if len(paths) == 0 {
		for _, p := range getSelectedDriveRoots() {
			if p == "" || seen[p] {
				continue
			}
			if st, err := os.Stat(p); err == nil && st.IsDir() {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}

	if len(paths) == 0 {
		writeJSON(w, map[string]string{
			"status": "no_paths",
			"msg":    "No valid folders selected. Add a folder first.",
		})
		return
	}

	enqueueIndex(paths, body.Force)
	if running || indexQueueLen() > 0 {
		writeJSON(w, map[string]interface{}{"status": "queued", "paths": paths, "count": len(paths)})
		return
	}
	writeJSON(w, map[string]interface{}{"status": "started", "paths": paths, "count": len(paths)})
}

// hDriveSelect persists selected scan roots from UI checkboxes.
func hDriveSelect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}
	valid := make([]string, 0, len(body.Paths))
	seen := make(map[string]bool)
	for _, p := range body.Paths {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			continue
		}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			seen[p] = true
			valid = append(valid, p)
		}
	}
	setSelectedDriveRoots(valid)
	writeJSON(w, map[string]interface{}{"status": "ok", "selected": valid})
}

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
func hClear(w http.ResponseWriter, r *http.Request) {
	// Step 1 — Signal the indexer to stop
	idxState.mu.Lock()
	idxState.Running = false
	idxState.UserPaused = true
	idxState.Status = "Clearing data..."
	idxState.mu.Unlock()

	// Step 2 — Wait for indexer to actually stop (max 2s)
	for i := 0; i < 20; i++ {
		idxState.mu.RLock()
		running := idxState.Running
		idxState.mu.RUnlock()
		if !running {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Step 3 — Wipe database
	if err := dbClearAll(); err != nil {
		log.Printf("[DB] Failed to clear designs: %v", err)
		idxState.mu.Lock()
		idxState.UserPaused = false
		idxState.Status = "Idle"
		idxState.mu.Unlock()
		http.Error(w, "database busy, try again", 500)
		return
	}

	// Reset counters AFTER wiping
	atomic.StoreInt32(&idxState.Progress, 0)
	atomic.StoreInt32(&idxState.Discovered, 0)
	atomic.StoreInt32(&idxState.Total, 0)
	idxState.mu.Lock()
	idxState.ScanDone = false
	idxState.mu.Unlock()

	// Step 4 — Wipe memory index
	globalIndex.Clear()

	// Step 5 — Reset state and notify SSE clients
	idxState.mu.Lock()
	idxState.UserPaused = false // Allow auto-sync to restart on next cycle
	idxState.Status = "Idle"
	idxState.mu.Unlock()

	// Reset all folder indexed counts to 0 (designs table is empty)
	dbRecalcAllFolderCounts()
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

// hStopAllIndexing force-stops active indexing and clears queued jobs.
func hStopAllIndexing(w http.ResponseWriter, r *http.Request) {
	idxState.mu.Lock()
	wasRunning := idxState.Running
	idxState.Running = false
	idxState.UserPaused = true
	idxState.Status = "Stopped by user"
	idxState.ScanDone = true
	idxState.CurrentRoot = ""
	idxState.CurrentFile = ""
	idxState.mu.Unlock()

	cleared := clearIndexQueue()
	dbStopAllFolders()
	RefreshIdxStateCounts()
	writeJSON(w, map[string]interface{}{
		"status":        "stopped",
		"was_running":   wasRunning,
		"queue_cleared": cleared,
	})
}

// hFolderList returns all indexed folders and their stats.
func hFolderList(w http.ResponseWriter, r *http.Request) {
	folders, err := dbLoadFolders()
	if err != nil {
		writeJSON(w, errJSON(err.Error()))
		return
	}
	if folders == nil {
		folders = []FolderStats{}
	}
	writeJSON(w, folders)
}

// ── GET /api/perf ───────────────────────────────────────────────────────────
// Returns current system metrics and adaptive performance state.
func hPerf(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, getPerfMetrics())
}

// ── POST /api/perf/mode ─────────────────────────────────────────────────────
// Sets performance mode: auto | performance | balanced | power_saver
func hPerfMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}
	mode := parsePerfMode(body.Mode)
	setPerfMode(mode)
	updatePerf()
	writeJSON(w, map[string]string{"status": "ok", "mode": string(mode)})
}

// hFolderRescan triggers an immediate rescan of a specific folder.
func hFolderRescan(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}
	if body.Path == "" {
		writeJSON(w, errJSON("path required"))
		return
	}
	if st, err := os.Stat(body.Path); err != nil || !st.IsDir() {
		writeJSON(w, errJSON("path not found"))
		return
	}

	added := addSelectedDriveRoot(body.Path)

	enqueueIndex([]string{body.Path}, true)

	writeJSON(w, map[string]interface{}{"status": "queued", "path": body.Path, "selected": added})
}

// hFolderStop stops an in-progress scan or removes a queued folder scan.
func hFolderStop(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, errJSON("invalid body"))
		return
	}
	if body.Path == "" {
		writeJSON(w, errJSON("path required"))
		return
	}

	removed := removeIndexQueuePath(body.Path)
	stopped := false
	cleanPath := filepath.Clean(body.Path)

	idxState.mu.Lock()
	currentRoot := filepath.Clean(idxState.CurrentRoot)
	if idxState.Running && (currentRoot == cleanPath || strings.HasPrefix(currentRoot, cleanPath+string(os.PathSeparator))) {
		idxState.Running = false
		idxState.ScanDone = true
		idxState.UserPaused = false
		idxState.Status = "Stopped " + filepath.Base(cleanPath)
		stopped = true
	}
	idxState.mu.Unlock()

	if stopped {
		name := filepath.Base(cleanPath)
		if name == "" || name == "." || name == string(os.PathSeparator) {
			name = cleanPath
		}
		dbSaveFolder(FolderStats{
			Path:         cleanPath,
			Name:         name,
			TotalFiles:   int(dbGetTotalForFolder(cleanPath)),
			IndexedFiles: dbCountForPath(cleanPath),
			Status:       "Stopped",
			LastScan:     time.Now().Unix(),
		})
	} else if removed > 0 {
		dbSetFolderStatus(cleanPath, "Stopped")
	}

	writeJSON(w, map[string]interface{}{
		"status":          "ok",
		"removed":         removed,
		"running_stopped": stopped,
	})
}

// ── GET /api/drives ──────────────────────────────────────────────────────────
// returns all detected drives/volumes with their EMB counts.
func hDriveList(w http.ResponseWriter, r *http.Request) {
	all := autoLibPaths()

	type driveInfo struct {
		Path     string `json:"path"`
		Label    string `json:"label"`
		FSType   string `json:"fs_type,omitempty"`
		Usable   bool   `json:"usable"`
		Indexed  int    `json:"indexed"`
		Selected bool   `json:"selected"`
	}

	infos := make([]driveInfo, 0, len(all))
	for _, d := range all {
		var n int
		// Count designs under this drive prefix
		db.QueryRow("SELECT COUNT(*) FROM designs WHERE file_path LIKE ?", d.Path+"/%").Scan(&n)

		infos = append(infos, driveInfo{
			Path:     d.Path,
			Label:    d.Label,
			FSType:   d.FSType,
			Usable:   usableDrive(d),
			Indexed:  n,
			Selected: isSelectedRoot(d.Path),
		})
	}
	writeJSON(w, map[string]interface{}{"drives": infos})
}

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
	atomic.AddInt32(&searchWaiters, 1)
	embedderMu.Lock()
	defer embedderMu.Unlock()
	defer atomic.AddInt32(&searchWaiters, -1)

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
	for atomic.LoadInt32(&searchWaiters) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	embedderMu.Lock()
	defer embedderMu.Unlock()

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

	// If it's a directory, open it directly. If it's a file, open its parent.
	dir := filePath
	st, err := os.Stat(filePath)
	if err == nil && !st.IsDir() {
		dir = filepath.Dir(filePath)
	}

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "windows":
		if err == nil && !st.IsDir() {
			// Explorer highlights the specific file
			cmd = exec.Command("explorer", "/select,"+filepath.ToSlash(filePath))
		} else {
			cmd = exec.Command("explorer", filepath.ToSlash(dir))
		}
	case "darwin":
		if err == nil && !st.IsDir() {
			cmd = exec.Command("open", "-R", filePath)
		} else {
			cmd = exec.Command("open", dir)
		}
	default: // linux
		highlightPath := ""
		if err == nil && !st.IsDir() {
			highlightPath = filePath
		}
		if err := openFolderLinux(dir, highlightPath); err != nil {
			log.Printf("[OpenFile] Failed: %v", err)
			writeJSON(w, errJSON("could not open folder: "+err.Error()))
			return
		}
		writeJSON(w, map[string]string{"status": "ok", "path": dir})
		return
	}
	if err := cmd.Start(); err != nil {
		log.Printf("[OpenFile] Failed: %v", err)
		writeJSON(w, errJSON("could not open folder: "+err.Error()))
		return
	}
	writeJSON(w, map[string]string{"status": "ok", "path": dir})
}

// openFolderLinux opens dir in a file manager, trying to SELECT filePath if non-empty.
func openFolderLinux(dir, filePath string) error {
	// Managers that support file-selection / highlighting
	if filePath != "" {
		// 1. Try universal D-Bus standard first (works on Ubuntu/Gnome, KDE, Mint, etc.)
		uri := "file://" + filePath
		if err := exec.Command("dbus-send", "--session", "--dest=org.freedesktop.FileManager1", 
			"--type=method_call", "/org/freedesktop/FileManager1", 
			"org.freedesktop.FileManager1.ShowItems", "array:string:"+uri, "string:").Start(); err == nil {
			return nil
		}
		
		// 2. Direct CLI fallbacks
		if _, err := exec.LookPath("nautilus"); err == nil {
			if e := exec.Command("nautilus", "--select", filePath).Start(); e == nil {
				return nil
			}
		}
		if _, err := exec.LookPath("nemo"); err == nil {
			if e := exec.Command("nemo", filePath).Start(); e == nil {
				return nil
			}
		}
		if _, err := exec.LookPath("dolphin"); err == nil {
			if e := exec.Command("dolphin", "--select", filePath).Start(); e == nil {
				return nil
			}
		}
	}
	// Managers that just open the folder
	candidates := [][]string{
		{"gio", "open", dir},
		{"thunar", dir},
		{"pcmanfm", dir},
		{"caja", dir},
		{"xdg-open", dir},
	}
	for _, c := range candidates {
		if _, err := exec.LookPath(c[0]); err == nil {
			return exec.Command(c[0], c[1:]...).Start()
		}
	}
	return errors.New("no file manager found")
}

// ── GET /api/pick-folder ─────────────────────────────────────────────────────
// Opens a native OS directory picker and returns the selected path.
func hPickFolder(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var path string
	var err error

	switch runtime.GOOS {
	case "windows":
		// Windows: use PowerShell to open a folder picker
		script := `
$app = New-Object -ComObject Shell.Application
$folder = $app.BrowseForFolder(0, "Select Embroidery Folder", 0, 0)
if ($folder) { $folder.Self.Path }
`
		out, e := exec.CommandContext(ctx, "powershell", "-Command", script).Output()
		if e != nil {
			err = e
		} else {
			path = strings.TrimSpace(string(out))
		}
	case "darwin":
		// macOS: use osascript
		script := `choose folder with prompt "Select Embroidery Folder" as string`
		out, e := exec.CommandContext(ctx, "osascript", "-e", script).Output()
		if e != nil {
			err = e
		} else {
			// Convert "Macintosh HD:Users:..." to POSIX path
			p := strings.TrimSpace(string(out))
			if strings.HasSuffix(p, ":") {
				p = p[:len(p)-1]
			}
			path = "/" + strings.ReplaceAll(p, ":", "/")
		}
	default: // linux
		path, err = pickFolderLinux(ctx)
	}

	if err != nil {
		log.Printf("[PickFolder] Failed or canceled: %v", err)
		writeJSON(w, map[string]string{"status": "canceled"})
		return
	}
	if path == "" {
		writeJSON(w, map[string]string{"status": "canceled"})
		return
	}

	writeJSON(w, map[string]string{"status": "ok", "path": path})
}

func pickFolderLinux(ctx context.Context) (string, error) {
	// Zenity is most common
	if _, err := exec.LookPath("zenity"); err == nil {
		out, err := exec.CommandContext(ctx, "zenity", "--file-selection", "--directory", "--title=Select Embroidery Folder").Output()
		return strings.TrimSpace(string(out)), err
	}
	// Kdialog for KDE
	if _, err := exec.LookPath("kdialog"); err == nil {
		out, err := exec.CommandContext(ctx, "kdialog", "--getexistingdirectory", ".", "--title", "Select Embroidery Folder").Output()
		return strings.TrimSpace(string(out)), err
	}
	return "", errors.New("no native picker (zenity/kdialog) found")
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

// ── POST /api/open-truesizer ──────────────────────────────────────────────────
// Opens the EMB file in the TrueSizer GUI via the emb-engine /open endpoint.
// Non-blocking: TrueSizer launches in the background for interactive inspection.
func hOpenTrueSizer(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
		ID   string `json:"id"`
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

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("file_path", filePath)
	mw.Close()

	resp, err := httpClient.Post(Config.EmbEngineURL()+"/open", mw.FormDataContentType(), &buf)
	if err != nil {
		writeJSON(w, errJSON("emb-engine offline: "+err.Error()))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	io.Copy(w, resp.Body)
}

// ── POST /api/db/backup ───────────────────────────────────────────────────────
// Creates a timestamped backup copy of the SQLite DB file before any cleanup.
func hDbBackup(w http.ResponseWriter, r *http.Request) {
	src := Config.DBPath
	ts := time.Now().Format("2006-01-02_15-04-05")
	dst := src + ".backup-" + ts

	in, err := os.Open(src)
	if err != nil {
		writeJSON(w, errJSON("cannot open DB: "+err.Error()))
		return
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		writeJSON(w, errJSON("cannot create backup file: "+err.Error()))
		return
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		os.Remove(dst)
		writeJSON(w, errJSON("backup copy failed: "+err.Error()))
		return
	}
	log.Printf("[Backup] DB backed up → %s", dst)
	writeJSON(w, map[string]string{"status": "ok", "backup_path": dst})
}

// ── POST /api/db/repair ───────────────────────────────────────────────────────
// Deduplicates designs by file_path and recalculates all folder counts.
func hDbRepair(w http.ResponseWriter, r *http.Request) {
	before := dbCount()
	dbDeduplicatePaths()
	dbRecalcAllFolderCounts()
	dbFixCompletedStatus()
	after := dbCount()
	RefreshIdxStateCounts()

	removed := before - after
	log.Printf("[Repair] Dedup complete: %d → %d designs (%d removed)", before, after, removed)
	writeJSON(w, map[string]interface{}{
		"status":  "ok",
		"before":  before,
		"after":   after,
		"removed": removed,
	})
}
