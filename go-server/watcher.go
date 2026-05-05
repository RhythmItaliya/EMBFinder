package main

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// ── Filesystem watcher ────────────────────────────────────────────────────────
//
// Real-time index updates for every supported file event:
//
//   Create (new .emb file)        → debounced single-file index
//   Write  (file modified)        → debounced single-file index (re-embed)
//   Remove (file deleted)         → DB + memory removal
//   Rename (out → in pair)        → move detected via content-hash in processOneEmb
//   Create (new directory)        → directory added recursively to watcher
//   Sidecar .jpg/.png change      → re-index the matching .emb (so sidecar vector
//                                   gets refreshed)
//
// Events are debounced per-path with a 750 ms quiet window to coalesce rapid
// writes from editors / save-to-temp + rename patterns.

var (
	watcher       *fsnotify.Watcher
	watchedRoots  = map[string]bool{}
	watchedRootMu sync.Mutex

	// Debounce table: path → pending timer.
	debounceMu sync.Mutex
	debounce   = map[string]*time.Timer{}
)

const debounceWindow = 750 * time.Millisecond

// StartWatcher initialises a recursive filesystem watcher on every selected drive.
func StartWatcher() {
	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("[Watcher] init error: %v", err)
		return
	}

	go watcherLoop()

	// Subscribe to every usable drive.
	for _, d := range autoLibPaths() {
		if usableDrive(d) {
			watcherAddRoot(d.Path)
		}
	}
}

// watcherAddRoot registers a drive root and walks it to add every subdirectory.
// Idempotent — calling twice is a no-op.
func watcherAddRoot(root string) {
	if watcher == nil || root == "" {
		return
	}
	watchedRootMu.Lock()
	if watchedRoots[root] {
		watchedRootMu.Unlock()
		return
	}
	watchedRoots[root] = true
	watchedRootMu.Unlock()

	go addRecursive(root)
}

// watcherRemoveRoot stops watching a drive root and all its descendants.
// No-op if the root was never watched (avoids slow filesystem walk).
func watcherRemoveRoot(root string) {
	if watcher == nil || root == "" {
		return
	}
	watchedRootMu.Lock()
	wasWatched := watchedRoots[root]
	delete(watchedRoots, root)
	watchedRootMu.Unlock()
	if !wasWatched {
		return
	}

	// Walk to remove every subscribed sub-directory.
	go filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			watcher.Remove(p)
		}
		return nil
	})
}

func addRecursive(root string) {
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" ||
				name == "venv" || name == "__pycache__" || name == ".git" {
				return filepath.SkipDir
			}
			watcher.Add(p)
		}
		return nil
	})
}

func watcherLoop() {
	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			handleWatcherEvent(event)
		case err, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.Printf("[Watcher] error: %v", err)
		}
	}
}

func pathOnSelectedDrive(p string) bool {
	return findFolderRoot(p) != ""
}

func findFolderRoot(p string) string {
	watchedRootMu.Lock()
	defer watchedRootMu.Unlock()
	best := ""
	for root := range watchedRoots {
		if strings.HasPrefix(p, root+string(os.PathSeparator)) || p == root {
			if len(root) > len(best) {
				best = root
			}
		}
	}
	return best
}

// findEmbForSidecar returns the .emb path that pairs with a given image
// sidecar (same dir, same base name), or "" if none exists.
func findEmbForSidecar(imgPath string) string {
	dir := filepath.Dir(imgPath)
	base := strings.TrimSuffix(filepath.Base(imgPath), filepath.Ext(imgPath))
	for _, ext := range []string{".emb", ".EMB"} {
		cand := filepath.Join(dir, base+ext)
		if _, err := os.Stat(cand); err == nil {
			return cand
		}
	}
	return ""
}

func handleWatcherEvent(event fsnotify.Event) {
	root := findFolderRoot(event.Name)
	if root == "" {
		return
	}

	// Mark folder as outdated if any file event occurs
	db.Exec("UPDATE folders SET needs_rescan=1, status='Outdated' WHERE path=?", root)

	// New directory? Subscribe recursively (covers nested copies / git clones).
	if event.Has(fsnotify.Create) {
		if info, err := os.Stat(event.Name); err == nil && info.IsDir() {
			go addRecursive(event.Name)
			return
		}
	}

	ext := strings.ToLower(filepath.Ext(event.Name))
	isEmb := ext == ".emb"
	isSidecar := ext == ".jpg" || ext == ".jpeg" || ext == ".png"

	if !isEmb && !isSidecar {
		return
	}
	if !pathOnSelectedDrive(event.Name) {
		return
	}

	// Removal — purge the DB record (or the .emb owning a deleted sidecar).
	if event.Has(fsnotify.Remove) {
		if isEmb {
			log.Printf("[Watcher] removed: %s", event.Name)
			dbRemoveByPath(event.Name)
			globalIndex.RemoveByPrefix(event.Name)
			return
		}
		// Sidecar removed → re-index its .emb (sidecar vector goes away).
		if emb := findEmbForSidecar(event.Name); emb != "" {
			scheduleIndex(emb)
		}
		return
	}

	// Rename — might be a move. We don't delete immediately so processOneEmb
	// can detect the move via content-hash when the new file is created.
	if event.Has(fsnotify.Rename) {
		if !isEmb {
			// If a sidecar is renamed, treat as removal for its .emb
			if emb := findEmbForSidecar(event.Name); emb != "" {
				scheduleIndex(emb)
			}
		}
		return
	}

	// Create / Write — debounce, then index.
	if event.Has(fsnotify.Create) || event.Has(fsnotify.Write) {
		if isEmb {
			scheduleIndex(event.Name)
			return
		}
		// Sidecar add/change → refresh the matching .emb.
		if emb := findEmbForSidecar(event.Name); emb != "" {
			scheduleIndex(emb)
		}
	}
}

// scheduleIndex coalesces rapid events on `path` into a single deferred
// re-index that fires `debounceWindow` after the LAST event.
func scheduleIndex(path string) {
	debounceMu.Lock()
	if t, ok := debounce[path]; ok {
		t.Stop()
	}
	debounce[path] = time.AfterFunc(debounceWindow, func() {
		debounceMu.Lock()
		delete(debounce, path)
		debounceMu.Unlock()
		runWatcherIndex(path)
	})
	debounceMu.Unlock()
}

// runWatcherIndex re-indexes a single .emb file, going through the same
// pipeline used by full scans (cache → hash → render → embed → sidecar).
func runWatcherIndex(path string) {
	if _, err := os.Stat(path); err != nil {
		// File vanished between debounce and run — treat as removal.
		dbRemoveByPath(path)
		globalIndex.RemoveByPrefix(path)
		return
	}
	// Force=true so a Write to an existing file actually re-embeds.
	status := processOneEmb(path, "", true)
	if status == "indexed" || status == "moved" {
		log.Printf("[Watcher] auto-updated: %s (%s)", filepath.Base(path), status)
		RefreshIdxStateCounts()
	}
}
