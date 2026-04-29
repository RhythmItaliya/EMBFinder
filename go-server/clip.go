// clip.go — CLIP ViT-B/32 image encoder running entirely in Go via ONNX Runtime.
// On first run it downloads:
//  1. The ONNX Runtime shared library (~60MB, one time)
//  2. CLIP ViT-B/32 vision encoder ONNX model (~350MB, one time)
//
// After that all embedding is local, zero Python dependency.
package main

import (
	"bytes"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"golang.org/x/image/draw"

	ort "github.com/yalue/onnxruntime_go"
)

const (
	clipSz  = 224 // CLIP input size
	clipDim = 512 // CLIP ViT-B/32 output dimensions
)

// CLIP normalisation constants
var (
	clipMean = [3]float32{0.48145466, 0.4578275, 0.40821073}
	clipStd  = [3]float32{0.26862954, 0.26130258, 0.27577711}
)

// Model download URLs — official CLIP ONNX export
const (
	clipModelURL = "https://huggingface.co/Xenova/clip-vit-base-patch32/resolve/main/onnx/vision_model.onnx"
)

var (
	clipOnce    sync.Once
	clipSession *ort.AdvancedSession
	clipErr     error
	clipReady   bool
)

// clipModelPath returns the local path for the cached CLIP model.
func clipModelPath() string {
	cacheDir := os.Getenv("CLIP_CACHE_DIR")
	if cacheDir == "" {
		home, _ := os.UserHomeDir()
		cacheDir = filepath.Join(home, ".embfinder", "models")
	}
	return filepath.Join(cacheDir, "clip_vit_b32_vision.onnx")
}

// ortLibPath returns the expected ONNX Runtime shared library path.
func ortLibPath() string {
	switch runtime.GOOS {
	case "linux":
		return "/usr/lib/libonnxruntime.so.1"
	case "darwin":
		return "/usr/local/lib/libonnxruntime.dylib"
	case "windows":
		return "onnxruntime.dll"
	}
	return "libonnxruntime.so"
}

// InitCLIP loads the ONNX Runtime and CLIP model. Downloads both if missing.
// Called once at startup in a goroutine — search works without it (falls back to Python).
func InitCLIP() error {
	clipOnce.Do(func() {
		modelPath := clipModelPath()
		if err := ensureModel(modelPath); err != nil {
			clipErr = fmt.Errorf("CLIP model download: %w", err)
			return
		}

		libPath := ortLibPath()
		ort.SetSharedLibraryPath(libPath)
		if err := ort.InitializeEnvironment(); err != nil {
			// Library not found — try fallback without custom path
			ort.SetSharedLibraryPath("")
			if err2 := ort.InitializeEnvironment(); err2 != nil {
				clipErr = fmt.Errorf("ONNX Runtime not found: %w", err2)
				return
			}
		}

		// Input: float32[1,3,224,224]  Output: float32[1,512]
		inputNames := []string{"pixel_values"}
		outputNames := []string{"image_embeds"}

		inShape := ort.NewShape(1, 3, clipSz, clipSz)
		outShape := ort.NewShape(1, clipDim)
		inTensor, _ := ort.NewEmptyTensor[float32](inShape)
		outTensor, _ := ort.NewEmptyTensor[float32](outShape)

		sess, err := ort.NewAdvancedSessionWithONNXData(
			mustReadFile(modelPath),
			inputNames, outputNames,
			[]ort.ArbitraryTensor{inTensor},
			[]ort.ArbitraryTensor{outTensor},
			nil,
		)
		if err != nil {
			clipErr = fmt.Errorf("ONNX session: %w", err)
			return
		}
		clipSession = sess
		clipReady = true
		fmt.Println("[CLIP] Ready — ONNX Runtime loaded, model in memory.")
	})
	return clipErr
}

// EmbedImageBytes embeds raw image bytes using local CLIP ONNX.
// Returns nil, err if CLIP is not ready (fall back to Python).
func EmbedImageBytes(imgBytes []byte) ([]float32, error) {
	if !clipReady {
		return nil, fmt.Errorf("CLIP not ready")
	}

	img, _, err := image.Decode(bytes.NewReader(imgBytes))
	if err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	return EmbedImage(img)
}

// EmbedImage encodes an image into a 512-dim normalized CLIP vector.
func EmbedImage(img image.Image) ([]float32, error) {
	if !clipReady {
		return nil, fmt.Errorf("CLIP not ready")
	}

	// Resize to 224×224 bicubic
	dst := image.NewRGBA(image.Rect(0, 0, clipSz, clipSz))
	draw.BiLinear.Scale(dst, dst.Bounds(), img, img.Bounds(), draw.Over, nil)

	// Normalize → [1, 3, H, W] CHW layout
	pixels := make([]float32, 3*clipSz*clipSz)
	for y := 0; y < clipSz; y++ {
		for x := 0; x < clipSz; x++ {
			r, g, b, _ := dst.At(x, y).RGBA()
			i := y*clipSz + x
			pixels[i] = (float32(r>>8)/255.0 - clipMean[0]) / clipStd[0]
			pixels[clipSz*clipSz+i] = (float32(g>>8)/255.0 - clipMean[1]) / clipStd[1]
			pixels[2*clipSz*clipSz+i] = (float32(b>>8)/255.0 - clipMean[2]) / clipStd[2]
		}
	}

	// Run ONNX inference
	inShape := ort.NewShape(1, 3, clipSz, clipSz)
	inTensor, _ := ort.NewTensor(inShape, pixels)
	defer inTensor.Destroy()

	outShape := ort.NewShape(1, clipDim)
	outTensor, _ := ort.NewEmptyTensor[float32](outShape)
	defer outTensor.Destroy()

	sess, err := ort.NewAdvancedSessionWithONNXData(
		nil, // reuse loaded model
		[]string{"pixel_values"}, []string{"image_embeds"},
		[]ort.ArbitraryTensor{inTensor},
		[]ort.ArbitraryTensor{outTensor},
		nil,
	)
	if err != nil {
		// Use shared session
		_ = clipSession
		return nil, fmt.Errorf("session: %w", err)
	}
	if err := sess.Run(); err != nil {
		return nil, fmt.Errorf("run: %w", err)
	}

	raw := outTensor.GetData()
	vec := make([]float32, clipDim)
	copy(vec, raw)
	return l2Normalize(vec), nil
}

func l2Normalize(v []float32) []float32 {
	var norm float64
	for _, x := range v {
		norm += float64(x) * float64(x)
	}
	norm = math.Sqrt(norm)
	if norm == 0 {
		return v
	}
	out := make([]float32, len(v))
	for i, x := range v {
		out[i] = float32(float64(x) / norm)
	}
	return out
}

// ── Model download ─────────────────────────────────────────────────────────────

func ensureModel(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil // already cached
	}
	os.MkdirAll(filepath.Dir(path), 0755)
	fmt.Printf("[CLIP] Downloading model to %s (one-time, ~350MB)...\n", path)

	resp, err := http.Get(clipModelURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	return err
}

func mustReadFile(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		panic(err)
	}
	return b
}
