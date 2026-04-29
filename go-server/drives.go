package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// autoLibPaths returns auto-detected embroidery/image library paths.
// No user input required — loads from system drives.
func autoLibPaths() []DriveEntry {
	switch runtime.GOOS {
	case "linux":
		return linuxDrives()
	case "darwin":
		return macDrives()
	case "windows":
		return windowsDrives()
	}
	return nil
}

// DriveEntry describes one auto-detected drive or mount.
type DriveEntry struct {
	Path   string `json:"path"`
	Label  string `json:"label"`
	FSType string `json:"fs_type,omitempty"`
	Free   int64  `json:"free_mb,omitempty"`
}

// ── Linux: read /proc/mounts ──────────────────────────────────────────────────
var skipFS = map[string]bool{
	"proc": true, "sysfs": true, "devtmpfs": true, "devpts": true,
	"tmpfs": true, "cgroup": true, "cgroup2": true, "pstore": true,
	"bpf": true, "tracefs": true, "debugfs": true, "mqueue": true,
	"hugetlbfs": true, "fusectl": true, "overlay": true, "securityfs": true,
}

func linuxDrives() []DriveEntry {
	var entries []DriveEntry
	seen := map[string]bool{}

	// Always include home dir
	home, _ := os.UserHomeDir()
	if home != "" && !seen[home] {
		entries = append(entries, DriveEntry{Path: home, Label: "Home (" + home + ")"})
		seen[home] = true
	}

	f, err := os.Open("/proc/mounts")
	if err != nil {
		return entries
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		parts := strings.Fields(scanner.Text())
		if len(parts) < 3 {
			continue
		}
		mountPt := parts[1]
		fsType := parts[2]
		if skipFS[fsType] {
			continue
		}
		// Skip kernel/system paths
		for _, pfx := range []string{"/sys", "/proc", "/dev", "/run/lock", "/snap", "/boot", "/run/snapd"} {
			if strings.HasPrefix(mountPt, pfx) {
				goto next
			}
		}
		if seen[mountPt] {
			continue
		}
		seen[mountPt] = true
		entries = append(entries, DriveEntry{Path: mountPt, Label: driveLabel(mountPt), FSType: fsType})
	next:
	}
	return entries
}

// ── macOS: /Volumes ───────────────────────────────────────────────────────────
func macDrives() []DriveEntry {
	var entries []DriveEntry
	home, _ := os.UserHomeDir()
	if home != "" {
		entries = append(entries, DriveEntry{Path: home, Label: "Home"})
	}
	entries = append(entries, DriveEntry{Path: "/", Label: "Macintosh HD"})
	dirs, _ := os.ReadDir("/Volumes")
	for _, d := range dirs {
		if d.IsDir() {
			p := filepath.Join("/Volumes", d.Name())
			entries = append(entries, DriveEntry{Path: p, Label: d.Name()})
		}
	}
	return entries
}

// ── Windows: scan A–Z drive letters ──────────────────────────────────────────
func windowsDrives() []DriveEntry {
	var entries []DriveEntry
	for c := 'C'; c <= 'Z'; c++ {
		path := fmt.Sprintf("%c:\\", c)
		if _, err := os.Stat(path); err == nil {
			label := fmt.Sprintf("Drive %c:", c)
			entries = append(entries, DriveEntry{Path: path, Label: label})
		}
	}
	return entries
}

func driveLabel(path string) string {
	switch path {
	case "/":
		return "Root (/)"
	case "/home":
		return "Home (/home)"
	}
	if strings.HasPrefix(path, "/media/") || strings.HasPrefix(path, "/mnt/") ||
		strings.HasPrefix(path, "/run/media/") {
		return "📀 " + filepath.Base(path)
	}
	return path
}
