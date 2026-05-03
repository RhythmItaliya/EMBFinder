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
	w := runtime.NumCPU() - 1
	if w < 2 { return 2 }
	if w > 8 { return 8 }
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

				// ── STEP 3: EMBED (New EMB File) ────────────────────────────
				idxState.mu.Lock()
				idxState.CurrentFile = filepath.Base(fp)
				idxState.mu.Unlock()

				// Render EMB → PNG via emb-engine
				png := callEmbEnginePreview(fp)
				if png == nil {
					log.Printf("[Indexer] ✗ No render for %s — skipping", filepath.Base(fp))
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				// Embed the rendered PNG as the primary search vector
				result, embErr := callEmbedRaw(png, ".png")
				if embErr != nil || result == nil {
					log.Printf("[Indexer] ✗ Embed failed for %s: %v", filepath.Base(fp), embErr)
					atomic.AddInt32(&idxState.Progress, 1)
					return
				}

				// ── SIDECAR: embed ALL paired garment photos if available ─────────────
				// This is the key accuracy improvement: garment photos and query images
				// are in the same visual domain, so matching query→sidecar is far more
				// accurate than matching query→flat-render.
				// We embed ALL sidecars (jpg + jpeg + png) and average them into one
				// variation-invariant vector that covers all photo angles/formats.
				var sidecarBytes []byte
				var sidecarVec []float32
				allSidecars := findAllSidecars(fp)
				if len(allSidecars) > 0 {
					// Read first sidecar for thumbnail
					if b, err := os.ReadFile(allSidecars[0]); err == nil {
						sidecarBytes = b
					}
					// Collect augmented embeddings from ALL sidecar photos
					var allVecs [][]float32
					for _, sc := range allSidecars {
						b, err := os.ReadFile(sc)
						if err != nil {
							continue
						}
						vecs, err := callEmbedAugmented(b, filepath.Base(sc))
						if err == nil && len(vecs) > 0 {
							allVecs = append(allVecs, vecs...)
							log.Printf("[Indexer] ✓ Sidecar: %s (%d views)", filepath.Base(sc), len(vecs))
						}
					}
					if len(allVecs) > 0 {
						// Average ALL vectors from all sidecars into one robust representation
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
						// Re-normalize to unit sphere
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
						log.Printf("[Indexer] ✓ Sidecar vector: %d sidecars × augmented → %d total views averaged",
							len(allSidecars), len(allVecs))
					}
				}

				// Compress sidecar thumbnail for storage
				var thumbBytes []byte
				if len(sidecarBytes) > 0 {
					if len(sidecarBytes) > 128*1024 {
						thumbBytes = sidecarBytes[:128*1024]
					} else {
						thumbBytes = sidecarBytes
					}
				}

				// Store entry: render PNG as preview, dual embeddings
				dbRemoveByPath(fp)
				globalIndex.RemoveByPrefix(fp)

				hasSidecar := len(sidecarVec) > 0
				e := Entry{
					ID: id, FilePath: fp, FileName: filepath.Base(fp),
					Format: "emb", SizeKB: float64(size) / 1024,
					FileMTime: mtime, HasPreview: true, HasSidecar: hasSidecar,
				}
				if err := dbUpsertDual(e, png, thumbBytes, result.Embedding, sidecarVec); err == nil {
					e.Vector = result.Embedding
					e.SidecarVector = sidecarVec
					globalIndex.Add(e)
					if hasSidecar {
						log.Printf("[Indexer] ✓ %s [render+sidecar]", filepath.Base(fp))
					} else {
						log.Printf("[Indexer] ✓ %s [render only]", filepath.Base(fp))
					}
					if atomic.LoadInt32(&idxState.Progress)%10 == 0 {
						RefreshIdxStateCounts()
					}
				} else {
					log.Printf("[Indexer] ✗ DB error for %s: %v", filepath.Base(fp), err)
				}
				atomic.AddInt32(&idxState.Progress, 1)
			}()
		}
		wg.Wait()
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
// This allows embedding multiple garment photos of the same design into one averaged vector.
func findAllSidecars(embPath string) []string {
	dir := filepath.Dir(embPath)
	base := strings.TrimSuffix(filepath.Base(embPath), filepath.Ext(embPath))

	var found []string
	seen := make(map[string]bool)
	for _, ext := range []string{".jpg", ".JPG", ".jpeg", ".JPEG", ".png", ".PNG"} {
		side := filepath.Join(dir, base+ext)
		if _, err := os.Stat(side); err == nil {
			// Deduplicate case-insensitive on same base name
			key := strings.ToLower(side)
			if !seen[key] {
				seen[key] = true
				found = append(found, side)
			}
		}
	}
	return found
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
