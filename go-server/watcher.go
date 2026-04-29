package main

import (
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/fsnotify/fsnotify"
)

var watcher *fsnotify.Watcher

// StartWatcher initializes a recursive filesystem watcher on all auto-detected drives.
func StartWatcher() {
	var err error
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		log.Printf("Watcher Error: %v", err)
		return
	}

	go func() {
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
				log.Printf("Watcher Event Error: %v", err)
			}
		}
	}()

	// Add all drives to watcher
	drives := autoLibPaths()
	for _, d := range drives {
		if d.Path == "/" || strings.HasPrefix(d.Path, "/home") {
			// Don't watch root or home (too many files/events)
			continue
		}
		addRecursive(d.Path)
	}
}

func addRecursive(path string) {
	err := filepath.WalkDir(path, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			// Skip hidden dirs and system dirs
			if strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "venv" {
				return filepath.SkipDir
			}
			watcher.Add(p)
		}
		return nil
	})
	if err != nil {
		log.Printf("Watcher: Failed to add path %s: %v", path, err)
	}
}

func handleWatcherEvent(event fsnotify.Event) {
	ext := strings.ToLower(filepath.Ext(event.Name))
	if !isSupportedExt(ext) && !event.Has(fsnotify.Create) {
		// If it's a directory creation, we might want to watch it
		info, err := os.Stat(event.Name)
		if err == nil && info.IsDir() && event.Has(fsnotify.Create) {
			watcher.Add(event.Name)
		}
		return
	}

	if event.Has(fsnotify.Write) || event.Has(fsnotify.Create) {
		// File added or updated
		log.Printf("Auto-Updating: %s", event.Name)
		// Trigger a single-file index (StartIndexing handles one file too)
		go indexSingleFile(event.Name)
	}

	if event.Has(fsnotify.Remove) || event.Has(fsnotify.Rename) {
		// File deleted or moved
		log.Printf("Auto-Removing: %s", event.Name)
		dbRemoveByPath(event.Name)
	}
}

func indexSingleFile(path string) {
	// Simple wrapper for indexing a single file
	id := fileID(path)
	
	// Get preview and embedding
	result, err := callEmbedFile(path)
	if err != nil {
		return
	}

	var png []byte
	if strings.ToLower(filepath.Ext(path)) == ".emb" {
		png = callEmbEnginePreview(path)
	}
	if png == nil && result.PreviewB64 != "" {
		png, _ = base64.StdEncoding.DecodeString(result.PreviewB64)
	}

	info, _ := os.Stat(path)
	sizeKB := 0.0
	if info != nil {
		sizeKB = float64(info.Size()) / 1024
	}

	e := Entry{
		ID:         id,
		FilePath:   path,
		FileName:   filepath.Base(path),
		Format:     strings.TrimPrefix(strings.ToLower(filepath.Ext(path)), "."),
		SizeKB:     sizeKB,
		HasPreview: png != nil,
	}
	if err := dbUpsert(e, png, result.Embedding); err == nil {
		e.Vector = result.Embedding
		globalIndex.Add(e)
	}
}
