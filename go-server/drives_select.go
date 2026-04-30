package main

import (
	"encoding/json"
	"net/http"
	"sync"
)

// ── Selected drives state ─────────────────────────────────────────────────────
// Stores which drives the user has checked in the UI.
// Default: ALL usable drives are selected.

var driveSelMu sync.RWMutex
var selectedDrivePaths map[string]bool // nil = all selected (default)

// isSelected returns true if the given drive path should be scanned.
// If no selection has been made yet (nil map), no drives are selected.
func isSelected(path string) bool {
	driveSelMu.RLock()
	defer driveSelMu.RUnlock()
	if selectedDrivePaths == nil {
		return false // default: none selected
	}
	return selectedDrivePaths[path]
}

// getDrivesToScan returns the filtered, usable, selected drives to index.
func getDrivesToScan() []DriveEntry {
	all := autoLibPaths()
	var out []DriveEntry
	for _, d := range all {
		if usableDrive(d) && isSelected(d.Path) {
			out = append(out, d)
		}
	}
	return out
}

// ── Per-drive DB counts ───────────────────────────────────────────────────────

// dbCountByDrivePrefix returns how many indexed designs live under each drive path.
func dbCountByDrive(drives []DriveEntry) map[string]int {
	counts := make(map[string]int, len(drives))
	for _, d := range drives {
		var n int
		db.QueryRow(
			"SELECT COUNT(*) FROM designs WHERE file_path LIKE ?",
			d.Path+"/%",
		).Scan(&n)
		counts[d.Path] = n
	}
	return counts
}

// ── HTTP handlers ─────────────────────────────────────────────────────────────

// GET /api/drives/list — returns all drives with usable flag, selected flag,
// and indexed count per drive.
func hDriveList(w http.ResponseWriter, r *http.Request) {
	RegisterHeartbeat()
	all := autoLibPaths()
	type driveInfo struct {
		Path     string `json:"path"`
		Label    string `json:"label"`
		FSType   string `json:"fs_type,omitempty"`
		Usable   bool   `json:"usable"`
		Selected bool   `json:"selected"`
		Indexed  int    `json:"indexed"`
	}

	usable := []DriveEntry{}
	for _, d := range all {
		if usableDrive(d) {
			usable = append(usable, d)
		}
	}
	counts := dbCountByDrive(usable)

	infos := make([]driveInfo, len(all))
	for i, d := range all {
		u := usableDrive(d)
		infos[i] = driveInfo{
			Path:     d.Path,
			Label:    d.Label,
			FSType:   d.FSType,
			Usable:   u,
			Selected: !u || isSelected(d.Path), // non-usable shown but not selectable
			Indexed:  counts[d.Path],
		}
	}
	writeJSON(w, map[string]interface{}{
		"drives": infos,
	})
}

// POST /api/drives/select — body: {"paths": ["/path/a", "/path/b"]}
// Sets which drives to include in scans.
func hDriveSelect(w http.ResponseWriter, r *http.Request) {
	RegisterHeartbeat()
	var body struct {
		Paths []string `json:"paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad JSON", 400)
		return
	}
	driveSelMu.Lock()
	selectedDrivePaths = make(map[string]bool, len(body.Paths))
	for _, p := range body.Paths {
		selectedDrivePaths[p] = true
	}
	driveSelMu.Unlock()

	// Clean up unselected drives from database and memory
	all := autoLibPaths()
	for _, d := range all {
		if usableDrive(d) && !isSelected(d.Path) {
			db.Exec("DELETE FROM designs WHERE file_path LIKE ?", d.Path+"/%")
			db.Exec("DELETE FROM designs WHERE file_path=?", d.Path)
			globalIndex.RemoveByPrefix(d.Path)
		}
	}
	RefreshIdxStateCounts()

	writeJSON(w, map[string]interface{}{"selected": body.Paths})
}

// hIndexStart manually triggers an immediate index scan of selected drives.
func hIndexStart(w http.ResponseWriter, r *http.Request) {
	RegisterHeartbeat()

	drives := getDrivesToScan()
	if len(drives) == 0 {
		writeJSON(w, map[string]interface{}{
			"status": "no_drives",
			"msg":    "Select at least one drive to scan.",
		})
		return
	}

	if idxState.Running {
		writeJSON(w, map[string]interface{}{"status": "already_running"})
		return
	}

	idxState.mu.Lock()
	idxState.UserPaused = false
	idxState.mu.Unlock()

	// Trigger the central AutoIndexAllDrives loop to wake up and scan immediately
	select {
	case triggerScan <- struct{}{}:
	default:
	}

	writeJSON(w, map[string]interface{}{"status": "started"})
}

func drivePaths(drives []DriveEntry) []string {
	out := make([]string, len(drives))
	for i, d := range drives {
		out[i] = d.Path
	}
	return out
}
