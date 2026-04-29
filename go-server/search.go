package main

import (
	"container/heap"
	"math"
	"runtime"
	"sort"
	"sync"
)

// Entry is one indexed design in memory.
type Entry struct {
	ID         string
	FilePath   string
	FileName   string
	Format     string
	SizeKB     float64
	HasPreview bool
	Vector     []float32 // 512-dim CLIP, unit-normalized
}

// SearchResult is returned to the browser.
type SearchResult struct {
	ID         string  `json:"id"`
	FilePath   string  `json:"file_path"`
	FileName   string  `json:"file_name"`
	Format     string  `json:"format"`
	SizeKB     float64 `json:"size_kb"`
	Score      float64 `json:"score"`
	HasPreview bool    `json:"has_preview"`
}

// Index holds all vectors in memory for parallel cosine search.
type Index struct {
	mu      sync.RWMutex
	entries []Entry
}

var globalIndex = &Index{}

// dot is a 4x-unrolled dot product (SIMD-friendly, ~4x faster than naive).
func dot(a, b []float32) float32 {
	var s float32
	n := len(a)
	if n > len(b) {
		n = len(b)
	}
	i := 0
	for ; i+3 < n; i += 4 {
		s += a[i]*b[i] + a[i+1]*b[i+1] + a[i+2]*b[i+2] + a[i+3]*b[i+3]
	}
	for ; i < n; i++ {
		s += a[i] * b[i]
	}
	return s
}

// scored is a temporary struct for heap elements.
type scored struct {
	e Entry
	s float32
}

// minHeap implements heap.Interface for Top-K extraction.
type minHeap []scored

func (h minHeap) Len() int           { return len(h) }
func (h minHeap) Less(i, j int) bool { return h[i].s < h[j].s }
func (h minHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *minHeap) Push(x interface{}) {
	*h = append(*h, x.(scored))
}
func (h *minHeap) Pop() interface{} {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

// Add inserts or replaces an entry by ID.
func (idx *Index) Add(e Entry) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	for i, ex := range idx.entries {
		if ex.ID == e.ID {
			idx.entries[i] = e
			return
		}
	}
	idx.entries = append(idx.entries, e)
}

// Search returns top-k by cosine similarity using DSA Min-Heap logic and prevents PC hanging.
func (idx *Index) Search(query []float32, k int) []SearchResult {
	idx.mu.RLock()
	snap := make([]Entry, len(idx.entries))
	copy(snap, idx.entries)
	idx.mu.RUnlock()

	n := len(snap)
	if n == 0 {
		return nil
	}

	// Task Division: Leave at least 1 core free to prevent OS hang.
	workers := runtime.NumCPU() - 1
	if workers < 1 {
		workers = 1
	}

	chunk := (n + workers - 1) / workers
	var wg sync.WaitGroup

	var resMu sync.Mutex
	var globalResults []scored

	for w := 0; w < workers; w++ {
		lo := w * chunk
		hi := lo + chunk
		if hi > n {
			hi = n
		}
		if lo >= n {
			break
		}
		wg.Add(1)
		go func(lo, hi int) {
			defer wg.Done()

			// Use binary heap logic to keep top-K per worker chunk.
			h := &minHeap{}
			heap.Init(h)

			for i := lo; i < hi; i++ {
				score := dot(query, snap[i].Vector)
				if h.Len() < k {
					heap.Push(h, scored{snap[i], score})
				} else if score > (*h)[0].s {
					heap.Pop(h)
					heap.Push(h, scored{snap[i], score})
				}
			}

			resMu.Lock()
			for _, item := range *h {
				globalResults = append(globalResults, item)
			}
			resMu.Unlock()
		}(lo, hi)
	}
	wg.Wait()

	// Final sort of merged top-K results (O(Workers * K log (Workers * K)))
	sort.Slice(globalResults, func(i, j int) bool { return globalResults[i].s > globalResults[j].s })

	if k > len(globalResults) {
		k = len(globalResults)
	}

	out := make([]SearchResult, k)
	for i := 0; i < k; i++ {
		out[i] = SearchResult{
			ID:         globalResults[i].e.ID,
			FilePath:   globalResults[i].e.FilePath,
			FileName:   globalResults[i].e.FileName,
			Format:     globalResults[i].e.Format,
			SizeKB:     globalResults[i].e.SizeKB,
			HasPreview: globalResults[i].e.HasPreview,
			Score:      math.Round(float64(globalResults[i].s)*10000) / 10000,
		}
	}
	return out
}

func (idx *Index) Count() int {
	idx.mu.RLock()
	n := len(idx.entries)
	idx.mu.RUnlock()
	return n
}

func (idx *Index) Clear() {
	idx.mu.Lock()
	idx.entries = nil
	idx.mu.Unlock()
}
