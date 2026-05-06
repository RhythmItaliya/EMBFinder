package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	db      *sql.DB
	writeMu sync.Mutex
)

func initDB(path string) error {
	os.MkdirAll(filepath.Dir(path), 0755)
	var err error
	db, err = sql.Open("sqlite", path)
	if err != nil {
		return fmt.Errorf("db open: %w", err)
	}
	db.Exec("PRAGMA journal_mode=WAL")
	db.Exec("PRAGMA synchronous=NORMAL")
	db.Exec("PRAGMA busy_timeout=5000") // Wait up to 5s if DB locked
	db.Exec("PRAGMA cache_size=-32000") // 32MB cache
	db.SetMaxOpenConns(10)              // Allow concurrent reads
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	db.Exec("PRAGMA mmap_size=268435456")
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS designs (
		id                TEXT PRIMARY KEY,
		file_path         TEXT NOT NULL,
		file_name         TEXT NOT NULL,
		format            TEXT NOT NULL,
		size_kb           REAL DEFAULT 0,
		file_mtime        INTEGER DEFAULT 0,
		preview_png       BLOB,
		thumbnail         BLOB,
		embedding         BLOB NOT NULL,
		sidecar_embedding BLOB,
		indexed_at        INTEGER DEFAULT (strftime('%s','now'))
	)`)

	// Migrations for existing DBs
	db.Exec("ALTER TABLE designs ADD COLUMN thumbnail BLOB")
	db.Exec("ALTER TABLE designs ADD COLUMN sidecar_embedding BLOB")
	db.Exec("ALTER TABLE folders ADD COLUMN needs_rescan INTEGER DEFAULT 0")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_designs_path ON designs(file_path)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_designs_mtime ON designs(file_path, file_mtime, size_kb)")
	// UNIQUE constraint prevents duplicate file_path rows (different hash IDs same path)
	db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_designs_unique_path ON designs(file_path)")

	// Per-folder tracking: records independent progress and status for each selected root.
	db.Exec(`CREATE TABLE IF NOT EXISTS folders (
		path           TEXT PRIMARY KEY,
		name           TEXT NOT NULL,
		total_files    INTEGER DEFAULT 0,
		indexed_files  INTEGER DEFAULT 0,
		last_file      TEXT,
		status         TEXT DEFAULT 'Pending',
		needs_rescan   INTEGER DEFAULT 0,
		last_scan      INTEGER DEFAULT 0,
		updated_at     INTEGER DEFAULT (strftime('%s','now'))
	)`)

	// Legacy support / checkpoint data
	db.Exec(`CREATE TABLE IF NOT EXISTS scan_progress (
		drive_path     TEXT PRIMARY KEY,
		last_file      TEXT,
		processed      INTEGER DEFAULT 0,
		updated_at     INTEGER DEFAULT (strftime('%s','now'))
	)`)
	if err != nil {
		return err
	}
	return nil
}

// dbStartupCleanup runs dedup + folder-count recalc in the background so the
// server can accept connections immediately on startup.
func dbStartupCleanup() {
	go func() {
		log.Printf("[DB] Running startup cleanup (dedup + folder count recalc)...")
		dbResetStuckFolders()
		dbDeduplicatePaths()
		dbRecalcAllFolderCounts()
		dbFixCompletedStatus()
		log.Printf("[DB] Startup cleanup complete")
	}()
}

// dbFixCompletedStatus marks any folder where indexed_files >= total_files
// (and total_files > 0) as 'Completed', repairing folders stuck in
// 'Pending', 'Stopped', or 'Scouting...' even though they are fully indexed.
func dbFixCompletedStatus() {
	writeMu.Lock()
	defer writeMu.Unlock()
	res, err := db.Exec(`
		UPDATE folders
		SET status = 'Completed',
		    needs_rescan = 0,
		    updated_at = strftime('%s','now')
		WHERE total_files > 0
		  AND indexed_files >= total_files
		  AND status IN ('Pending','Stopped','Scouting...','Error')
	`)
	if err == nil {
		n, _ := res.RowsAffected()
		if n > 0 {
			log.Printf("[DB] Auto-completed %d fully-indexed folders", n)
		}
	}
}

func f32b(v []float32) []byte {
	b := make([]byte, len(v)*4)
	for i, f := range v {
		binary.LittleEndian.PutUint32(b[i*4:], math.Float32bits(f))
	}
	return b
}

func bf32(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return v
}

// dbUpsert stores an entry with its render preview and embedding vector.
// For backward compatibility; new code should use dbUpsertFull.
func dbUpsert(e Entry, png []byte, emb []float32) error {
	return dbUpsertFull(e, png, nil, emb)
}

// dbUpsertFull stores an entry with separate preview (EMB render), thumbnail (sidecar photo),
// render embedding, and optionally a sidecar-photo embedding.
// preview          = the computer-generated EMB render PNG  → card image in search results
// thumb            = sidecar garment photo                  → shown in modal, may be nil
// emb              = CLIP embedding of the render           → cosine similarity search
// sidecarEmb       = CLIP embedding of the sidecar photo    → improves accuracy when available
func dbUpsertFull(e Entry, preview, thumb []byte, emb []float32) error {
	return dbUpsertDual(e, preview, thumb, emb, nil)
}

func dbUpsertDual(e Entry, preview, thumb []byte, emb, sidecarEmb []float32) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	var sidecarBlob []byte
	if len(sidecarEmb) > 0 {
		sidecarBlob = f32b(sidecarEmb)
	}
	// ON CONFLICT on file_path: update all fields so the same physical file
	// never accumulates multiple rows even if the content-hash changes.
	_, err := db.Exec(
		`INSERT INTO designs
		 (id,file_path,file_name,format,size_kb,file_mtime,preview_png,thumbnail,embedding,sidecar_embedding)
		 VALUES(?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(file_path) DO UPDATE SET
		   id=excluded.id,
		   file_name=excluded.file_name,
		   format=excluded.format,
		   size_kb=excluded.size_kb,
		   file_mtime=excluded.file_mtime,
		   preview_png=COALESCE(excluded.preview_png, preview_png),
		   thumbnail=COALESCE(excluded.thumbnail, thumbnail),
		   embedding=excluded.embedding,
		   sidecar_embedding=COALESCE(excluded.sidecar_embedding, sidecar_embedding),
		   indexed_at=strftime('%s','now')`,
		e.ID, e.FilePath, e.FileName, e.Format, e.SizeKB, e.FileMTime, preview, thumb, f32b(emb), sidecarBlob,
	)
	return err
}

// dbCheckCache returns true if the file at path with mtime and size is already indexed.
func dbCheckCache(path string, mtime int64, size int64) (string, bool) {
	var id string
	err := db.QueryRow(
		"SELECT id FROM designs WHERE file_path=? AND file_mtime=? AND size_kb=?",
		path, mtime, float64(size)/1024,
	).Scan(&id)
	return id, err == nil
}

// dbGetByHash checks if this content-DNA is already indexed under ANY path.
func dbGetByHash(id string) (Entry, bool) {
	var e Entry
	var raw []byte
	var sidecarRaw []byte
	err := db.QueryRow(
		`SELECT id, file_path, file_name, format, size_kb, file_mtime, (preview_png IS NOT NULL), embedding, sidecar_embedding
		 FROM designs WHERE id=?`, id,
	).Scan(&e.ID, &e.FilePath, &e.FileName, &e.Format, &e.SizeKB, &e.FileMTime, &e.HasPreview, &raw, &sidecarRaw)
	if err == nil {
		e.Vector = bf32(raw)
		if len(sidecarRaw) > 0 {
			e.SidecarVector = bf32(sidecarRaw)
		}
		return e, true
	}
	return e, false
}

func dbIndexed(id string) bool {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM designs WHERE id=?", id).Scan(&n)
	return n > 0
}

func dbPreview(id string) ([]byte, error) {
	var png []byte
	err := db.QueryRow("SELECT preview_png FROM designs WHERE id=?", id).Scan(&png)
	return png, err
}

func dbThumbnail(id string) ([]byte, error) {
	var thumb []byte
	// Returns thumbnail (garment photo) if stored, otherwise falls back to preview_png
	err := db.QueryRow("SELECT COALESCE(thumbnail, preview_png) FROM designs WHERE id=?", id).Scan(&thumb)
	return thumb, err
}

func dbGetPath(id string) (string, error) {
	var p string
	err := db.QueryRow("SELECT file_path FROM designs WHERE id=?", id).Scan(&p)
	return p, err
}

func dbLoadAll() ([]Entry, error) {
	rows, err := db.Query(
		`SELECT id,file_path,file_name,format,size_kb,file_mtime,
		        (preview_png IS NOT NULL),embedding,sidecar_embedding FROM designs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var raw []byte
		var sidecarRaw []byte
		rows.Scan(&e.ID, &e.FilePath, &e.FileName, &e.Format,
			&e.SizeKB, &e.FileMTime, &e.HasPreview, &raw, &sidecarRaw)
		e.Vector = bf32(raw)
		if len(sidecarRaw) > 0 {
			e.SidecarVector = bf32(sidecarRaw)
		}
		out = append(out, e)
	}
	return out, nil
}

func dbCount() int {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM designs").Scan(&n)
	return n
}

func dbCountForPath(path string) int {
	var n int
	// Use '/' separator so 'SINGLEHEAD-25' does NOT match 'SINGLEHEAD-25 (COD)'
	db.QueryRow("SELECT COUNT(*) FROM designs WHERE file_path LIKE ?", path+"/%").Scan(&n)
	return n
}

func dbClear() int {
	n := dbCount()
	db.Exec("DELETE FROM designs")
	return n
}

func dbRemoveByPath(path string) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	_, err := db.Exec("DELETE FROM designs WHERE file_path=?", path)
	return err
}

func dbUpdateFileMetadata(id string, newPath string, newName string, mtime int64, sizeKB float64) {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec("UPDATE designs SET file_path=?, file_name=?, file_mtime=?, size_kb=? WHERE id=?", newPath, newName, mtime, sizeKB, id)
}

func dbClearAll() error {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec("DELETE FROM folders")
	db.Exec("DELETE FROM scan_progress")
	_, err := db.Exec("DELETE FROM designs")
	return err
}

func dbGetFormatCounts() map[string]int {
	rows, err := db.Query("SELECT format, COUNT(*) FROM designs GROUP BY format")
	if err != nil {
		return nil
	}
	defer rows.Close()
	res := make(map[string]int)
	for rows.Next() {
		var fmt string
		var count int
		if err := rows.Scan(&fmt, &count); err == nil {
			res[fmt] = count
		}
	}
	return res
}

// dbSaveProgress upserts a per-drive checkpoint. lastFile may be empty
// (final flush); processed is monotonic across runs (additive when called
// repeatedly within the same run, since the indexer only counts files it
// actually attempted in this run).
func dbSaveProgress(drive, lastFile string, processed int) {
	if drive == "" {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec(`INSERT INTO scan_progress(drive_path, last_file, processed, updated_at)
	         VALUES(?,?,?,strftime('%s','now'))
	         ON CONFLICT(drive_path) DO UPDATE SET
	           last_file=excluded.last_file,
	           processed=excluded.processed,
	           updated_at=excluded.updated_at`,
		drive, lastFile, processed)
}

// dbLoadProgress returns the last checkpoint for `drive`, if one exists.
func dbLoadProgress(drive string) (lastFile string, processed int, ok bool) {
	err := db.QueryRow(
		"SELECT last_file, processed FROM scan_progress WHERE drive_path=?", drive,
	).Scan(&lastFile, &processed)
	return lastFile, processed, err == nil
}

// dbClearProgress wipes the checkpoint for one drive (used when the user
// unselects it or clears the index).
func dbClearProgress(drive string) {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec("DELETE FROM scan_progress WHERE drive_path=?", drive)
	db.Exec("DELETE FROM folders WHERE path=?", drive)
}

type FolderStats struct {
	Path         string `json:"path"`
	Name         string `json:"name"`
	TotalFiles   int    `json:"total_files"`
	IndexedFiles int    `json:"indexed_files"`
	LastFile     string `json:"last_file"`
	Status       string `json:"status"`
	NeedsRescan  bool   `json:"needs_rescan"`
	LastScan     int64  `json:"last_scan"`
}

func dbSaveFolder(s FolderStats) {
	writeMu.Lock()
	defer writeMu.Unlock()

	var existingStatus string
	_ = db.QueryRow("SELECT status FROM folders WHERE path=?", s.Path).Scan(&existingStatus)

	// Preserve total_files if caller didn't set it
	if s.TotalFiles == 0 {
		db.QueryRow("SELECT total_files FROM folders WHERE path=?", s.Path).Scan(&s.TotalFiles)
	}

	// Background discovery (Scouting... / Pending) must NEVER overwrite a real
	// scan state (In Progress, Completed, Stopped, Error).
	if existingStatus != "" &&
		!strings.EqualFold(existingStatus, "Scouting...") &&
		!strings.EqualFold(existingStatus, "Pending") {
		if strings.EqualFold(s.Status, "Scouting...") || strings.EqualFold(s.Status, "Pending") {
			s.Status = existingStatus
		}
	}

	db.Exec(`INSERT INTO folders(path, name, total_files, indexed_files, last_file, status, needs_rescan, last_scan, updated_at)
	         VALUES(?,?,?,?,?,?,?,?,strftime('%s','now'))
	         ON CONFLICT(path) DO UPDATE SET
	           name=excluded.name,
	           total_files=excluded.total_files,
	           indexed_files=excluded.indexed_files,
	           last_file=excluded.last_file,
	           status=excluded.status,
	           needs_rescan=excluded.needs_rescan,
	           last_scan=excluded.last_scan,
	           updated_at=excluded.updated_at`,
		s.Path, s.Name, s.TotalFiles, s.IndexedFiles, s.LastFile, s.Status, s.NeedsRescan, s.LastScan)
}

func dbGetTotalForFolder(path string) int32 {
	var n int32
	db.QueryRow("SELECT total_files FROM folders WHERE path=?", path).Scan(&n)
	return n
}

func dbLoadFolders() ([]FolderStats, error) {
	rows, err := db.Query("SELECT path, name, total_files, indexed_files, last_file, status, needs_rescan, last_scan FROM folders")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FolderStats
	for rows.Next() {
		var s FolderStats
		rows.Scan(&s.Path, &s.Name, &s.TotalFiles, &s.IndexedFiles, &s.LastFile, &s.Status, &s.NeedsRescan, &s.LastScan)
		out = append(out, s)
	}
	return out, nil
}

func dbSetFolderStatus(path string, status string) {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec("UPDATE folders SET status=?, updated_at=strftime('%s','now') WHERE path=?", status, path)
}

func dbStopAllFolders() {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec("UPDATE folders SET status='Stopped', updated_at=strftime('%s','now') WHERE status='In Progress'")
}

// dbRefreshFolderStatsForPath refreshes indexed_files for every folder that
// could contain the given file path.
func dbRefreshFolderStatsForPath(filePath string) {
	if filePath == "" {
		return
	}
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec(`
		UPDATE folders
		   SET indexed_files = (
		     SELECT COUNT(*) FROM designs d
		      WHERE d.file_path = folders.path OR d.file_path LIKE folders.path || '/%'
		   ),
		       updated_at = strftime('%s','now')
		 WHERE ? = folders.path OR ? LIKE folders.path || '/%'`,
		filePath, filePath)
}

func dbHasPath(path string) bool {
	var n int
	_ = db.QueryRow("SELECT COUNT(*) FROM designs WHERE file_path=?", path).Scan(&n)
	return n > 0
}

// dbDeduplicatePaths removes duplicate file_path rows keeping the best row
// (one with preview_png; if tie, most recently indexed).
// Safe to call on startup before the UNIQUE index migration.
func dbDeduplicatePaths() {
	writeMu.Lock()
	defer writeMu.Unlock()
	// Step 1: for each duplicated path, delete rows that have no preview
	db.Exec(`
		DELETE FROM designs
		WHERE file_path IN (
			SELECT file_path FROM designs GROUP BY file_path HAVING COUNT(*) > 1
		)
		AND preview_png IS NULL
	`)
	// Step 2: keep only the most recently indexed row per path
	db.Exec(`
		DELETE FROM designs
		WHERE rowid NOT IN (
			SELECT MAX(rowid) FROM designs GROUP BY file_path
		)
	`)
}

// dbResetStuckFolders marks any folder that was 'In Progress' at startup as
// 'Stopped' (the server crashed / was killed mid-scan).
func dbResetStuckFolders() {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec(`UPDATE folders SET status='Stopped', needs_rescan=1,
			updated_at=strftime('%s','now') WHERE status='In Progress'`)
}

// dbRecalcAllFolderCounts recomputes indexed_files for every folder row from
// the actual designs table. Call after deduplication or a full clear.
func dbRecalcAllFolderCounts() {
	writeMu.Lock()
	defer writeMu.Unlock()
	db.Exec(`
		UPDATE folders
		SET indexed_files = (
			SELECT COUNT(*) FROM designs d
			WHERE d.file_path LIKE folders.path || '/%'
		),
		updated_at = strftime('%s','now')
	`)
}
