package index

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/haojixing/agentory/internal/model"
	"github.com/haojixing/agentory/internal/source/claudecode"
)

type env struct {
	t    *testing.T
	root string
	db   *DB
	src  []model.Source
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	if err := os.MkdirAll(filepath.Join(root, "-home-alice-demo"), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &env{t: t, root: root, db: db, src: []model.Source{claudecode.NewWithRoot(root)}}
}

func (e *env) path(name string) string {
	return filepath.Join(e.root, "-home-alice-demo", name)
}

// userLine builds a synthetic user prompt record.
func userLine(sid string, n int, text string) string {
	ts := time.Date(2026, 9, 1, 10, 0, n, 0, time.UTC).Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"user","uuid":"u%d","sessionId":%q,"timestamp":%q,"cwd":"/home/alice/demo","gitBranch":"main","message":{"role":"user","content":%q}}`+"\n",
		n, sid, ts, text)
}

func toolResultLine(sid string, n int, text string) string {
	ts := time.Date(2026, 9, 1, 10, 0, n, 0, time.UTC).Format(time.RFC3339)
	return fmt.Sprintf(`{"type":"user","uuid":"u%d","sessionId":%q,"timestamp":%q,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":%q}]}}`+"\n",
		n, sid, ts, text)
}

func (e *env) write(name, content string) {
	e.t.Helper()
	if err := os.WriteFile(e.path(name), []byte(content), 0o644); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) appendTo(name, content string) {
	e.t.Helper()
	f, err := os.OpenFile(e.path(name), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		e.t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		e.t.Fatal(err)
	}
	f.Close()
	bumpMtime(e.t, e.path(name))
}

// bumpMtime makes sure consecutive writes are distinguishable even on
// filesystems with coarse timestamps.
func bumpMtime(t *testing.T, p string) {
	t.Helper()
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	next := info.ModTime().Add(2 * time.Second)
	if err := os.Chtimes(p, next, next); err != nil {
		t.Fatal(err)
	}
}

func (e *env) sync() Stats {
	e.t.Helper()
	st, err := e.db.Sync(context.Background(), e.src, Options{})
	if err != nil {
		e.t.Fatalf("sync: %v", err)
	}
	return st
}

func (e *env) texts() []string {
	e.t.Helper()
	rows, err := e.db.Query(`SELECT text FROM msgs ORDER BY file_id, seq`)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, s)
	}
	return out
}

func (e *env) fileRow(name string) fileRow {
	e.t.Helper()
	var r fileRow
	if err := e.db.QueryRow(`SELECT id, size, byte_off, n_msg FROM files WHERE path=?`, e.path(name)).
		Scan(&r.id, &r.size, &r.byteOff, &r.nMsg); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (e *env) ftsHits(term string) int {
	e.t.Helper()
	var n int
	if err := e.db.QueryRow(`SELECT count(*) FROM msgs_fts WHERE msgs_fts MATCH ?`, `"`+term+`"`).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func eq(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("messages = %q, want %q", got, want)
	}
}

func TestSyncAppendReadsOnlyNewBytes(t *testing.T) {
	e := newEnv(t)
	first := userLine("s1", 1, "alpha one") + userLine("s1", 2, "alpha two")
	e.write("s1.jsonl", first)

	st := e.sync()
	if st.New != 1 || st.Messages != 2 {
		t.Fatalf("first sync: %+v", st)
	}
	r := e.fileRow("s1.jsonl")
	if r.byteOff != int64(len(first)) || r.nMsg != 2 {
		t.Fatalf("byte_off=%d n_msg=%d, want %d/2", r.byteOff, r.nMsg, len(first))
	}

	e.appendTo("s1.jsonl", userLine("s1", 3, "alpha three"))
	st = e.sync()
	if st.Appended != 1 || st.Rebuilt != 0 || st.Messages != 1 {
		t.Fatalf("append sync should parse exactly the new line: %+v", st)
	}
	r2 := e.fileRow("s1.jsonl")
	if r2.byteOff <= r.byteOff || r2.nMsg != 3 || r2.id != r.id {
		t.Fatalf("byte_off did not advance: %+v -> %+v", r, r2)
	}
	eq(t, e.texts(), []string{"alpha one", "alpha two", "alpha three"})

	// Nothing changed: nothing is re-read.
	if st := e.sync(); st.Unchanged != 1 || st.Changed() {
		t.Fatalf("idle sync touched the index: %+v", st)
	}
}

func TestSyncPartialTrailingLineIsDeferred(t *testing.T) {
	e := newEnv(t)
	done := userLine("s1", 1, "complete line")
	partial := userLine("s1", 2, "being written")
	e.write("s1.jsonl", done+partial[:20])

	st := e.sync()
	if st.BadLines != 0 {
		t.Fatalf("a half-written line must not be parsed: %+v", st)
	}
	if r := e.fileRow("s1.jsonl"); r.byteOff != int64(len(done)) {
		t.Fatalf("byte_off=%d, want %d (end of last complete line)", r.byteOff, len(done))
	}

	e.appendTo("s1.jsonl", partial[20:])
	st = e.sync()
	if st.Appended != 1 || st.BadLines != 0 {
		t.Fatalf("completed line should be appended cleanly: %+v", st)
	}
	eq(t, e.texts(), []string{"complete line", "being written"})
}

func TestSyncRewriteInPlaceTriggersRebuild(t *testing.T) {
	e := newEnv(t)
	e.write("s1.jsonl", userLine("s1", 1, "old content zebra")+userLine("s1", 2, "old second"))
	e.sync()

	// Rewritten with different, longer content: the byte before the old
	// offset is no longer a newline.
	e.write("s1.jsonl", userLine("s1", 1, "brand new first line that is much longer than before")+
		userLine("s1", 2, "new second")+userLine("s1", 3, "new third"))
	bumpMtime(t, e.path("s1.jsonl"))

	st := e.sync()
	if st.Rebuilt != 1 || st.Appended != 0 || st.BadLines != 0 {
		t.Fatalf("rewrite must rebuild, not resume mid-line: %+v", st)
	}
	eq(t, e.texts(), []string{"brand new first line that is much longer than before", "new second", "new third"})
	if e.ftsHits("zebra") != 0 {
		t.Fatal("stale full-text entries survived the rebuild")
	}
}

func TestSyncRewriteBeyondHeadTriggersRebuild(t *testing.T) {
	e := newEnv(t)
	// The first line is longer than the fingerprinted head, so only the
	// newline check can notice that a later line was rewritten.
	first := userLine("s1", 1, strings.Repeat("h", 2*headLen))
	e.write("s1.jsonl", first+userLine("s1", 2, "short"))
	e.sync()

	e.write("s1.jsonl", first+userLine("s1", 2, "short line rewritten to be longer")+userLine("s1", 3, "tail"))
	bumpMtime(t, e.path("s1.jsonl"))
	st := e.sync()
	if st.Rebuilt != 1 || st.BadLines != 0 {
		t.Fatalf("mid-line resume must be detected: %+v", st)
	}
	got := e.texts()
	if len(got) != 3 || got[1] != "short line rewritten to be longer" || got[2] != "tail" {
		t.Fatalf("messages after rebuild: %q", got[1:])
	}
}

func TestSyncRewriteAlignedOnNewlineTriggersRebuild(t *testing.T) {
	e := newEnv(t)
	a := userLine("s1", 1, "aaaa")
	e.write("s1.jsonl", a)
	e.sync()

	// Same length first line, so the byte at the old offset-1 is still '\n',
	// but the head of the file changed.
	b := userLine("s1", 1, "bbbb")
	if len(a) != len(b) {
		t.Fatal("test setup: lines must have equal length")
	}
	e.write("s1.jsonl", b+userLine("s1", 2, "cccc"))
	bumpMtime(t, e.path("s1.jsonl"))

	st := e.sync()
	if st.Rebuilt != 1 {
		t.Fatalf("head change must force a rebuild: %+v", st)
	}
	eq(t, e.texts(), []string{"bbbb", "cccc"})
}

func TestSyncShrinkTriggersRebuild(t *testing.T) {
	e := newEnv(t)
	e.write("s1.jsonl", userLine("s1", 1, "one")+userLine("s1", 2, "two")+userLine("s1", 3, "three"))
	e.sync()

	e.write("s1.jsonl", userLine("s1", 1, "one"))
	bumpMtime(t, e.path("s1.jsonl"))
	st := e.sync()
	if st.Rebuilt != 1 {
		t.Fatalf("shrink must rebuild: %+v", st)
	}
	eq(t, e.texts(), []string{"one"})
	var n int
	e.db.QueryRow(`SELECT n_msg FROM sessions WHERE id='s1'`).Scan(&n)
	if n != 1 {
		t.Fatalf("session n_msg=%d after shrink, want 1", n)
	}
}

func TestSyncHugeLine(t *testing.T) {
	e := newEnv(t)
	big := "needle " + strings.Repeat("y", 3<<20)
	e.write("s1.jsonl", userLine("s1", 1, big)+userLine("s1", 2, "after big"))
	st := e.sync()
	if st.Messages != 2 || st.BadLines != 0 {
		t.Fatalf("huge line not indexed: %+v", st)
	}
	if e.ftsHits("after big") != 1 {
		t.Fatal("line after the huge one was lost")
	}
}

func TestSyncTruncatesToolTextAndFullModeRebuilds(t *testing.T) {
	e := newEnv(t)
	long := strings.Repeat("r", 5000)
	e.write("s1.jsonl", toolResultLine("s1", 1, long)+userLine("s1", 2, strings.Repeat("p", 5000)))
	e.sync()

	var tr, pr, nChars int
	e.db.QueryRow(`SELECT length(text), n_chars FROM msgs WHERE kind='tool_result'`).Scan(&tr, &nChars)
	e.db.QueryRow(`SELECT length(text) FROM msgs WHERE kind='prompt'`).Scan(&pr)
	if tr != TruncateDefault+1 || nChars != 5000 {
		t.Fatalf("tool_result length=%d n_chars=%d, want %d/5000", tr, nChars, TruncateDefault+1)
	}
	if pr != 5000 {
		t.Fatalf("prompts must never be truncated, got %d", pr)
	}

	if err := e.db.SetFullMode(true); err != nil {
		t.Fatal(err)
	}
	st := e.sync()
	if st.Rebuilt != 1 {
		t.Fatalf("mode change must rebuild the file: %+v", st)
	}
	e.db.QueryRow(`SELECT length(text) FROM msgs WHERE kind='tool_result'`).Scan(&tr)
	if tr != 5000 {
		t.Fatalf("full mode tool_result length=%d, want 5000", tr)
	}
}

func TestSyncFTSStaysAlignedAfterRebuild(t *testing.T) {
	e := newEnv(t)
	e.write("a.jsonl", userLine("sa", 1, "zebraA first")+userLine("sa", 2, "zebraA second"))
	e.write("b.jsonl", userLine("sb", 1, "giraffe only"))
	e.sync()

	// Rebuild a.jsonl with different content; deleted rowids get reused.
	e.write("a.jsonl", userLine("sa", 1, "zebraB"))
	bumpMtime(t, e.path("a.jsonl"))
	if st := e.sync(); st.Rebuilt != 1 {
		t.Fatalf("expected rebuild: %+v", st)
	}
	e.write("c.jsonl", userLine("sc", 1, "okapi new file"))
	e.sync()

	if n := e.ftsHits("zebraA"); n != 0 {
		t.Errorf("deleted text still matches: %d hits", n)
	}
	cases := map[string]string{"zebraB": "zebraB", "giraffe": "giraffe only", "okapi": "okapi new file"}
	for term, want := range cases {
		var got string
		err := e.db.QueryRow(`SELECT m.text FROM msgs_fts f JOIN msgs m ON m.id=f.rowid WHERE msgs_fts MATCH ?`, `"`+term+`"`).Scan(&got)
		if err != nil || got != want {
			t.Errorf("MATCH %s -> %q (%v), want %q", term, got, err, want)
		}
	}
	if err := e.db.CheckFTS(); err != nil {
		t.Fatal(err)
	}
}

func TestSyncSessionsAndTitles(t *testing.T) {
	e := newEnv(t)
	e.write("s1.jsonl",
		`{"type":"custom-title","sessionId":"s1","customTitle":"User title"}`+"\n"+
			userLine("s1", 1, "first question")+
			`{"type":"ai-title","sessionId":"s1","aiTitle":"Generated title"}`+"\n"+
			userLine("s1", 5, "second question"))
	// Sub-agent transcript belongs to the parent session.
	sub := filepath.Join(e.root, "-home-alice-demo", "s1", "subagents")
	os.MkdirAll(sub, 0o755)
	os.WriteFile(filepath.Join(sub, "agent-x.jsonl"), []byte(
		`{"type":"assistant","uuid":"x1","sessionId":"s1","agentId":"x","isSidechain":true,"timestamp":"2026-09-01T10:00:09Z","message":{"role":"assistant","content":[{"type":"text","text":"sub finding"}]}}`+"\n"), 0o644)
	e.sync()

	var title, first, project, cwd string
	var n int
	var started, ended int64
	err := e.db.QueryRow(`SELECT title, first_prompt, project, cwd, n_msg, started_at, ended_at FROM sessions WHERE id='s1'`).
		Scan(&title, &first, &project, &cwd, &n, &started, &ended)
	if err != nil {
		t.Fatal(err)
	}
	if title != "User title" {
		t.Errorf("custom title must win over a later ai-title, got %q", title)
	}
	if first != "first question" || project != "-home-alice-demo" || cwd != "/home/alice/demo" || n != 2 {
		t.Errorf("session aggregates wrong: first=%q project=%q cwd=%q n=%d", first, project, cwd, n)
	}
	wantEnd := time.Date(2026, 9, 1, 10, 0, 9, 0, time.UTC).UnixMilli()
	if ended != wantEnd || started >= ended {
		t.Errorf("time span wrong: %d..%d", started, ended)
	}
	var agent string
	e.db.QueryRow(`SELECT agent_id FROM msgs WHERE text='sub finding'`).Scan(&agent)
	if agent != "x" {
		t.Errorf("sub-agent message agent_id=%q", agent)
	}
}

func TestDecide(t *testing.T) {
	prev := &fileRow{size: 100, mtime: 50, full: false}
	cases := []struct {
		name        string
		prev        *fileRow
		size, mtime int64
		full        bool
		want        action
	}{
		{"new", nil, 10, 1, false, actNew},
		{"unchanged", prev, 100, 50, false, actSkip},
		{"grown", prev, 200, 60, false, actAppend},
		{"shrunk", prev, 90, 60, false, actRebuild},
		{"older mtime", prev, 200, 40, false, actRebuild},
		{"same size touched", prev, 100, 60, false, actRebuild},
		{"mode changed", prev, 100, 50, true, actRebuild},
	}
	for _, c := range cases {
		if got, _ := decide(c.prev, c.size, c.mtime, c.full); got != c.want {
			t.Errorf("%s: got %v want %v", c.name, got, c.want)
		}
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("库存锁定先锁后扣", 4); got != "库存锁定…" {
		t.Errorf("rune-aware truncate: %q", got)
	}
	if got := Truncate("short", 10); got != "short" {
		t.Errorf("no-op truncate: %q", got)
	}
}

func TestPrune(t *testing.T) {
	e := newEnv(t)
	e.write("a.jsonl", userLine("sa", 1, "keep"))
	e.write("b.jsonl", userLine("sb", 1, "gone"))
	e.sync()
	os.Remove(e.path("b.jsonl"))

	e.sync() // default: history is retained
	got := e.texts()
	sort.Strings(got) // files are parsed in parallel, so ids are unordered
	eq(t, got, []string{"gone", "keep"})

	st, err := e.db.Sync(context.Background(), e.src, Options{Prune: true})
	if err != nil || st.Pruned != 1 {
		t.Fatalf("prune: %+v %v", st, err)
	}
	eq(t, e.texts(), []string{"keep"})
	var n int
	e.db.QueryRow(`SELECT count(*) FROM sessions WHERE id='sb'`).Scan(&n)
	if n != 0 {
		t.Error("session of pruned file survived")
	}
}

func TestOpenPathWithURIDelimiters(t *testing.T) {
	name := "odd dir #1 %"
	if runtime.GOOS != "windows" {
		name += "?" // not a valid file name character on Windows
	}
	p := filepath.Join(t.TempDir(), name, "index.db")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database not created at the literal path: %v", err)
	}
}
