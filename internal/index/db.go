// Package index owns the SQLite database: schema, incremental ingestion and
// maintenance.
package index

import (
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

// SchemaVersion is bumped whenever the schema changes incompatibly; an
// index with a different version is dropped and rebuilt from scratch.
const SchemaVersion = 1

// Truncation limits for tool_use / tool_result text, in characters.
const (
	TruncateDefault = 2000
	TruncateFull    = 40000
)

// DB wraps the index database.
type DB struct {
	*sql.DB
	Path string
}

// DefaultPath returns $AGENTORY_DB, or the XDG data location.
func DefaultPath() string {
	if p := os.Getenv("AGENTORY_DB"); p != "" {
		return p
	}
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return filepath.Join(d, "agentory", "index.db")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".agentory", "index.db")
	}
	return filepath.Join(home, ".local", "share", "agentory", "index.db")
}

// Open opens (creating if needed) the index at path.
func Open(path string) (*DB, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "busy_timeout(15000)")
	q.Add("_pragma", "foreign_keys(OFF)")
	q.Add("_pragma", "cache_size(-65536)")
	q.Add("_txlock", "immediate")
	sqlDB, err := sql.Open("sqlite", "file:"+uriPath(path)+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// A single connection keeps writes serialized and pragmas consistent.
	sqlDB.SetMaxOpenConns(1)
	db := &DB{DB: sqlDB, Path: path}
	if err := db.migrate(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	return db, nil
}

// uriPath turns a filesystem path into the path part of an SQLite URI
// (forward slashes, with the URI delimiters escaped).
func uriPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(filepath.ToSlash(p))
}

const schema = `
CREATE TABLE IF NOT EXISTS meta(
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS files(
  id       INTEGER PRIMARY KEY,
  source   TEXT    NOT NULL,
  path     TEXT    NOT NULL UNIQUE,
  size     INTEGER NOT NULL,
  mtime    INTEGER NOT NULL,
  byte_off INTEGER NOT NULL,
  n_msg    INTEGER NOT NULL,
  full     INTEGER NOT NULL,
  head_crc INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS sessions(
  id           TEXT PRIMARY KEY,
  source       TEXT    NOT NULL,
  project      TEXT    NOT NULL DEFAULT '',
  cwd          TEXT    NOT NULL DEFAULT '',
  branch       TEXT    NOT NULL DEFAULT '',
  title        TEXT    NOT NULL DEFAULT '',
  title_rank   INTEGER NOT NULL DEFAULT 0,
  first_prompt TEXT    NOT NULL DEFAULT '',
  started_at   INTEGER NOT NULL DEFAULT 0,
  ended_at     INTEGER NOT NULL DEFAULT 0,
  n_msg        INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS msgs(
  id          INTEGER PRIMARY KEY,
  file_id     INTEGER NOT NULL,
  source      TEXT    NOT NULL,
  session_id  TEXT    NOT NULL,
  agent_id    TEXT    NOT NULL DEFAULT '',
  slug        TEXT    NOT NULL DEFAULT '',
  uuid        TEXT    NOT NULL DEFAULT '',
  parent_uuid TEXT    NOT NULL DEFAULT '',
  ts          INTEGER NOT NULL,
  seq         INTEGER NOT NULL,
  role        TEXT    NOT NULL,
  kind        TEXT    NOT NULL,
  tool        TEXT    NOT NULL DEFAULT '',
  cwd         TEXT    NOT NULL DEFAULT '',
  branch      TEXT    NOT NULL DEFAULT '',
  n_chars     INTEGER NOT NULL,
  text        TEXT    NOT NULL
);
CREATE INDEX IF NOT EXISTS msgs_file    ON msgs(file_id, seq);
CREATE INDEX IF NOT EXISTS msgs_session ON msgs(session_id, ts);
CREATE INDEX IF NOT EXISTS msgs_ts      ON msgs(ts);
CREATE INDEX IF NOT EXISTS sessions_ended ON sessions(ended_at);

CREATE VIRTUAL TABLE IF NOT EXISTS msgs_fts USING fts5(
  text, content='msgs', content_rowid='id', tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS msgs_ai AFTER INSERT ON msgs BEGIN
  INSERT INTO msgs_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS msgs_ad AFTER DELETE ON msgs BEGIN
  INSERT INTO msgs_fts(msgs_fts, rowid, text) VALUES ('delete', old.id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS msgs_au AFTER UPDATE ON msgs BEGIN
  INSERT INTO msgs_fts(msgs_fts, rowid, text) VALUES ('delete', old.id, old.text);
  INSERT INTO msgs_fts(rowid, text) VALUES (new.id, new.text);
END;
`

var dropAll = []string{
	"DROP TRIGGER IF EXISTS msgs_ai", "DROP TRIGGER IF EXISTS msgs_ad", "DROP TRIGGER IF EXISTS msgs_au",
	"DROP TABLE IF EXISTS msgs_fts", "DROP TABLE IF EXISTS msgs",
	"DROP TABLE IF EXISTS sessions", "DROP TABLE IF EXISTS files", "DROP TABLE IF EXISTS meta",
}

func (db *DB) migrate() error {
	var v string
	err := db.QueryRow(`SELECT value FROM meta WHERE key='schema_version'`).Scan(&v)
	switch {
	case err == nil && v == strconv.Itoa(SchemaVersion):
		return nil // up to date; opening does not write
	case err == nil:
		if err := db.dropAll(); err != nil {
			return err
		}
	}
	if _, err := db.Exec(schema); err != nil {
		return err
	}
	// Rows reach msgs in large INSERT … SELECT batches; with the default
	// 1 MiB term buffer FTS5 would still flush a small segment every few
	// hundred rows. 32 MiB cut a full build from 58 s to ~50 s.
	if _, err := db.Exec(`INSERT INTO msgs_fts(msgs_fts, rank) VALUES('hashsize', 33554432)`); err != nil {
		return err
	}
	return db.SetMeta("schema_version", strconv.Itoa(SchemaVersion))
}

func (db *DB) dropAll() error {
	for _, s := range dropAll {
		if _, err := db.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// Reset drops every table and recreates an empty index. The full-text
// mode preference survives.
func (db *DB) Reset() error {
	full := db.FullMode()
	if err := db.dropAll(); err != nil {
		return err
	}
	if err := db.migrate(); err != nil {
		return err
	}
	_, err := db.Exec(`VACUUM`)
	if err != nil {
		return err
	}
	return db.SetFullMode(full)
}

// Meta reads a key from the meta table ("" when absent).
func (db *DB) Meta(key string) string {
	var v string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v); err != nil {
		return ""
	}
	return v
}

// SetMeta writes a key to the meta table.
func (db *DB) SetMeta(key, value string) error {
	_, err := db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// FullMode reports whether tool text is stored with the larger limit.
func (db *DB) FullMode() bool { return db.Meta("full") == "1" }

// SetFullMode persists the truncation mode used by future syncs.
func (db *DB) SetFullMode(full bool) error {
	v := "0"
	if full {
		v = "1"
	}
	return db.SetMeta("full", v)
}

// CheckFTS runs the FTS5 integrity check, which verifies that the full-text
// index matches the content table row for row.
func (db *DB) CheckFTS() error {
	_, err := db.Exec(`INSERT INTO msgs_fts(msgs_fts, rank) VALUES('integrity-check', 1)`)
	if err != nil {
		return errors.New("full-text index is inconsistent with msgs: " + err.Error())
	}
	return nil
}
