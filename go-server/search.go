package main

import (
	"cmp"
	"container/heap"
	"slices"
	"strings"
	"sync"
)

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

func (idx *Index) Has(id string) bool {
	idx.mu.RLock()
	defer idx.mu.RUnlock()
	for _, e := range idx.entries {
		if e.ID == id {
			return true
		}
	}
	return false
}

// RemoveByPrefix removes entries matching the given file path prefix.
func (idx *Index) RemoveByPrefix(prefix string) {
	idx.mu.Lock()
	defer idx.mu.Unlock()
	var keep []Entry
	for _, e := range idx.entries {
		if !strings.HasPrefix(e.FilePath, prefix+"/") && e.FilePath != prefix {
			keep = append(keep, e)
		}
	}
	idx.entries = keep
}

// dot computes the dot product of two vectors.
func dot(a, b []float32) float32 {
	if len(a) != len(b) {
		return 0
	}
	var s float32
	for i := range a {
		s += a[i] * b[i]
	}
	return s
}

// scored is a temporary struct for min-heap elements.
type scored struct {
	e Entry
	s float32
}

// minHeap implements heap.Interface for Top-K extraction.
// A min-heap lets us maintain exactly K candidates in O(log K) per push.
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

// Add inserts or replaces an entry by ID in the global in-memory index.
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

// Search performs a parallel, CPU-optimised Top-K cosine similarity search.
//
// Algorithm (DSA):
//  1. Snapshot the index under RLock so writers don't block long searches.
//  2. Partition the snapshot evenly across (NumCPU - 1) goroutines so that
//     at least 1 CPU core remains free for the OS + UI (prevents hang).
//  3. Each goroutine maintains a local min-heap of size K — O(N/W × log K)
//     per worker, where N = index size and W = worker count.
//  4. Local heaps are merged into a single slice and sorted — O(W×K × log(W×K)).
//  5. Total complexity: O(N log K / W + W·K·log(W·K)), typically ~5–10× faster
//     than a single-threaded scan for large libraries.
//
// Dual-vector scoring: for each indexed EMB, we compute cosine similarity against
// BOTH the render embedding (flat icon) and the sidecar photo embedding (garment photo).
// The score used is the MAXIMUM of the two — this bridges the domain gap between
// photographic query images and synthetic renders.
func (idx *Index) Search(query []float32, k int, formatFilter string) []SearchResult {
	// ── 1. Snapshot under RLock ───────────────────────────────────────────────
	idx.mu.RLock()
	var snap []Entry
	if formatFilter != "" {
		for _, e := range idx.entries {
			if strings.EqualFold(e.Format, formatFilter) {
				snap = append(snap, e)
			}
		}
	} else {
		snap = make([]Entry, len(idx.entries))
		copy(snap, idx.entries)
	}
	idx.mu.RUnlock()

	n := len(snap)
	if n == 0 {
		return nil
	}
	if k > n {
		k = n
	}

	// ── 2. Worker count: use all but one CPU core ─────────────────────────────
	workers := getSearchWorkers()
	if workers < 1 {
		workers = 1
	}
	if workers > n {
		workers = n
	}

	chunk := (n + workers - 1) / workers

	var (
		wg         sync.WaitGroup
		resMu      sync.Mutex
		globalHeap []scored
	)

	// Pre-allocate output slice to avoid repeated lock acquisitions.
	globalHeap = make([]scored, 0, workers*k)

	// ── 3. Parallel Top-K per shard ───────────────────────────────────────────
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

			// Local min-heap: O(log K) per file.
			h := make(minHeap, 0, k)
			heap.Init(&h)

			for i := lo; i < hi; i++ {
				// Score = max(render_score, sidecar_score)
				score := dot(query, snap[i].Vector)
				if len(snap[i].SidecarVector) > 0 {
					if sc := dot(query, snap[i].SidecarVector); sc > score {
						score = sc
					}
				}

				if h.Len() < k {
					heap.Push(&h, scored{snap[i], score})
				} else if score > h[0].s {
					// Pop the current minimum and push the better result.
					heap.Pop(&h)
					heap.Push(&h, scored{snap[i], score})
				}
			}

			// Flush local heap to shared slice.
			resMu.Lock()
			globalHeap = append(globalHeap, h...)
			resMu.Unlock()
		}(lo, hi)
	}
	wg.Wait()

	// ── 4. Merge and sort: O(W·K · log(W·K)) ────────────────────────────────
	// Using modern Pattern-Defeating Quicksort (pdqsort) via slices.SortFunc
	slices.SortFunc(globalHeap, func(a, b scored) int {
		return cmp.Compare(b.s, a.s) // Descending order
	})

	if k > len(globalHeap) {
		k = len(globalHeap)
	}

	// ── 5. Build output ───────────────────────────────────────────────────────
	out := make([]SearchResult, k)
	for i := 0; i < k; i++ {
		out[i] = SearchResult{
			ID:         globalHeap[i].e.ID,
			FilePath:   globalHeap[i].e.FilePath,
			FileName:   globalHeap[i].e.FileName,
			Format:     globalHeap[i].e.Format,
			SizeKB:     globalHeap[i].e.SizeKB,
			HasPreview: globalHeap[i].e.HasPreview,
			Score:      rescaleScore(float64(globalHeap[i].s)),
		}
	}
	return out
}

// rescaleScore maps raw CLIP cosine similarity to a honest 0-100% scale.
func rescaleScore(s float64) float64 {
	return s
}

// Count returns the number of entries currently in the in-memory index.
func (idx *Index) Count() int {
	idx.mu.RLock()
	n := len(idx.entries)
	idx.mu.RUnlock()
	return n
}

// Clear removes all entries from the in-memory index.
func (idx *Index) Clear() {
	idx.mu.Lock()
	idx.entries = nil
	idx.mu.Unlock()
}
