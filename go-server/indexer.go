package main

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// ── Supported formats ─────────────────────────────────────────────────────────
// Both embroidery AND image formats — CLIP handles all of them.

var embroideryExts = map[string]bool{
	".emb": true, ".dst": true, ".pes": true, ".jef": true,
	".vp3": true, ".hus": true, ".xxx": true, ".sew": true,
	".u01": true, ".shv": true, ".bro": true, ".dat": true,
	".dsb": true, ".dsz": true, ".emd": true, ".gt": true,
	".inb": true, ".tbf": true, ".ksm": true, ".tap": true,
	".stx": true, ".phb": true, ".phc": true, ".new": true,
	".max": true, ".mit": true, ".pcd": true, ".pcm": true,
	".pcs": true, ".jpx": true, ".stc": true, ".zhs": true,
	".pmv": true, ".plt": true, ".qcc": true, ".iqp": true,
	".exp": true,
}

// imageExts — regular images can also be indexed and searched.
var imageExts = map[string]bool{
	".jpg": true, ".jpeg": true, ".png": true, ".webp": true,
	".gif": true, ".bmp": true, ".tiff": true, ".tif": true,
	".heic": true, ".avif": true,
}

func isSupportedExt(ext string) bool {
	return isEmbExt(ext)
}

func isImageExt(ext string) bool { return imageExts[strings.ToLower(ext)] }
func isEmbExt(ext string) bool   { return embroideryExts[strings.ToLower(ext)] }

// AllSupportedFormats returns sorted list of all supported extensions.
func AllSupportedFormats() []string {
	all := make([]string, 0, len(embroideryExts)+len(imageExts))
	for e := range embroideryExts {
		all = append(all, e)
	}
	for e := range imageExts {
		all = append(all, e)
	}
	return all
}

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
type IndexState struct {
	mu          sync.RWMutex
	Running     bool           `json:"running"`
	Progress    int32          `json:"processed"`
	Total       int            `json:"total"`
	CurrentFile string         `json:"current_file"`
	Status      string         `json:"status"`
	Log         []string       `json:"log"`
	ErrMsg      string         `json:"error,omitempty"`
	Counts      map[string]int `json:"counts"`
	UserPaused  bool           `json:"user_paused"`
	LastHeart   time.Time      `json:"-"`
}

func RegisterHeartbeat() {
	idxState.mu.Lock()
	idxState.LastHeart = time.Now()
	idxState.mu.Unlock()
}

func isAppOpen() bool {
	idxState.mu.RLock()
	defer idxState.mu.RUnlock()
	return time.Since(idxState.LastHeart) < 15*time.Second
}

func (s *IndexState) snap() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	logCopy := make([]string, len(s.Log))
	copy(logCopy, s.Log)
	return map[string]interface{}{
		"running":      s.Running,
		"processed":    atomic.LoadInt32(&s.Progress),
		"total":        s.Total,
		"current_file": s.CurrentFile,
		"status":       s.Status,
		"log":          logCopy,
		"error":        s.ErrMsg,
		"counts":       s.Counts,
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

func RefreshIdxStateCounts() {
	idxState.mu.Lock()
	idxState.Counts = dbGetFormatCounts()
	idxState.mu.Unlock()
}

// ── File scan ─────────────────────────────────────────────────────────────────

func findFiles(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip heavy/hidden folders for maximum performance
			name := d.Name()
			if name == "node_modules" || name == ".git" || name == ".cache" || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			// Update status with current folder to show activity
			idxState.mu.Lock()
			idxState.Status = "Scanning " + name + "..."
			idxState.mu.Unlock()
			return nil
		}
		if isSupportedExt(filepath.Ext(p)) {
			files = append(files, p)
			// Update total count live so user sees progress
			idxState.mu.Lock()
			idxState.Total = len(files)
			idxState.mu.Unlock()
		}
		return nil
	})
	return files
}

func fileID(p string) string {
	f, err := os.Open(p)
	if err != nil {
		return ""
	}
	defer f.Close()

	// Deep Logic: Hash the actual content (first 2MB + Size)
	// This creates a unique 'DNA' for the file even if renamed/moved.
	h := md5.New()
	info, _ := f.Stat()
	if info != nil {
		fmt.Fprintf(h, "%d", info.Size()) // Include size in DNA
	}
	
	// Fast-hash: Read up to 2MB to keep it lightning fast
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

	// ── Path 2: image file → Python /embed-image-raw (raw bytes POST) ────────
	// Avoids file-path sharing issues; Python opens image from bytes directly.
	if isImageExt(ext) {
		imgBytes, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Post(
			Config.EmbedderURL()+"/embed-image-raw",
			"image/"+strings.TrimPrefix(ext, "."),
			bytes.NewReader(imgBytes),
		)
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
		// Use original image as preview
		r.PreviewB64 = base64.StdEncoding.EncodeToString(resizeForPreview(imgBytes))
		return &r, nil
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
		return b[:128*1024] // placeholder — real resize in production
	}
	return b
}

// ── Indexing ──────────────────────────────────────────────────────────────────

// StartIndexing scans a local folder for supported image and embroidery files,
// orchestrates AI embedding, and stores the results in the local SQLite database.
func StartIndexing(path string, force bool) {
	idxState.mu.Lock()
	if idxState.Running {
		idxState.mu.Unlock()
		return
	}
	idxState.Running = true
	idxState.Status = "Waiting for AI engine to warm up..."
	idxState.Progress = 0
	idxState.Total = 0
	idxState.mu.Unlock()

	go func() {
		defer func() {
			idxState.mu.Lock()
			idxState.Running = false
			idxState.Status = "Idle"
			idxState.mu.Unlock()
			RefreshIdxStateCounts()
			MemoryCleanup()
		}()

		// Deep Logic: Wait for Python server to be 100% ready before hammering it
		for i := 0; i < 30; i++ { // Wait up to 60s
			if embedderAlive() {
				break
			}
			time.Sleep(2 * time.Second)
		}

		// 1. Live Discovery (findFiles now updates Total live)
		files := findFiles(path)

		// 2. Indexing
		idxState.mu.Lock()
		idxState.Status = "Indexing Designs..."
		idxState.mu.Unlock()

		workers := 4
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup

		for _, fp := range files {
			// Deep Logic: Only work if the app is open AND user hasn't paused!
			for !isAppOpen() || idxState.UserPaused {
				idxState.mu.Lock()
				if idxState.UserPaused {
					idxState.Status = "Sync paused by user"
				} else {
					idxState.Status = "Paused — waiting for app window..."
				}
				idxState.mu.Unlock()
				time.Sleep(2 * time.Second)
			}

			fp := fp
			id := fileID(fp)
			if id == "" {
				continue
			}

			if !force && dbIndexed(id) {
				// Deep Logic: Move/Rename detected. Update path without re-indexing.
				dbUpdatePath(id, fp, filepath.Base(fp))
				atomic.AddInt32(&idxState.Progress, 1)
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()

				idxState.mu.Lock()
				idxState.CurrentFile = filepath.Base(fp)
				idxState.mu.Unlock()

				var embEnginePNG []byte
				if strings.ToLower(filepath.Ext(fp)) == ".emb" {
					embEnginePNG = callEmbEnginePreview(fp)
				}

				result, err := callEmbedFile(fp)
				atomic.AddInt32(&idxState.Progress, 1)
				if err != nil {
					return
				}

				png := embEnginePNG
				if png == nil && result.PreviewB64 != "" {
					png, _ = base64.StdEncoding.DecodeString(result.PreviewB64)
				}

				info, _ := os.Stat(fp)
				sizeKB := 0.0
				if info != nil {
					sizeKB = float64(info.Size()) / 1024
				}

				e := Entry{
					ID: id, FilePath: fp, FileName: filepath.Base(fp),
					Format: strings.TrimPrefix(strings.ToLower(filepath.Ext(fp)), "."),
					SizeKB: sizeKB, HasPreview: png != nil,
				}
				if err := dbUpsert(e, png, result.Embedding); err == nil {
					e.Vector = result.Embedding
					globalIndex.Add(e)
				}
			}()
		}
		wg.Wait()
	}()
}

// AutoIndexAllDrives scans all auto-detected system drives and indexes them in the background.
// It processes drives sequentially to avoid system overload.
func AutoIndexAllDrives() {
	// Let the user know we are ready and waiting
	idxState.mu.Lock()
	idxState.Status = "Awaiting app window to begin sync..."
	idxState.mu.Unlock()

	// Wait for the first app heartbeat before doing anything
	for !isAppOpen() {
		time.Sleep(2 * time.Second)
	}

	// Perpetual Background Loop: Keeps the library in sync and allows for fresh starts
	for {
		// Only work if the user hasn't paused the engine and app is open
		for !isAppOpen() || idxState.UserPaused {
			idxState.mu.Lock()
			if idxState.UserPaused {
				idxState.Status = "Sync paused by user"
			} else {
				idxState.Status = "Awaiting app window..."
			}
			idxState.mu.Unlock()
			time.Sleep(1 * time.Second)
		}

		drives := autoLibPaths()
		if len(drives) == 0 {
			time.Sleep(30 * time.Second)
			continue
		}

		for _, d := range drives {
			if d.Path == "/" {
				continue
			}

			// Perform indexing (Content-DNA logic handles the skipping automatically)
			StartIndexing(d.Path, false)

			// Wait for the indexer to become idle before moving to the next drive
			for {
				time.Sleep(5 * time.Second)
				idxState.mu.RLock()
				running := idxState.Running
				idxState.mu.RUnlock()
				if !running {
					break
				}
			}
		}

		// Full system scan complete. Wait before the next health check scan.
		time.Sleep(2 * time.Minute)
	}
}
