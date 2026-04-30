package main

import (
	"database/sql"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
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
	db.SetMaxOpenConns(10)               // Allow concurrent reads
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(time.Hour)
	db.Exec("PRAGMA mmap_size=268435456")
	_, err = db.Exec(`CREATE TABLE IF NOT EXISTS designs (
		id          TEXT PRIMARY KEY,
		file_path   TEXT NOT NULL,
		file_name   TEXT NOT NULL,
		format      TEXT NOT NULL,
		size_kb     REAL DEFAULT 0,
		file_mtime  INTEGER DEFAULT 0,
		preview_png BLOB,
		embedding   BLOB NOT NULL,
		indexed_at  INTEGER DEFAULT (strftime('%s','now'))
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_designs_path ON designs(file_path)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_designs_mtime ON designs(file_path, file_mtime, size_kb)")
	return err
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

func dbUpsert(e Entry, png []byte, emb []float32) error {
	writeMu.Lock()
	defer writeMu.Unlock()
	_, err := db.Exec(
		`INSERT OR REPLACE INTO designs
		 (id,file_path,file_name,format,size_kb,file_mtime,preview_png,embedding)
		 VALUES(?,?,?,?,?,?,?,?)`,
		e.ID, e.FilePath, e.FileName, e.Format, e.SizeKB, e.FileMTime, png, f32b(emb),
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
	err := db.QueryRow(
		`SELECT id, file_path, file_name, format, size_kb, file_mtime, (preview_png IS NOT NULL), embedding 
		 FROM designs WHERE id=?`, id,
	).Scan(&e.ID, &e.FilePath, &e.FileName, &e.Format, &e.SizeKB, &e.FileMTime, &e.HasPreview, &raw)
	if err == nil {
		e.Vector = bf32(raw)
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

func dbGetPath(id string) (string, error) {
	var p string
	err := db.QueryRow("SELECT file_path FROM designs WHERE id=?", id).Scan(&p)
	return p, err
}

func dbLoadAll() ([]Entry, error) {
	rows, err := db.Query(
		`SELECT id,file_path,file_name,format,size_kb,file_mtime,
		        (preview_png IS NOT NULL),embedding FROM designs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		var e Entry
		var raw []byte
		rows.Scan(&e.ID, &e.FilePath, &e.FileName, &e.Format,
			&e.SizeKB, &e.FileMTime, &e.HasPreview, &raw)
		e.Vector = bf32(raw)
		out = append(out, e)
	}
	return out, nil
}

func dbCount() int {
	var n int
	db.QueryRow("SELECT COUNT(*) FROM designs").Scan(&n)
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
