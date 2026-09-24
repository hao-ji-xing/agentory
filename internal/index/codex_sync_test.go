package index

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/codex"
)

const codexThread = "0192f0c1-0000-7000-8000-00000000abcd"

// codexEnv indexes a synthetic Codex home.
type codexEnv struct {
	t    *testing.T
	home string
	db   *DB
	src  []model.Source
}

func newCodexEnv(t *testing.T) *codexEnv {
	t.Helper()
	dir := t.TempDir()
	home := filepath.Join(dir, "codex")
	for _, d := range []string{"sessions/2026/09/02", "archived_sessions"} {
		if err := os.MkdirAll(filepath.Join(home, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	db, err := Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return &codexEnv{t: t, home: home, db: db, src: []model.Source{codex.NewWithHome(home)}}
}

func (e *codexEnv) live() string {
	return filepath.Join(e.home, "sessions/2026/09/02", "rollout-2026-09-02T10-00-00-"+codexThread+".jsonl")
}

func (e *codexEnv) archived() string {
	return filepath.Join(e.home, "archived_sessions", filepath.Base(e.live()))
}

func (e *codexEnv) sync() Stats {
	e.t.Helper()
	st, err := e.db.Sync(context.Background(), e.src, Options{})
	if err != nil {
		e.t.Fatalf("sync: %v", err)
	}
	return st
}

func (e *codexEnv) rows(query string) [][3]string {
	e.t.Helper()
	rs, err := e.db.Query(query)
	if err != nil {
		e.t.Fatal(err)
	}
	defer rs.Close()
	var out [][3]string
	for rs.Next() {
		var r [3]string
		if err := rs.Scan(&r[0], &r[1], &r[2]); err != nil {
			e.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

const codexHead = `{"timestamp":"2026-09-02T02:00:00.000Z","type":"session_meta","payload":{"id":"` + codexThread + `","session_id":"` + codexThread + `","cwd":"/home/alice/code/erp","originator":"codex-tui","source":"cli","git":{"branch":"main"}}}
{"timestamp":"2026-09-02T02:00:00.100Z","type":"turn_context","payload":{"cwd":"/home/alice/code/erp","model":"gpt-5.5"}}
{"timestamp":"2026-09-02T02:00:01.000Z","type":"event_msg","payload":{"type":"user_message","message":"first platypus question"}}
{"timestamp":"2026-09-02T02:00:02.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first platypus answer"}]}}
`

const codexTail = `{"timestamp":"2026-09-02T02:01:00.000Z","type":"event_msg","payload":{"type":"user_message","message":"second platypus question"}}
{"timestamp":"2026-09-02T02:01:01.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"second platypus answer"}]}}
{"timestamp":"2026-09-02T02:01:01.100Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":900},"last_token_usage":{"input_tokens":800,"cached_input_tokens":500,"output_tokens":100,"total_tokens":900}}}}
`

func TestSyncCodexAppendKeepsFileContext(t *testing.T) {
	e := newCodexEnv(t)
	if err := os.WriteFile(e.live(), []byte(codexHead), 0o644); err != nil {
		t.Fatal(err)
	}
	if st := e.sync(); st.New != 1 || st.Messages != 2 {
		t.Fatalf("first sync: %+v", st)
	}
	f, _ := os.OpenFile(e.live(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(codexTail)
	f.Close()
	bumpMtime(t, e.live())

	if st := e.sync(); st.Appended != 1 || st.Rebuilt != 0 || st.Messages != 2 {
		t.Fatalf("append sync: %+v", st)
	}
	// The appended lines carry no session id, cwd or model of their own.
	got := e.rows(`SELECT session_id || '|' || cwd || '|' || branch, model, text FROM msgs ORDER BY seq`)
	if len(got) != 4 {
		t.Fatalf("messages = %v", got)
	}
	want := codexThread + "|/home/alice/code/erp|main"
	for _, r := range got {
		if r[0] != want {
			t.Fatalf("message %q has context %q, want %q", r[2], r[0], want)
		}
	}
	if got[3][1] != "gpt-5.5" {
		t.Errorf("appended reply model = %q, want gpt-5.5", got[3][1])
	}
	req := e.rows(`SELECT session_id, model, tok_in || '/' || cache_read FROM requests`)
	if len(req) != 1 || req[0][0] != codexThread || req[0][1] != "gpt-5.5" || req[0][2] != "300/500" {
		t.Errorf("requests = %v", req)
	}
	sess := e.rows(`SELECT source, project, first_prompt FROM sessions`)
	if len(sess) != 1 || sess[0] != [3]string{"codex", "-home-alice-code-erp", "first platypus question"} {
		t.Errorf("sessions = %v", sess)
	}
}

func TestSyncMovedFileIsNotIndexedTwice(t *testing.T) {
	e := newCodexEnv(t)
	os.WriteFile(e.live(), []byte(codexHead), 0o644)
	e.sync()

	// Archiving moves the transcript and may append to it on the way.
	if err := os.Rename(e.live(), e.archived()); err != nil {
		t.Fatal(err)
	}
	f, _ := os.OpenFile(e.archived(), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(codexTail)
	f.Close()
	bumpMtime(t, e.archived())

	st := e.sync()
	if st.Moved != 1 || st.New != 0 || st.Appended != 1 || st.Messages != 2 {
		t.Fatalf("sync after move: %+v", st)
	}
	var files, msgs int
	e.db.QueryRow(`SELECT count(*) FROM files`).Scan(&files)
	e.db.QueryRow(`SELECT count(*) FROM msgs`).Scan(&msgs)
	if files != 1 || msgs != 4 {
		t.Fatalf("files=%d msgs=%d, want 1/4", files, msgs)
	}
	var path string
	e.db.QueryRow(`SELECT path FROM files`).Scan(&path)
	if path != e.archived() {
		t.Errorf("file path = %s, want the new location", path)
	}

	// A copy (old path still present) is a separate file, not a move.
	os.WriteFile(e.live(), []byte(codexHead), 0o644)
	if st := e.sync(); st.Moved != 0 || st.New != 1 {
		t.Fatalf("sync after copy: %+v", st)
	}
}

func TestAddingCodexLeavesIndexedClaudeFilesAlone(t *testing.T) {
	e := newEnv(t) // Claude-only index
	e.write("s1.jsonl", userLine("s1", 1, "claude one")+userLine("s1", 2, "claude two"))
	e.write("s2.jsonl", userLine("s2", 1, "claude three"))
	if st := e.sync(); st.New != 2 {
		t.Fatalf("claude sync: %+v", st)
	}

	// Registering Codex must index only the Codex files.
	c := newCodexEnv(t)
	os.WriteFile(c.live(), []byte(codexHead), 0o644)
	e.src = append(e.src, c.src...)
	st := e.sync()
	if st.New != 1 || st.Unchanged != 2 || st.Rebuilt != 0 || st.Appended != 0 || st.Messages != 2 {
		t.Fatalf("sync after adding codex: %+v; claude files must stay untouched", st)
	}
}
