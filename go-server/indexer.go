package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
// Both embroidery AND image formats — CLIP handles all of them.

var embroideryExts = map[string]bool{
	".emb": true,
}

// imageExts — helper for search query validation
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
}

func isSupportedExt(ext string) bool {
	return isEmbExt(ext) || isImageExt(ext)
}

func isImageExt(ext string) bool { return imageExts[strings.ToLower(ext)] }
func isEmbExt(ext string) bool   { return embroideryExts[strings.ToLower(ext)] }

// ── Services ──────────────────────────────────────────────────────────────────
// Python Embedder URL is now provided by Config.EmbedderURL()

func embEngineSvcURL() string {
	if u := os.Getenv("EMB_ENGINE_URL"); u != "" {
		return u
	}
	// Default to localhost for local development, Docker overrides this via ENV
	return "http://localhost:8767"
}

// ── IndexState ────────────────────────────────────────────────────────────────

// IndexState holds the real-time state of the background indexing engine.
// All fields are safe for concurrent read/write via its embedded RWMutex.
type IndexState struct {
	mu          sync.RWMutex
	Running     bool           `json:"running"`
	Progress    int32          `json:"processed"`  // atomic — files finished
	Discovered  int32          `json:"discovered"` // atomic — files found by scanner
	Total       int32          `json:"total"`      // atomic — final total (set when scan ends)
	ScanDone    bool           `json:"scan_done"`  // scanner has finished walking
	CurrentFile string         `json:"current_file"`
	Status      string         `json:"status"`
	Log         []string       `json:"log"`
	ErrMsg      string         `json:"error,omitempty"`
	Counts      map[string]int `json:"counts"`
	UserPaused  bool           `json:"user_paused"`
	LastHeart   time.Time      `json:"-"`
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
		"running":      running,
		"processed":    prog,
		"total":        displayTotal,
		"scan_done":    scanDone,
		"current_file": s.CurrentFile,
		"status":       status,
		"log":          logCopy,
		"error":        errMsg,
		"counts":       counts,
		"user_paused":  paused,
	}
}

func (s *IndexState) addLog(line string) {
	s.mu.Lock()
	s.Log = append(s.Log, line)
	if len(s.Log) > 2000 {
		s.Log = s.Log[len(s.Log)-2000:]
	}
	s.mu.Unlock()
}

var idxState = &IndexState{
	Status: "idle",
	Counts: make(map[string]int),
}

// RefreshIdxStateCounts reloads per-format counts from SQLite.
func RefreshIdxStateCounts() {
	idxState.mu.Lock()
	idxState.Counts = dbGetFormatCounts()
	idxState.mu.Unlock()
}

// ── File scan (streaming) ─────────────────────────────────────────────────────

// streamFiles walks dir and sends every supported file to the out channel.
// It atomically increments idxState.Discovered as each file is found,
// so the UI can display a live "found N so far" count without waiting for
// the full walk to complete. When done it signals the done channel.
func streamFiles(dir string, out chan<- string, done chan<- struct{}) {
	defer func() {
		done <- struct{}{}
	}()

	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if !isSelected(dir) {
			return fmt.Errorf("drive unselected")
		}

		// Fast check: respect user pause and app-open state
		for {
			if !isAppOpen() {
				return fmt.Errorf("app closed")
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
			// Skip heavy / hidden directories for maximum walk speed.
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
			// Increment discovered BEFORE sending so the UI total is always >= processed.
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

// callEmbedFile orchestrates the preview rendering and AI embedding for a given file path.
// It automatically routes image files directly to the AI engine via raw bytes to avoid IPC path issues,
// and routes embroidery files through the pystitch renderer before embedding.
func callEmbedFile(path string) (*embedResp, error) {
	ext := strings.ToLower(filepath.Ext(path))

	// ── Path 1: local ONNX CLIP (image or any file with preview) ─────────────
	if clipReady && isImageExt(ext) {
		imgBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		vec, err := EmbedImageBytes(imgBytes)
		if err != nil {
			return nil, err
		}
		previewB64 := base64.StdEncoding.EncodeToString(resizeForPreview(imgBytes))
		return &embedResp{Embedding: vec, PreviewB64: previewB64}, nil
	}

	// ── Path 2: image file → Python /embed (multipart POST) ────────
	// Consistent with callEmbedRaw, handles binary image data.
	if isImageExt(ext) {
		imgBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return callEmbedRaw(imgBytes, ext)
	}

	// ── Path 3: embroidery → Python /embed-file (pystitch render + embed) ─────
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
	// If local CLIP is also ready, re-embed the preview locally (consistent dim)
	if clipReady && r.PreviewB64 != "" {
		if pngBytes, decErr := base64.StdEncoding.DecodeString(r.PreviewB64); decErr == nil {
			if vec, embErr := EmbedImageBytes(pngBytes); embErr == nil {
				r.Embedding = vec
			}
		}
	}
	return &r, nil
}

func callEmbedRaw(imgBytes []byte, ext string) (*embedResp, error) {
	if clipReady {
		vec, err := EmbedImageBytes(imgBytes)
		if err == nil {
			previewB64 := base64.StdEncoding.EncodeToString(resizeForPreview(imgBytes))
			return &embedResp{Embedding: vec, PreviewB64: previewB64}, nil
		}
	}

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

	resp, err := httpClient.Post(embEngineSvcURL()+"/preview", w.FormDataContentType(), &buf)
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
	w := runtime.NumCPU() - 1
	if w < 2 { return 2 }
	if w > 8 { return 8 }
	return w
}

// Entry represents a single design in the index.
type Entry struct {
	ID         string    `json:"id"`
	FilePath   string    `json:"file_path"`
	FileName   string    `json:"file_name"`
	Format     string    `json:"format"`
	SizeKB     float64   `json:"size_kb"`
	FileMTime  int64     `json:"file_mtime"`
	Vector     []float32 `json:"-"`
	HasPreview bool      `json:"has_preview"`
}

// ── Indexing ──────────────────────────────────────────────────────────────────

func StartIndexing(path string, force bool) {
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
	idxState.mu.Unlock()

	go func() {
		defer func() {
			// Ensure progress and total are synced at the end for a clean 100% UI look
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

		// Wait for embedder (max 10s for faster UI response)
		for i := 0; i < 5; i++ {
			if !isAppOpen() { return } // Abort instantly if UI closes during warmup
			if embedderAlive() { break }
			time.Sleep(2 * time.Second)
		}

		idxState.mu.Lock()
		idxState.Status = "Indexing " + filepath.Base(path) + "..."
		idxState.mu.Unlock()

		workers := numIndexWorkers()
		fileCh := make(chan string, workers*4)
		scanDone := make(chan struct{}, 1)

		go func() {
			streamFiles(path, fileCh, scanDone)
			close(fileCh)
		}()

		go func() {
			<-scanDone
			final := atomic.LoadInt32(&idxState.Discovered)
			atomic.StoreInt32(&idxState.Total, final)
			idxState.mu.Lock()
			idxState.ScanDone = true
			idxState.mu.Unlock()
		}()

		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup

		for fp := range fileCh {
			// Fast check: respect user pause, app-open state, and stop signal
			for {
				if !isAppOpen() { break }
				idxState.mu.RLock()
				paused := idxState.UserPaused
				running := idxState.Running
				idxState.mu.RUnlock()
				if !running { 
					// Stop signal received
					return 
				}
				if !paused { break }
				time.Sleep(1 * time.Second)
			}

			if !isAppOpen() {
				// Rapidly drain the channel if app is closed to exit cleanly
				atomic.AddInt32(&idxState.Progress, 1)
				continue
			}

			fp := fp
			info, err := os.Stat(fp)
			if err != nil {
				atomic.AddInt32(&idxState.Progress, 1)
				continue
			}
			mtime := info.ModTime().Unix()
			size := info.Size()

			// ── STEP 1: FAST RESUME (Path + MTime + Size match) ─────────────
			if !force {
				if id, found := dbCheckCache(fp, mtime, size); found {
					// Path exists and hasn't changed. Check if in memory.
					if !globalIndex.Has(id) {
						// Load back into memory if missing (e.g. after crash/clear)
						if entry, ok := dbGetByHash(id); ok {
							globalIndex.Add(entry)
						}
					}
					atomic.AddInt32(&idxState.Progress, 1)
					continue
				}
			}

			// ── STEP 2: CONTENT DNA (Hash) ──────────────────────────────────
			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()
				
				// Final safety check: if drive was unchecked while queued
				if !isSelected(path) {
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				id := fileID(fp)
				if id == "" {
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				// Check if hash exists under DIFFERENT path (Rename/Move)
				if existing, ok := dbGetByHash(id); ok {
					// Clean up any stale records at the new path before overwriting
					dbRemoveByPath(fp)
					globalIndex.RemoveByPrefix(fp)

					dbUpdateFileMetadata(id, fp, filepath.Base(fp), mtime, float64(size)/1024)
					existing.FilePath = fp
					existing.FileName = filepath.Base(fp)
					existing.FileMTime = mtime
					existing.SizeKB = float64(size)/1024
					globalIndex.Add(existing)
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				// ── STEP 3: EMBED (New File) ────────────────────────────────
				idxState.mu.Lock()
				idxState.CurrentFile = filepath.Base(fp)
				idxState.mu.Unlock()

				var png []byte
				var result *embedResp
				var err error

				if strings.ToLower(filepath.Ext(fp)) == ".emb" {
					log.Printf("[Indexer] Processing EMB: %s", filepath.Base(fp))
					// 1. Try to find a sidecar image first
					sidecar := findSidecar(fp)
					if sidecar != "" {
						log.Printf("[Indexer]   -> Found sidecar: %s", filepath.Base(sidecar))
						result, err = callEmbedFile(sidecar)
					}
					
					// 2. If no sidecar, or sidecar failed, render a preview
					if result == nil {
						log.Printf("[Indexer]   -> Rendering Wilcom preview...")
						png = callEmbEnginePreview(fp)
						if png != nil {
							result, err = callEmbedRaw(png, ".png")
						} else {
							log.Printf("[Indexer]   ! Wilcom render failed for %s", filepath.Base(fp))
						}
					}
					
					// DO NOT fallback to direct file embed for .EMB as it is binary data, not an image.
					if result == nil {
						log.Printf("[Indexer]   ! Skipping %s: no visual preview or sidecar found", filepath.Base(fp))
					}
				} else {
					log.Printf("[Indexer] Processing Image: %s", filepath.Base(fp))
					result, err = callEmbedFile(fp)
				}

				if err != nil || result == nil {
					log.Printf("[Indexer] ✗ Failed to index %s: %v", filepath.Base(fp), err)
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				if png == nil && result.PreviewB64 != "" {
					png, _ = base64.StdEncoding.DecodeString(result.PreviewB64)
				}

				// Clean up any old entries at this path
				dbRemoveByPath(fp)
				globalIndex.RemoveByPrefix(fp)

				e := Entry{
					ID: id, FilePath: fp, FileName: filepath.Base(fp),
					Format:     strings.TrimPrefix(strings.ToLower(filepath.Ext(fp)), "."),
					SizeKB:     float64(size) / 1024, FileMTime: mtime, HasPreview: png != nil,
				}
				if err := dbUpsert(e, png, result.Embedding); err == nil {
					e.Vector = result.Embedding
					globalIndex.Add(e)
					log.Printf("[Indexer] ✓ Indexed: %s", filepath.Base(fp))
					
					if atomic.LoadInt32(&idxState.Progress)%10 == 0 {
						RefreshIdxStateCounts()
					}
				} else {
					log.Printf("[Indexer] ✗ DB Error for %s: %v", filepath.Base(fp), err)
				}
				atomic.AddInt32(&idxState.Progress, 1)
			}()
		}
		wg.Wait()
	}()
}

var triggerScan = make(chan struct{}, 1)

func findSidecar(embPath string) string {
	dir := filepath.Dir(embPath)
	base := strings.TrimSuffix(filepath.Base(embPath), filepath.Ext(embPath))
	
	// Try common extensions
	for _, ext := range []string{".jpg", ".JPG", ".png", ".PNG", ".jpeg", ".JPEG"} {
		side := filepath.Join(dir, base+ext)
		if _, err := os.Stat(side); err == nil {
			return side
		}
	}
	return ""
}

func interruptibleSleep(d time.Duration) {
	select {
	case <-time.After(d):
	case <-triggerScan:
	}
}

// AutoIndexAllDrives is the perpetual background sync loop.
func AutoIndexAllDrives() {
	for {
		// Wait for app window.
		for !isAppOpen() {
			idxState.mu.Lock()
			idxState.Status = "Awaiting app window..."
			idxState.mu.Unlock()
			interruptibleSleep(2 * time.Second)
		}

		// Sync only if not paused.
		if idxState.UserPaused {
			idxState.mu.Lock()
			idxState.Status = "Sync paused"
			idxState.mu.Unlock()
			interruptibleSleep(2 * time.Second)
			continue
		}

		drives := getDrivesToScan()
		if len(drives) == 0 {
			interruptibleSleep(10 * time.Second)
			continue
		}

		for _, d := range drives {
			// Check app-open and pause before every drive.
			if !isAppOpen() || idxState.UserPaused { break }

			StartIndexing(d.Path, false)

			// Wait for drive to finish, checking app-open frequently.
			for {
				interruptibleSleep(2 * time.Second)
				if !isAppOpen() || idxState.UserPaused { break }
				idxState.mu.RLock()
				running := idxState.Running
				idxState.mu.RUnlock()
				if !running { break }
			}
		}

		// Large sleep between full sweeps to save CPU/Disk.
		interruptibleSleep(5 * time.Minute)
	}
}
