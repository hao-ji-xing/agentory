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

// SchemaVersion is bumped whenever the schema or the parsed output of a
// source changes; an index with a different version is dropped and rebuilt
// from scratch.
//
//	2: tool_use input renders short values first
//	3: model, request, tool linkage, invocation and prompt-source columns;
//	   requests and turns tables; session cost; queued prompts
//	4: contentless full-text index without tool_use / tool_result text
const SchemaVersion = 4

// Tool call arguments and output are ~80% of all text but are not searched
// by default. Keeping them out of the full-text index makes a full build
// several times faster and the index 40% smaller; searches that include
// these kinds scan them with LIKE instead (a few hundred ms).
const unindexedKinds = `('tool_use', 'tool_result')`

// FullTextKind reports whether messages of kind k are in the full-text index.
func FullTextKind(k string) bool { return k != "tool_use" && k != "tool_result" }

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
  n_msg        INTEGER NOT NULL DEFAULT 0,
  cost_usd      REAL    NOT NULL DEFAULT 0,
  lines_added   INTEGER NOT NULL DEFAULT 0,
  lines_removed INTEGER NOT NULL DEFAULT 0,
  duration_ms   INTEGER NOT NULL DEFAULT 0
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
  text        TEXT    NOT NULL,
  model         TEXT    NOT NULL DEFAULT '',
  request_id    TEXT    NOT NULL DEFAULT '',
  tool_use_id   TEXT    NOT NULL DEFAULT '',
  is_error      INTEGER NOT NULL DEFAULT 0,
  prompt_source TEXT    NOT NULL DEFAULT '',
  file_path     TEXT    NOT NULL DEFAULT '',
  inv_kind      TEXT    NOT NULL DEFAULT '',
  inv_name      TEXT    NOT NULL DEFAULT '',
  inv_args      TEXT    NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS requests(
  request_id TEXT PRIMARY KEY,
  file_id    INTEGER NOT NULL,
  source     TEXT    NOT NULL,
  session_id TEXT    NOT NULL,
  agent_id   TEXT    NOT NULL DEFAULT '',
  ts         INTEGER NOT NULL,
  cwd        TEXT    NOT NULL DEFAULT '',
  branch     TEXT    NOT NULL DEFAULT '',
  model      TEXT    NOT NULL DEFAULT '',
  tok_in     INTEGER NOT NULL DEFAULT 0,
  tok_out    INTEGER NOT NULL DEFAULT 0,
  cache_read INTEGER NOT NULL DEFAULT 0,
  cache_w5m  INTEGER NOT NULL DEFAULT 0,
  cache_w1h  INTEGER NOT NULL DEFAULT 0,
  tok_think  INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS turns(
  id          INTEGER PRIMARY KEY,
  file_id     INTEGER NOT NULL,
  source      TEXT    NOT NULL,
  session_id  TEXT    NOT NULL,
  agent_id    TEXT    NOT NULL DEFAULT '',
  ts          INTEGER NOT NULL,
  cwd         TEXT    NOT NULL DEFAULT '',
  branch      TEXT    NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL,
  n_msgs      INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS msgs_file    ON msgs(file_id, seq);
CREATE INDEX IF NOT EXISTS msgs_session ON msgs(session_id, ts);
CREATE INDEX IF NOT EXISTS msgs_ts      ON msgs(ts);
CREATE INDEX IF NOT EXISTS sessions_ended ON sessions(ended_at);
-- Narrow covering index: aggregations over time never touch text pages.
CREATE INDEX IF NOT EXISTS msgs_cover   ON msgs(ts, kind, agent_id, session_id, tool, branch, cwd);
CREATE INDEX IF NOT EXISTS msgs_tooluse ON msgs(tool_use_id) WHERE tool_use_id <> '';
-- Finds the next prompt / interruption after a message without reading text.
CREATE INDEX IF NOT EXISTS msgs_file_kind ON msgs(file_id, kind, seq);
CREATE INDEX IF NOT EXISTS msgs_inv     ON msgs(inv_kind, inv_name, ts) WHERE inv_kind <> '';
CREATE INDEX IF NOT EXISTS requests_file ON requests(file_id);
CREATE INDEX IF NOT EXISTS requests_ts   ON requests(ts);
CREATE INDEX IF NOT EXISTS turns_file    ON turns(file_id);
CREATE INDEX IF NOT EXISTS turns_ts      ON turns(ts);

-- One row per slash command the user typed, skill or sub-agent the model
-- started.
CREATE VIEW IF NOT EXISTS invocations AS
  SELECT id AS msg_id, file_id, seq, source, session_id, agent_id, ts, cwd, branch,
         CASE inv_kind WHEN 'command' THEN 'user' ELSE 'agent' END AS actor,
         inv_kind AS kind, inv_name AS name, inv_args AS args, tool_use_id
  FROM msgs WHERE inv_kind <> '';

CREATE VIRTUAL TABLE IF NOT EXISTS msgs_fts USING fts5(
  text, content='', contentless_delete=1, tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS msgs_ai AFTER INSERT ON msgs
  WHEN new.kind NOT IN ` + unindexedKinds + ` BEGIN
  INSERT INTO msgs_fts(rowid, text) VALUES (new.id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS msgs_ad AFTER DELETE ON msgs
  WHEN old.kind NOT IN ` + unindexedKinds + ` BEGIN
  DELETE FROM msgs_fts WHERE rowid = old.id;
END;
CREATE TRIGGER IF NOT EXISTS msgs_au AFTER UPDATE ON msgs BEGIN
  DELETE FROM msgs_fts WHERE rowid = old.id;
  INSERT INTO msgs_fts(rowid, text) SELECT new.id, new.text WHERE new.kind NOT IN ` + unindexedKinds + `;
END;
`

var dropAll = []string{
	"DROP TRIGGER IF EXISTS msgs_ai", "DROP TRIGGER IF EXISTS msgs_ad", "DROP TRIGGER IF EXISTS msgs_au",
	"DROP VIEW IF EXISTS invocations", "DROP TABLE IF EXISTS requests", "DROP TABLE IF EXISTS turns",
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

// CheckFTS verifies the full-text index: FTS5's own integrity check, then
// that it holds exactly the rows of msgs that should be indexed (catching
// stale or missing entries, e.g. after rowid reuse).
func (db *DB) CheckFTS() error {
	if _, err := db.Exec(`INSERT INTO msgs_fts(msgs_fts) VALUES('integrity-check')`); err != nil {
		return errors.New("full-text index is corrupt: " + err.Error())
	}
	var stale, missing int
	err := db.QueryRow(`SELECT
		(SELECT count(*) FROM msgs_fts_docsize d LEFT JOIN msgs m ON m.id = d.id
		 WHERE m.id IS NULL OR m.kind IN `+unindexedKinds+`),
		(SELECT count(*) FROM msgs m LEFT JOIN msgs_fts_docsize d ON d.id = m.id
		 WHERE d.id IS NULL AND m.kind NOT IN `+unindexedKinds+`)`).Scan(&stale, &missing)
	if err != nil {
		return err
	}
	if stale > 0 || missing > 0 {
		return fmt.Errorf("full-text index is inconsistent with msgs: %d stale, %d missing entries", stale, missing)
	}
	return nil
}
