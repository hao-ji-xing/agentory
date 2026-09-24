package index

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hao-ji-xing/agentory/internal/model"
)

// headLen is how many leading bytes are fingerprinted to detect a file that
// was rewritten in place (rather than appended to).
const headLen = 4096

// Options controls a Sync run.
type Options struct {
	// Prune removes indexed files that no longer exist on disk. It is off by
	// default so history survives the agent's own transcript cleanup.
	Prune bool
	// Workers is the number of parallel parsers (default: NumCPU).
	Workers int
	// Progress, when set, is called after each file is processed.
	Progress func(done, total int)
	// OnRebuild, when set, is called for every file indexed from scratch
	// although it was indexed before, with the reason.
	OnRebuild func(path, reason string)
}

// Stats summarizes a Sync run.
type Stats struct {
	Files     int // matching files on disk
	Unchanged int
	New       int // first-time indexed
	Appended  int // resumed from byte_off
	Rebuilt   int // re-indexed from scratch
	Pruned    int
	Messages  int // messages written
	BadLines  int // lines that failed to parse
	Duration  time.Duration
}

// Changed reports whether the run touched the index.
func (s Stats) Changed() bool { return s.New+s.Appended+s.Rebuilt+s.Pruned > 0 }

type fileRow struct {
	id      int64
	size    int64
	mtime   int64
	byteOff int64
	nMsg    int64
	full    bool
	headCRC uint32
}

type action int

const (
	actSkip action = iota
	actNew
	actAppend
	actRebuild
)

type job struct {
	src    model.Source
	path   string
	size   int64
	mtime  int64
	prev   *fileRow
	action action
	reason string // why a known file is rebuilt
}

type result struct {
	job     *job
	msgs    []model.Message
	metas   []model.SessionMeta
	usages  []model.Usage
	turns   []model.Turn
	endOff  int64
	headCRC uint32
	bad     int
	err     error
}

// Sync brings the index up to date with every file of the given sources.
func (db *DB) Sync(ctx context.Context, sources []model.Source, opt Options) (Stats, error) {
	start := time.Now()
	var st Stats
	full := db.FullMode()

	known, err := db.loadFiles()
	if err != nil {
		return st, err
	}

	var jobs []*job
	seen := map[string]bool{}
	for _, src := range sources {
		files, err := discover(src)
		if err != nil {
			return st, err
		}
		for _, f := range files {
			seen[f.path] = true
			st.Files++
			f.src = src
			f.action, f.reason = decide(known[f.path], f.size, f.mtime, full)
			f.prev = known[f.path]
			if f.action == actSkip {
				st.Unchanged++
				continue
			}
			jobs = append(jobs, f)
		}
	}
	// Big files first keeps all workers busy until the end.
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].size > jobs[j].size })

	workers := opt.Workers
	if workers <= 0 {
		workers = runtime.NumCPU()
	}
	jobCh := make(chan *job)
	resCh := make(chan *result, workers)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				r := parseFile(j)
				select {
				case resCh <- r:
				case <-ctx.Done():
					return
				}
			}
		}()
	}
	go func() {
		defer close(jobCh)
		for _, j := range jobs {
			select {
			case jobCh <- j:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(resCh) }()

	// Results are written in batched transactions: FTS5 flushes a new index
	// segment on every commit, so one commit per file would make the initial
	// build spend most of its time merging tiny segments.
	var (
		firstErr error
		tx       *sql.Tx
		pending  Stats
		batch    int
		done     int
		affected = map[string]bool{}
	)
	commit := func() error {
		if tx == nil {
			return nil
		}
		err := flushStage(tx, affected)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
		clear(affected)
		tx = nil
		if err == nil {
			st.New += pending.New
			st.Appended += pending.Appended
			st.Rebuilt += pending.Rebuilt
			st.Messages += pending.Messages
			st.BadLines += pending.BadLines
		}
		pending, batch = Stats{}, 0
		return err
	}
	for r := range resCh {
		done++
		if firstErr == nil {
			firstErr = ctx.Err()
			if firstErr == nil && r.err != nil {
				firstErr = fmt.Errorf("%s: %w", r.job.path, r.err)
			}
			if firstErr == nil && tx == nil {
				tx, firstErr = db.Begin()
			}
			if firstErr == nil {
				if n, err := db.writeFile(tx, r, full, affected); err != nil {
					firstErr = fmt.Errorf("%s: %w", r.job.path, err)
				} else if n >= 0 {
					switch r.job.action {
					case actNew:
						pending.New++
					case actAppend:
						pending.Appended++
					case actRebuild:
						pending.Rebuilt++
						if opt.OnRebuild != nil {
							opt.OnRebuild(r.job.path, r.job.reason)
						}
					}
					pending.Messages += len(r.msgs)
					pending.BadLines += r.bad
					batch += n
				}
			}
			if firstErr == nil && batch >= batchBytes {
				firstErr = commit()
			}
			if firstErr != nil {
				if tx != nil {
					tx.Rollback()
					tx = nil
				}
				clear(affected)
				cancel()
			}
		}
		if opt.Progress != nil {
			opt.Progress(done, len(jobs))
		}
	}
	if firstErr == nil {
		firstErr = commit()
	}
	if firstErr != nil {
		return st, firstErr
	}

	if opt.Prune {
		for path, row := range known {
			if seen[path] {
				continue
			}
			if err := db.removeFile(row.id); err != nil {
				return st, err
			}
			st.Pruned++
		}
	}
	if st.Messages >= 5000 {
		// Fold a large WAL back into the main file so it does not linger.
		if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
			return st, err
		}
	}
	if st.Changed() {
		if err := db.SetMeta("last_sync", time.Now().UTC().Format(time.RFC3339)); err != nil {
			return st, err
		}
	}
	st.Duration = time.Since(start)
	return st, nil
}

// decide picks what to do with a file given its stored state.
func decide(prev *fileRow, size, mtime int64, full bool) (action, string) {
	switch {
	case prev == nil:
		return actNew, ""
	case prev.full != full:
		return actRebuild, "truncation mode changed"
	case size < prev.size:
		return actRebuild, "file shrank"
	case mtime < prev.mtime:
		return actRebuild, "mtime went backwards"
	case size == prev.size && mtime == prev.mtime:
		return actSkip, ""
	case size == prev.size:
		return actRebuild, "modified without growing"
	default:
		return actAppend, "" // parseFile verifies the resume point
	}
}

func discover(src model.Source) ([]*job, error) {
	var out []*job
	for _, root := range src.Roots() {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || errors.Is(err, fs.ErrPermission) {
					if d != nil && d.IsDir() {
						return fs.SkipDir
					}
					return nil
				}
				return err
			}
			if d.IsDir() || !d.Type().IsRegular() || !src.Match(path) {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil // vanished between readdir and stat
			}
			out = append(out, &job{path: path, size: info.Size(), mtime: info.ModTime().UnixNano()})
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (db *DB) loadFiles() (map[string]*fileRow, error) {
	rows, err := db.Query(`SELECT id, path, size, mtime, byte_off, n_msg, full, head_crc FROM files`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]*fileRow{}
	for rows.Next() {
		var r fileRow
		var path string
		var crc int64
		if err := rows.Scan(&r.id, &path, &r.size, &r.mtime, &r.byteOff, &r.nMsg, &r.full, &crc); err != nil {
			return nil, err
		}
		r.headCRC = uint32(crc)
		out[path] = &r
	}
	return out, rows.Err()
}

// parseFile reads the file from the resume point (or from the start) and
// parses every complete line. An unterminated last line is left for the
// next run: the agent may still be writing it.
func parseFile(j *job) *result {
	r := &result{job: j}
	f, err := os.Open(j.path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			j.action = actSkip
			return r
		}
		r.err = err
		return r
	}
	defer f.Close()

	var off int64
	if j.action == actAppend {
		if ok, err := canResume(f, j.prev); err != nil {
			r.err = err
			return r
		} else if ok {
			off = j.prev.byteOff
		} else {
			j.action, j.reason = actRebuild, "rewritten in place (resume point invalid)"
		}
	}
	if _, err := f.Seek(off, io.SeekStart); err != nil {
		r.err = err
		return r
	}

	br := bufio.NewReaderSize(f, 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if err == io.EOF {
			break // partial trailing line (or nothing): not consumed
		}
		if err != nil {
			r.err = err
			return r
		}
		off += int64(len(line))
		p, perr := j.src.ParseLine(line)
		if perr != nil {
			r.bad++
			continue
		}
		r.msgs = append(r.msgs, p.Messages...)
		if p.Meta != nil {
			r.metas = append(r.metas, *p.Meta)
		}
		if p.Usage != nil {
			r.usages = append(r.usages, *p.Usage)
		}
		if p.Turn != nil {
			r.turns = append(r.turns, *p.Turn)
		}
	}
	r.endOff = off
	if r.headCRC, err = headCRC(f, off); err != nil {
		r.err = err
	}
	return r
}

// canResume reports whether appending from prev.byteOff is safe: the byte
// before the offset must be a newline and the file head must be unchanged.
// Otherwise the file was rewritten and resuming would start mid-line.
func canResume(f *os.File, prev *fileRow) (bool, error) {
	if prev.byteOff == 0 {
		return true, nil
	}
	b := make([]byte, 1)
	if _, err := f.ReadAt(b, prev.byteOff-1); err != nil {
		if errors.Is(err, io.EOF) {
			return false, nil
		}
		return false, err
	}
	if b[0] != '\n' {
		return false, nil
	}
	crc, err := headCRC(f, prev.byteOff)
	return crc == prev.headCRC, err
}

// headCRC fingerprints the first min(end, headLen) bytes of f.
func headCRC(f *os.File, end int64) (uint32, error) {
	buf := make([]byte, min(end, headLen))
	if _, err := f.ReadAt(buf, 0); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	return crc32.ChecksumIEEE(buf), nil
}

// Truncate shortens s to at most limit characters.
func Truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s // byte length bounds rune count
	}
	n := 0
	for i := range s {
		if n == limit {
			return s[:i] + "…"
		}
		n++
	}
	return s
}

func truncates(k model.Kind) bool {
	return k == model.KindToolResult || k == model.KindToolUse
}

const stageCols = `file_id, source, session_id, agent_id, slug, uuid, parent_uuid,
	ts, seq, role, kind, tool, cwd, branch, n_chars, text,
	model, request_id, tool_use_id, is_error, prompt_source, file_path, inv_kind, inv_name, inv_args`

const stageSchema = `CREATE TEMP TABLE IF NOT EXISTS stage(` + stageCols + `)`

// batchBytes is roughly how much text is written per transaction.
const batchBytes = 48 << 20

// writeFile stages one parsed file inside tx and returns the number of text
// bytes written, or -1 when the file was skipped. Sessions whose messages
// changed are added to affected; flushStage must run before commit.
func (db *DB) writeFile(tx *sql.Tx, r *result, full bool, affected map[string]bool) (int, error) {
	j := r.job
	if j.action == actSkip {
		return -1, nil
	}
	limit := TruncateDefault
	if full {
		limit = TruncateFull
	}
	// Another process may have synced this file since we planned the job.
	var cur fileRow
	err := tx.QueryRow(`SELECT id, size, mtime, byte_off, n_msg FROM files WHERE path=?`, j.path).
		Scan(&cur.id, &cur.size, &cur.mtime, &cur.byteOff, &cur.nMsg)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if j.prev != nil {
			return -1, nil
		}
	case err != nil:
		return 0, err
	default:
		if j.prev == nil || cur.byteOff != j.prev.byteOff || cur.mtime != j.prev.mtime {
			return -1, nil
		}
	}

	project := j.src.ProjectOf(j.path)
	fileID := cur.id
	seq := int64(0)
	written := 0
	if j.action == actAppend {
		seq = cur.nMsg
	} else if fileID != 0 {
		if err := sessionsOfFile(tx, fileID, affected); err != nil {
			return 0, err
		}
		if err := deleteFileRows(tx, fileID); err != nil {
			return 0, err
		}
	}
	if fileID == 0 {
		res, err := tx.Exec(`INSERT INTO files(source, path, size, mtime, byte_off, n_msg, full, head_crc)
			VALUES(?,?,0,0,0,0,?,0)`, j.src.Name(), j.path, full)
		if err != nil {
			return 0, err
		}
		if fileID, err = res.LastInsertId(); err != nil {
			return 0, err
		}
	}

	// Rows go to a trigger-less staging table first and reach msgs in one
	// INSERT … SELECT. A statement that fires the FTS triggers opens a
	// savepoint, and FTS5 flushes its pending terms to a new segment on
	// every savepoint: row-by-row inserts would create one segment per row.
	if _, err := tx.Exec(stageSchema); err != nil {
		return 0, err
	}
	ins, err := tx.Prepare(`INSERT INTO temp.stage(` + stageCols + `) VALUES(` + placeholders(25) + `)`)
	if err != nil {
		return 0, err
	}
	defer ins.Close()
	for i := range r.msgs {
		m := &r.msgs[i]
		if m.SessionID == "" {
			continue
		}
		nChars := utf8.RuneCountInString(m.Text)
		text := m.Text
		if truncates(m.Kind) {
			text = Truncate(text, limit)
		}
		var ts int64
		if !m.Time.IsZero() {
			ts = m.Time.UnixMilli()
		}
		if _, err := ins.Exec(fileID, j.src.Name(), m.SessionID, m.AgentID, m.Slug, m.UUID, m.ParentUUID,
			ts, seq, m.Role, string(m.Kind), m.Tool, m.CWD, m.Branch, nChars, text,
			m.Model, m.RequestID, m.ToolUseID, m.IsError, m.PromptSource, m.FilePath, m.InvKind, m.InvName, m.InvArgs); err != nil {
			return 0, err
		}
		seq++
		written += len(text)
		if !affected[m.SessionID] {
			affected[m.SessionID] = true
			if err := ensureSession(tx, m.SessionID, j.src.Name(), project); err != nil {
				return 0, err
			}
		}
	}

	for _, m := range r.metas {
		if err := ensureSession(tx, m.SessionID, j.src.Name(), project); err != nil {
			return 0, err
		}
		if m.Title != "" {
			if _, err := tx.Exec(`UPDATE sessions SET title=?, title_rank=? WHERE id=? AND title_rank<=?`,
				m.Title, m.TitleRank, m.SessionID, m.TitleRank); err != nil {
				return 0, err
			}
		}
		if c := m.Cost; c != nil {
			// Cumulative snapshots: the latest one in transcript order wins.
			if _, err := tx.Exec(`UPDATE sessions SET cost_usd=?, lines_added=?, lines_removed=?, duration_ms=? WHERE id=?`,
				c.USD, c.LinesAdded, c.LinesRemoved, c.DurationMs, m.SessionID); err != nil {
				return 0, err
			}
		}
	}
	if err := writeRequests(tx, fileID, j.src.Name(), r.usages); err != nil {
		return 0, err
	}
	if err := writeTurns(tx, fileID, j.src.Name(), r.turns); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE files SET size=?, mtime=?, byte_off=?, n_msg=?, full=?, head_crc=? WHERE id=?`,
		j.size, j.mtime, r.endOff, seq, full, int64(r.headCRC), fileID); err != nil {
		return 0, err
	}
	return written, nil
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// deleteFileRows removes everything derived from one transcript; the FTS
// delete trigger keeps msgs_fts in step.
func deleteFileRows(tx *sql.Tx, fileID int64) error {
	for _, table := range []string{"msgs", "requests", "turns"} {
		if _, err := tx.Exec(`DELETE FROM `+table+` WHERE file_id=?`, fileID); err != nil {
			return err
		}
	}
	return nil
}

// writeRequests upserts token usage per API request. A request can be
// spread over several lines, possibly across two syncs; the line with the
// most output tokens is the final one and wins.
func writeRequests(tx *sql.Tx, fileID int64, source string, us []model.Usage) error {
	if len(us) == 0 {
		return nil
	}
	st, err := tx.Prepare(`INSERT INTO requests(request_id, file_id, source, session_id, agent_id, ts, cwd, branch, model,
		tok_in, tok_out, cache_read, cache_w5m, cache_w1h, tok_think) VALUES(` + placeholders(15) + `)
		ON CONFLICT(request_id) DO UPDATE SET ts=excluded.ts, model=excluded.model,
		tok_in=excluded.tok_in, tok_out=excluded.tok_out, cache_read=excluded.cache_read,
		cache_w5m=excluded.cache_w5m, cache_w1h=excluded.cache_w1h, tok_think=excluded.tok_think
		WHERE excluded.tok_out > requests.tok_out`)
	if err != nil {
		return err
	}
	defer st.Close()
	for _, u := range us {
		if u.SessionID == "" {
			continue
		}
		if _, err := st.Exec(u.RequestID, fileID, source, u.SessionID, u.AgentID, millis(u.Time), u.CWD, u.Branch, u.Model,
			u.Input, u.Output, u.CacheRead, u.CacheWrite5m, u.CacheWrite1h, u.Thinking); err != nil {
			return err
		}
	}
	return nil
}

func writeTurns(tx *sql.Tx, fileID int64, source string, ts []model.Turn) error {
	for _, t := range ts {
		if t.SessionID == "" {
			continue
		}
		if _, err := tx.Exec(`INSERT INTO turns(file_id, source, session_id, agent_id, ts, cwd, branch, duration_ms, n_msgs)
			VALUES(`+placeholders(9)+`)`, fileID, source, t.SessionID, t.AgentID, millis(t.Time), t.CWD, t.Branch,
			t.DurationMs, t.Messages); err != nil {
			return err
		}
	}
	return nil
}

// flushStage moves staged rows into msgs with a single statement (so the FTS
// triggers flush once per batch) and refreshes the affected sessions.
func flushStage(tx *sql.Tx, affected map[string]bool) error {
	if _, err := tx.Exec(`INSERT INTO msgs(` + stageCols + `) SELECT ` + stageCols + ` FROM temp.stage ORDER BY rowid`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM temp.stage`); err != nil {
		return err
	}
	for sid := range affected {
		if err := refreshSession(tx, sid); err != nil {
			return err
		}
	}
	return nil
}

func sessionsOfFile(tx *sql.Tx, fileID int64, into map[string]bool) error {
	rows, err := tx.Query(`SELECT DISTINCT session_id FROM msgs WHERE file_id=?`, fileID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		into[s] = true
	}
	return rows.Err()
}

func ensureSession(tx *sql.Tx, id, source, project string) error {
	_, err := tx.Exec(`INSERT INTO sessions(id, source, project) VALUES(?,?,?)
		ON CONFLICT(id) DO UPDATE SET project=excluded.project WHERE sessions.project=''`, id, source, project)
	return err
}

// refreshSession recomputes a session's aggregates from its messages.
// Sub-agent messages count towards the time span but not n_msg.
func refreshSession(tx *sql.Tx, id string) error {
	_, err := tx.Exec(`UPDATE sessions SET
		started_at   = COALESCE((SELECT min(ts) FROM msgs WHERE session_id=?1 AND ts>0), 0),
		ended_at     = COALESCE((SELECT max(ts) FROM msgs WHERE session_id=?1), 0),
		n_msg        = (SELECT count(*) FROM msgs WHERE session_id=?1 AND agent_id=''),
		first_prompt = COALESCE((SELECT substr(text, 1, 300) FROM msgs
		                 WHERE session_id=?1 AND agent_id='' AND kind='prompt' ORDER BY ts, id LIMIT 1), ''),
		cwd          = COALESCE((SELECT cwd FROM msgs WHERE session_id=?1 AND agent_id='' AND cwd<>''
		                 ORDER BY ts DESC, id DESC LIMIT 1), cwd),
		branch       = COALESCE((SELECT branch FROM msgs WHERE session_id=?1 AND agent_id='' AND branch<>''
		                 ORDER BY ts DESC, id DESC LIMIT 1), branch)
		WHERE id=?1`, id)
	return err
}

func (db *DB) removeFile(fileID int64) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	affected := map[string]bool{}
	if err := sessionsOfFile(tx, fileID, affected); err != nil {
		return err
	}
	if err := deleteFileRows(tx, fileID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM files WHERE id=?`, fileID); err != nil {
		return err
	}
	for sid := range affected {
		if err := refreshSession(tx, sid); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE n_msg=0 AND NOT EXISTS
		(SELECT 1 FROM msgs WHERE msgs.session_id=sessions.id)`); err != nil {
		return err
	}
	return tx.Commit()
}

// ProjectLabel is the human-facing project name: the last element of the
// working directory, falling back to the stored project key.
func ProjectLabel(cwd, project string) string {
	if cwd != "" {
		cwd = strings.TrimRight(filepath.ToSlash(cwd), "/")
		if i := strings.LastIndex(cwd, "/"); i >= 0 && i < len(cwd)-1 {
			return cwd[i+1:]
		}
		return cwd
	}
	return project
}
