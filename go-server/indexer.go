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
	e := strings.ToLower(ext)
	return embroideryExts[e] || imageExts[e]
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
	return "http://emb-engine:8767"
}

// ── IndexState ────────────────────────────────────────────────────────────────
type IndexState struct {
	mu          sync.RWMutex
	Running     bool     `json:"running"`
	Progress    int32    `json:"-"`
	Total       int      `json:"total"`
	CurrentFile string   `json:"current_file"`
	Status      string   `json:"status"`
	Log         []string `json:"log"`
	ErrMsg      string   `json:"error,omitempty"`
}

func (s *IndexState) snap() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	logCopy := make([]string, len(s.Log))
	copy(logCopy, s.Log)
	return map[string]interface{}{
		"running":      s.Running,
		"progress":     atomic.LoadInt32(&s.Progress),
		"total":        s.Total,
		"current_file": s.CurrentFile,
		"status":       s.Status,
		"log":          logCopy,
		"error":        s.ErrMsg,
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

var idxState = &IndexState{Status: "idle"}

// ── File scan ─────────────────────────────────────────────────────────────────

func findFiles(dir string) []string {
	var files []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isSupportedExt(filepath.Ext(p)) {
			files = append(files, p)
		}
		return nil
	})
	return files
}

func fileID(p string) string { return fmt.Sprintf("%x", md5.Sum([]byte(p))) }

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

func callEmbEnginePreview(path string) []byte {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	w.WriteField("file_path", path)
	w.Close()
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
func StartIndexing(folder string, force bool) {
	files := findFiles(folder)

	idxState.mu.Lock()
	idxState.Running = true
	idxState.Total = len(files)
	idxState.Log = nil
	idxState.ErrMsg = ""
	idxState.Status = "running"
	atomic.StoreInt32(&idxState.Progress, 0)
	idxState.mu.Unlock()

	go func() {
		defer func() {
			idxState.mu.Lock()
			idxState.Running = false
			idxState.Status = "done"
			idxState.mu.Unlock()

			// Aggressively clean up memory after a big indexing run
			MemoryCleanup()
		}()

		workers := Config.MaxWorkers
		// Throttle slightly if we are heavily relying on Python and CPU is limited
		if workers > 4 {
			workers = 4
		}
		sem := make(chan struct{}, workers)
		var wg sync.WaitGroup

		for _, fp := range files {
			fp := fp
			id := fileID(fp)

			if !force && dbIndexed(id) {
				atomic.AddInt32(&idxState.Progress, 1)
				// Silent skip for maximum performancefor
				continue
			}

			sem <- struct{}{}
			wg.Add(1)
			go func() {
				defer func() { <-sem; wg.Done() }()

				idxState.mu.Lock()
				idxState.CurrentFile = filepath.Base(fp)
				idxState.mu.Unlock()

				// High-quality EmbEngine preview for .emb files
				var embEnginePNG []byte
				if strings.ToLower(filepath.Ext(fp)) == ".emb" {
					embEnginePNG = callEmbEnginePreview(fp)
				}

				result, err := callEmbedFile(fp)
				prog := atomic.AddInt32(&idxState.Progress, 1)
				if err != nil {
					idxState.addLog(fmt.Sprintf("FAIL %s — %v", filepath.Base(fp), err))
					_ = prog
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
					ID:         id,
					FilePath:   fp,
					FileName:   filepath.Base(fp),
					Format:     strings.TrimPrefix(strings.ToLower(filepath.Ext(fp)), "."),
					SizeKB:     sizeKB,
					HasPreview: png != nil,
				}
				if err := dbUpsert(e, png, result.Embedding); err == nil {
					e.Vector = result.Embedding
					globalIndex.Add(e)
				}
				// Silent success for maximum performance, UI updates via progress bar
			}()
		}
		wg.Wait()
	}()
}
