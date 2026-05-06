package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"mime/multipart"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Supported formats ─────────────────────────────────────────────────────────
// ONLY .emb files are indexed. Photos/images are NEVER indexed.
// Uploaded images are used as search queries only — never stored in the index.

var embroideryExts = map[string]bool{
	".emb": true,
}

// isSupportedExt returns true ONLY for .emb files.
func isSupportedExt(ext string) bool {
	return strings.EqualFold(ext, ".emb")
}

func isEmbExt(ext string) bool { return strings.EqualFold(ext, ".emb") }

// ── IndexState ────────────────────────────────────────────────────────────────

// IndexState holds the real-time state of the background indexing engine.
// All fields are safe for concurrent read/write via its embedded RWMutex.
type IndexState struct {
	mu             sync.RWMutex
	Running        bool           `json:"running"`
	Progress       int32          `json:"processed"`  // atomic — files finished
	Discovered     int32          `json:"discovered"` // atomic — files found by scanner
	Total          int32          `json:"total"`      // atomic — final total (set when scan ends)
	ScanDone       bool           `json:"scan_done"`  // scanner has finished walking
	CurrentFile    string         `json:"current_file"`
	CurrentRoot    string         `json:"current_root"`
	Status         string         `json:"status"`
	Log            []string       `json:"log"`
	ErrMsg         string         `json:"error,omitempty"`
	Counts         map[string]int `json:"counts"`
	UserPaused     bool           `json:"user_paused"`
	LastHeart      time.Time      `json:"-"`
	LastProgressAt time.Time      `json:"-"`

	// Global System Overview
	GlobalTotal   int32 `json:"global_total"`
	GlobalIndexed int32 `json:"global_indexed"`
}

// RegisterHeartbeat records the latest UI ping. Called from every status/state endpoint.
func RegisterHeartbeat() {
	idxState.mu.Lock()
	idxState.LastHeart = time.Now()
	idxState.mu.Unlock()
}

// isAppOpen returns true when the UI last pinged us within 15 seconds.
func isAppOpen() bool {
	idxState.mu.RLock()
	defer idxState.mu.RUnlock()
	return time.Since(idxState.LastHeart) < 15*time.Second
}

// snap produces a JSON-safe snapshot of the current state (no locks held by caller).
func (s *IndexState) snap() map[string]interface{} {
	s.mu.RLock()
	logCopy := make([]string, len(s.Log))
	copy(logCopy, s.Log)
	paused := s.UserPaused
	status := s.Status
	running := s.Running
	scanDone := s.ScanDone
	errMsg := s.ErrMsg
	counts := s.Counts
	s.mu.RUnlock()

	disc := atomic.LoadInt32(&s.Discovered)
	prog := atomic.LoadInt32(&s.Progress)
	tot := atomic.LoadInt32(&s.Total)

	// Smart total: while scanner is still running, show discovered count.
	// Once scan is done, lock in the real total.
	displayTotal := disc
	if scanDone {
		displayTotal = tot
	}

	return map[string]interface{}{
		"running":        running,
		"processed":      prog,
		"total":          displayTotal,
		"scan_done":      scanDone,
		"current_file":   s.CurrentFile,
		"current_root":   s.CurrentRoot,
		"status":         status,
		"log":            logCopy,
		"error":          errMsg,
		"counts":         counts,
		"user_paused":    paused,
		"global_total":   atomic.LoadInt32(&s.GlobalTotal),
		"global_indexed": atomic.LoadInt32(&s.GlobalIndexed),
	}
}

func (s *IndexState) addLog(msg string) {} // Deprecated

var idxState = &IndexState{
	Status: "idle",
	Counts: make(map[string]int),
}

func bumpProgress(delta int32) int32 {
	v := atomic.AddInt32(&idxState.Progress, delta)
	idxState.mu.Lock()
	idxState.LastProgressAt = time.Now()
	idxState.mu.Unlock()
	return v
}

// RefreshIdxStateCounts reloads per-format counts and global stats from SQLite.
func RefreshIdxStateCounts() {
	idxState.mu.Lock()
	idxState.Counts = dbGetFormatCounts()
	idxState.mu.Unlock()

	// Global summary
	totalIndexed := dbCount()
	atomic.StoreInt32(&idxState.GlobalIndexed, int32(totalIndexed))

	// Sum total files across all folders
	folders, _ := dbLoadFolders()
	var totalFiles int
	for _, f := range folders {
		totalFiles += f.TotalFiles
	}
	atomic.StoreInt32(&idxState.GlobalTotal, int32(totalFiles))
}

// ── File scan (streaming) ─────────────────────────────────────────────────────

// streamFiles walks dir and sends every supported file to the out channel.
func streamFiles(dir string, out chan<- string, done chan<- struct{}, resumeFrom string) {
	defer func() {
		done <- struct{}{}
	}()
	idxState.mu.Lock()
	idxState.CurrentRoot = dir
	idxState.mu.Unlock()

	// First pass: quick count to get the "Total" for this folder if not already known
	// or if we want real-time accurate totals.
	var folderTotal int32
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !d.IsDir() && isSupportedExt(filepath.Ext(p)) {
			if !isSelectedPath(p) {
				return nil
			}
			folderTotal++
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == ".cache" ||
				name == "__pycache__" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
		}
		return nil
	})
	atomic.AddInt32(&idxState.Total, folderTotal)

	// Persist the total count to the folder record
	dbSaveFolder(FolderStats{
		Path:       dir,
		Name:       filepath.Base(dir),
		TotalFiles: int(folderTotal),
		Status:     "In Progress",
	})

	skipping := resumeFrom != ""
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		idxState.mu.RLock()
		running := idxState.Running
		idxState.mu.RUnlock()
		if !running {
			return fmt.Errorf("stopped")
		}

		// Fast check: respect user pause and app-open state
		for {
			if !isAppOpen() {
				// Wait for app to reopen instead of returning error immediately
				// to allow for seamless resume without full walk restart if possible.
				// However, StartIndexingMulti checks isAppOpen too.
				time.Sleep(1 * time.Second)
				continue
			}
			idxState.mu.RLock()
			paused := idxState.UserPaused
			idxState.mu.RUnlock()
			if !paused {
				break
			}
			time.Sleep(1 * time.Second)
		}

		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == ".cache" ||
				name == "__pycache__" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			idxState.mu.Lock()
			idxState.Status = "Scanning " + name + "..."
			idxState.mu.Unlock()
			return nil
		}

		if isSupportedExt(filepath.Ext(p)) {
			if !isSelectedPath(p) {
				return nil
			}
			if skipping {
				if p == resumeFrom {
					skipping = false
					log.Printf("[Scanner] Resumed from %s", filepath.Base(p))
				}
				// Even when skipping, we count towards 'discovered' and 'progress'
				// so the UI percentage (proc/total) is accurate for resumed drives.
				atomic.AddInt32(&idxState.Discovered, 1)
				bumpProgress(1)
				return nil
			}

			atomic.AddInt32(&idxState.Discovered, 1)
			out <- p
		}
		return nil
	})
}

// fileID computes a fast content-DNA hash for a file (sha256 of size + first 2MB).
// This allows detecting moved/renamed files without re-embedding.
func fileID(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()

	h := sha256.New()
	info, _ := f.Stat()
	if info != nil {
		fmt.Fprintf(h, "%d", info.Size())
	}
	io.CopyN(h, f, 2*1024*1024)
	return fmt.Sprintf("%x", h.Sum(nil))
}

// ── Embedder calls ────────────────────────────────────────────────────────────

type embedResp struct {
	Embedding  []float32 `json:"embedding"`
	PreviewB64 string    `json:"preview_b64"`
}

// callEmbedFile embeds any image file path via the Python embedder service.
func callEmbedFile(path string) (*embedResp, error) {
	ext := strings.ToLower(filepath.Ext(path))
	isImg := ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp"

	// image → Python /embed (multi-crop, CUDA ViT-L-14)
	if isImg {
		imgBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return callEmbedRaw(imgBytes, ext)
	}

	// ── Path 3: any file → Python /embed-file ─────────────────────────────────
	for atomic.LoadInt32(&searchWaiters) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	embedderMu.Lock()
	defer embedderMu.Unlock()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("file_path", path)
	mw.Close()
	resp, err := httpClient.Post(Config.EmbedderURL()+"/embed-file", mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, fmt.Errorf("embedder unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("embedder %d: %s", resp.StatusCode, b)
	}
	var r embedResp
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

func callEmbedRaw(imgBytes []byte, ext string) (*embedResp, error) {
	for atomic.LoadInt32(&searchWaiters) > 0 {
		time.Sleep(50 * time.Millisecond)
	}
	embedderMu.Lock()
	defer embedderMu.Unlock()

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "image"+ext)
	part.Write(imgBytes)
	mw.Close()

	resp, err := httpClient.Post(Config.EmbedderURL()+"/embed", mw.FormDataContentType(), &buf)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("embedder status %d", resp.StatusCode)
	}
	var r embedResp
	json.NewDecoder(resp.Body).Decode(&r)
	r.PreviewB64 = base64.StdEncoding.EncodeToString(resizeForPreview(imgBytes))
	return &r, nil
}

var embMutex sync.Mutex

func callEmbEnginePreview(path string) []byte {
	embMutex.Lock()
	defer embMutex.Unlock()

	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("file_path", path)
	w.Close()

	// Add a tiny 'breath' for the engine
	time.Sleep(50 * time.Millisecond)

	resp, err := httpClient.Post(Config.EmbEngineURL()+"/preview", w.FormDataContentType(), &buf)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil
	}
	var r struct {
		PngB64 string `json:"png_b64"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil || r.PngB64 == "" {
		return nil
	}
	b, _ := base64.StdEncoding.DecodeString(r.PngB64)
	return b
}

// resizeForPreview compresses a raw image byte slice into a smaller thumbnail.
// Limits payload size to 128KB to preserve database performance.
func resizeForPreview(b []byte) []byte {
	if len(b) > 128*1024 {
		return b[:128*1024]
	}
	return b
}

// ── Indexing ──────────────────────────────────────────────────────────────────

// numIndexWorkers returns an optimal worker count (2-8).
func numIndexWorkers() int {
	if w := getIndexWorkers(); w > 0 {
		return w
	}
	w := runtime.NumCPU() - 1
	if w < 1 {
		return 1
	}
	if w > 8 {
		return 8
	}
	return w
}

// Entry represents a single design in the index.
type Entry struct {
	ID            string    `json:"id"`
	FilePath      string    `json:"file_path"`
	FileName      string    `json:"file_name"`
	Format        string    `json:"format"`
	SizeKB        float64   `json:"size_kb"`
	FileMTime     int64     `json:"file_mtime"`
	Vector        []float32 `json:"-"` // render embedding (from EMB preview PNG)
	SidecarVector []float32 `json:"-"` // sidecar garment-photo embedding (when available)
	HasPreview    bool      `json:"has_preview"`
	HasSidecar    bool      `json:"has_sidecar"`
}

// ── Indexing ──────────────────────────────────────────────────────────────────

// processOneEmb runs the full per-file pipeline for an .emb file:
//  1. fast-resume cache check (path+mtime+size)
//  2. content-DNA hash → detect rename/move
//  3. render via emb-engine → embed render PNG
//  4. find + embed all sidecar garment photos (averaged)
//  5. dbUpsertDual + globalIndex.Add
//
// Returns one of: "skipped" (cache hit), "moved", "indexed", "failed".
// basePath is the drive root used for the isSelected() guard so a file is
// dropped if the user un-checks the drive while this file is queued.
func processOneEmb(fp, basePath string, force bool) string {
	info, err := os.Stat(fp)
	if err != nil {
		return "failed"
	}
	mtime := info.ModTime().Unix()
	size := info.Size()

	// ── STEP 1: FAST RESUME (Path + MTime + Size match) ──────────────────────
	if !force {
		if id, found := dbCheckCache(fp, mtime, size); found {
			if !globalIndex.Has(id) {
				if entry, ok := dbGetByHash(id); ok {
					globalIndex.Add(entry)
				}
			}
			return "skipped"
		}
	}

	// ── STEP 2: CONTENT DNA (Hash) ───────────────────────────────────────────
	id := fileID(fp)
	if id == "" {
		return "failed"
	}

	// Hash already known under a DIFFERENT path → it's a move/rename.
	if existing, ok := dbGetByHash(id); ok && existing.FilePath != fp {
		dbRemoveByPath(fp)
		globalIndex.RemoveByPrefix(fp)
		dbUpdateFileMetadata(id, fp, filepath.Base(fp), mtime, float64(size)/1024)
		existing.FilePath = fp
		existing.FileName = filepath.Base(fp)
		existing.FileMTime = mtime
		existing.SizeKB = float64(size) / 1024
		globalIndex.Add(existing)
		log.Printf("[Indexer] ↪ Moved: %s", filepath.Base(fp))
		return "moved"
	}

	// ── STEP 3: EMBED (New / changed EMB file) ───────────────────────────────
	idxState.mu.Lock()
	idxState.CurrentFile = filepath.Base(fp)
	idxState.mu.Unlock()

	png := callEmbEnginePreview(fp)
	if png == nil {
		log.Printf("[Indexer] ✗ No render for %s — skipping", filepath.Base(fp))
		return "failed"
	}

	result, embErr := callEmbedRaw(png, ".png")
	if embErr != nil || result == nil {
		log.Printf("[Indexer] ✗ Embed failed for %s: %v", filepath.Base(fp), embErr)
		return "failed"
	}

	// ── SIDECAR: embed ALL paired garment photos (averaged) ──────────────────
	var sidecarBytes []byte
	var sidecarVec []float32
	allSidecars := findAllSidecars(fp)
	if len(allSidecars) > 0 {
		if b, err := os.ReadFile(allSidecars[0]); err == nil {
			sidecarBytes = b
		}
		var allVecs [][]float32
		for _, sc := range allSidecars {
			b, err := os.ReadFile(sc)
			if err != nil {
				continue
			}
			vecs, err := callEmbedAugmented(b, filepath.Base(sc))
			if err == nil && len(vecs) > 0 {
				allVecs = append(allVecs, vecs...)
			}
		}
		if len(allVecs) > 0 {
			dim := len(allVecs[0])
			avg := make([]float32, dim)
			for _, v := range allVecs {
				for i, x := range v {
					avg[i] += x
				}
			}
			nv := float32(len(allVecs))
			for i := range avg {
				avg[i] /= nv
			}
			var norm float32
			for _, x := range avg {
				norm += x * x
			}
			if norm > 0 {
				norm = float32(1.0 / math.Sqrt(float64(norm)))
				for i := range avg {
					avg[i] *= norm
				}
			}
			sidecarVec = avg
		}
	}

	var thumbBytes []byte
	if len(sidecarBytes) > 0 {
		if len(sidecarBytes) > 128*1024 {
			thumbBytes = sidecarBytes[:128*1024]
		} else {
			thumbBytes = sidecarBytes
		}
	}

	dbRemoveByPath(fp)
	globalIndex.RemoveByPrefix(fp)

	hasSidecar := len(sidecarVec) > 0
	e := Entry{
		ID: id, FilePath: fp, FileName: filepath.Base(fp),
		Format: "emb", SizeKB: float64(size) / 1024,
		FileMTime: mtime, HasPreview: true, HasSidecar: hasSidecar,
	}
	if err := dbUpsertDual(e, png, thumbBytes, result.Embedding, sidecarVec); err != nil {
		log.Printf("[Indexer] ✗ DB error for %s: %v", filepath.Base(fp), err)
		return "failed"
	}
	e.Vector = result.Embedding
	e.SidecarVector = sidecarVec
	globalIndex.Add(e)
	if hasSidecar {
		log.Printf("[Indexer] ✓ %s [render+sidecar]", filepath.Base(fp))
	} else {
		log.Printf("[Indexer] ✓ %s [render only]", filepath.Base(fp))
	}
	return "indexed"
}

// StartIndexing indexes a single drive/folder. Thin wrapper over StartIndexingMulti.
func StartIndexing(path string, force bool) {
	StartIndexingMulti([]string{path}, force)
}

// StartIndexingMulti walks every path in `paths` CONCURRENTLY, feeding all
// discovered .emb files into one shared file channel drained by a single
// worker pool. This means files from multiple drives/folders are processed
// in parallel as soon as they're found.
//
// Resume behaviour: each file goes through processOneEmb, where the path+mtime
// +size cache (dbCheckCache) skips already-indexed files in <1ms. So if you
// stop and restart, work continues from where it stopped — no full re-embed.
func StartIndexingMulti(paths []string, force bool) {
	if len(paths) == 0 {
		return
	}

	idxState.mu.Lock()
	if idxState.Running {
		idxState.mu.Unlock()
		return
	}
	idxState.Running = true
	idxState.Status = "Warming up AI engine..."
	atomic.StoreInt32(&idxState.Progress, 0)
	atomic.StoreInt32(&idxState.Discovered, 0)
	atomic.StoreInt32(&idxState.Total, 0)
	idxState.ScanDone = false
	idxState.LastProgressAt = time.Now()
	idxState.mu.Unlock()

	go func() {
		defer func() {
			final := atomic.LoadInt32(&idxState.Progress)
			atomic.StoreInt32(&idxState.Total, final)
			idxState.mu.Lock()
			idxState.Running = false
			idxState.ScanDone = true
			idxState.Status = "Idle"
			idxState.mu.Unlock()
			RefreshIdxStateCounts()
			MemoryCleanup()
		}()

		// Wait for embedder (max ~10s)
		for i := 0; i < 5; i++ {
			if !isAppOpen() {
				return
			}
			if embedderAlive() {
				break
			}
			time.Sleep(2 * time.Second)
		}

		idxState.mu.Lock()
		if len(paths) == 1 {
			idxState.Status = "Indexing " + filepath.Base(paths[0]) + "..."
		} else {
			idxState.Status = fmt.Sprintf("Indexing %d folders in parallel...", len(paths))
		}
		idxState.mu.Unlock()

		// ── Per-folder tracking initialization ──────────────────────────────
		for _, p := range paths {
			// Get folder name
			name := filepath.Base(p)
			if name == "" || name == "/" || name == "." {
				name = p
			}

			// Load or create folder stats
			lastFile, lastProcessed, _ := dbLoadProgress(p)

			// Initial stats update
			dbSaveFolder(FolderStats{
				Path:         p,
				Name:         name,
				IndexedFiles: lastProcessed,
				Status:       "In Progress",
				LastScan:     time.Now().Unix(),
			})

			if lastProcessed > 0 {
				log.Printf("[Indexer] Resuming %s — %d processed last time, last=%s",
					p, lastProcessed, filepath.Base(lastFile))
			}
		}

		workers := numIndexWorkers()
		fileCh := make(chan string, workers*4)

		// Walk all paths concurrently into the SHARED channel.
		var scanWG sync.WaitGroup
		for _, p := range paths {
			scanWG.Add(1)
			go func(root string) {
				defer scanWG.Done()

				// Load last checkpoint to skip ahead
				lastFile, _, _ := dbLoadProgress(root)

				done := make(chan struct{}, 1)
				go func() {
					streamFiles(root, fileCh, done, lastFile)
				}()
				<-done
			}(p)
		}
		go func() {
			scanWG.Wait()
			close(fileCh)
			final := atomic.LoadInt32(&idxState.Discovered)
			atomic.StoreInt32(&idxState.Total, final)
			idxState.mu.Lock()
			idxState.ScanDone = true
			idxState.mu.Unlock()
		}()

		// pickBase finds which root drive `fp` belongs to (for isSelected guard).
		pickBase := func(fp string) string {
			best := ""
			for _, p := range paths {
				if strings.HasPrefix(fp, p) && len(p) > len(best) {
					best = p
				}
			}
			return best
		}

		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup

		// Per-drive progress counters, flushed every 50 files.
		var progMu sync.Mutex
		progCounts := make(map[string]int)

		for fp := range fileCh {
			// Honour user-pause / app-close / stop signal.
			for {
				if !isAppOpen() {
					// Wait for app to reopen
					time.Sleep(1 * time.Second)
					continue
				}
				idxState.mu.RLock()
				paused := idxState.UserPaused
				running := idxState.Running
				idxState.mu.RUnlock()
				if !running {
					return
				}
				if !paused {
					break
				}
				time.Sleep(1 * time.Second)
			}

			fp := fp
			base := pickBase(fp)
			if base != "" && !isSelectedRoot(base) {
				bumpProgress(1)
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()

				if base != "" && !isSelectedRoot(base) {
					bumpProgress(1)
					return
				}
				processOneEmb(fp, base, force)
				bumpProgress(1)

				// Persist per-drive progress every 50 files.
				progMu.Lock()
				progCounts[base]++
				cnt := progCounts[base]
				progMu.Unlock()
				if cnt%50 == 0 {
					dbSaveProgress(base, fp, cnt)
					if base != "" {
						dbSaveFolder(FolderStats{
							Path:         base,
							Name:         filepath.Base(base),
							IndexedFiles: dbCountForPath(base),
							Status:       "In Progress",
							LastScan:     time.Now().Unix(),
						})
					}
					RefreshIdxStateCounts()
				}
			}()
		}
		wg.Wait()

		// Final progress flush.
		progMu.Lock()
		for base, cnt := range progCounts {
			if base != "" {
				dbSaveProgress(base, "", cnt)

				// Final folder status update
				idxFiles := dbCountForPath(base) // We need this function
				dbSaveFolder(FolderStats{
					Path:         base,
					Name:         filepath.Base(base),
					TotalFiles:   int(dbGetTotalForFolder(base)),
					IndexedFiles: idxFiles,
					Status:       "Completed",
					NeedsRescan:  false,
					LastScan:     time.Now().Unix(),
				})
			}
		}
		progMu.Unlock()
	}()
}

var triggerScan = make(chan struct{}, 1)

// findSidecar returns the first matching sidecar image file for an EMB (for thumbnail).
func findSidecar(embPath string) string {
	all := findAllSidecars(embPath)
	if len(all) > 0 {
		return all[0]
	}
	return ""
}

// findAllSidecars returns ALL matching sidecar image files (jpg, jpeg, png) for an EMB.
// DISABLED: User has many unrelated images sharing the same name as .emb files, causing false positives.
func findAllSidecars(embPath string) []string {
	return nil
}

func interruptibleSleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-triggerScan:
	}
}

// AutoIndexAllDrives is now a passive loop that ensures the engine stays warm
// and periodically scouts all drives to update the 'Global PC EMB' count.
func AutoIndexAllDrives() {
	for {
		// Wait for app window.
		for !isAppOpen() {
			idxState.mu.Lock()
			idxState.Status = "Awaiting app window..."
			idxState.mu.Unlock()
			interruptibleSleep(2 * time.Second)
		}

		// Periodic global discovery (every 30 mins or on start)
		go func() {
			log.Printf("[Discovery] Starting global EMB scouting...")
			selected := getSelectedDriveRoots()
			drives := autoLibPaths()
			if len(selected) > 0 {
				drives = make([]DriveEntry, 0, len(selected))
				for _, p := range selected {
					if st, err := os.Stat(p); err == nil && st.IsDir() {
						drives = append(drives, DriveEntry{Path: p, Label: driveLabel(p)})
					}
				}
			}
			extraPaths := os.Getenv("EMBFIND_EXTRA_DRIVES")
			extraRoots := make([]string, 0)
			for _, ep := range strings.Split(extraPaths, ";") {
				ep = strings.TrimSpace(ep)
				if ep != "" {
					extraRoots = append(extraRoots, ep)
				}
			}
			var globalTotal int32
			discoveredFolders := make(map[string]int)
			lastRefresh := time.Now()

			scout := func(d DriveEntry, skipExtraSubtrees bool) {
				if !usableDrive(d) {
					return
				}
				log.Printf("[Discovery]   Scouting %s...", d.Label)
				filepath.Walk(d.Path, func(fp string, info os.FileInfo, err error) error {
					if skipExtraSubtrees {
						for _, ep := range extraRoots {
							if ep != "" && fp != ep && strings.HasPrefix(fp, ep+string(os.PathSeparator)) {
								if info != nil && info.IsDir() {
									return filepath.SkipDir
								}
								return nil
							}
						}
					}
					if err == nil && !info.IsDir() && strings.HasSuffix(strings.ToLower(fp), ".emb") {
						atomic.AddInt32(&globalTotal, 1)
						dir := filepath.Dir(fp)
						if _, exists := discoveredFolders[dir]; !exists {
							dbSaveFolder(FolderStats{Path: dir, Name: filepath.Base(dir), TotalFiles: 0, Status: "Scouting..."})
						}
						discoveredFolders[dir]++

						if time.Since(lastRefresh) > 5*time.Second {
							idxState.mu.Lock()
							idxState.Total = globalTotal
							idxState.mu.Unlock()
							RefreshIdxStateCounts()
							for p, c := range discoveredFolders {
								dbSaveFolder(FolderStats{Path: p, Name: filepath.Base(p), TotalFiles: c, Status: "Scouting..."})
							}
							lastRefresh = time.Now()
						}
					}
					return nil
				})
			}

			// 1. Scout extra drives first
			for _, d := range drives {
				isExtra := false
				for _, ep := range extraRoots {
					if ep != "" && d.Path == ep {
						isExtra = true
						break
					}
				}
				if isExtra {
					scout(d, false)
				}
			}
			// 2. Scout everything else
			for _, d := range drives {
				isExtra := false
				for _, ep := range extraRoots {
					if ep != "" && d.Path == ep {
						isExtra = true
						break
					}
				}
				if !isExtra {
					scout(d, true)
				}
			}

			log.Printf("[Discovery] Found %d designs in %d folders", globalTotal, len(discoveredFolders))

			// Upsert discovered folders — use "Scouting..." so dbSaveFolder
			// preserves any real scan state (Completed/In Progress/Stopped).
			for path, count := range discoveredFolders {
				dbSaveFolder(FolderStats{
					Path:       path,
					Name:       filepath.Base(path),
					TotalFiles: count,
					Status:     "Scouting...",
				})
			}

			// Recalculate indexed_files for every folder from real DB data.
			dbRecalcAllFolderCounts()

			idxState.mu.Lock()
			idxState.Total = globalTotal
			idxState.mu.Unlock()
			RefreshIdxStateCounts()
			log.Printf("[Discovery] Global EMB scouting complete")
		}()

		// Wait for a manual trigger or just sleep (discovery runs once then we wait)
		interruptibleSleep(30 * time.Minute)
	}
}
