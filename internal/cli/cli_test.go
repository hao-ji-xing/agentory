package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const fixtureSession = "5f0c1d2e-0000-4000-8000-000000000001"

// harness runs the CLI against a private copy of testdata and a temp index.
type harness struct {
	t    *testing.T
	root string // .../claude/projects
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	claude := filepath.Join(dir, "claude")
	if err := os.CopyFS(claude, os.DirFS(filepath.Join("..", "..", "testdata", "claude"))); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("AGENTORY_DB", filepath.Join(dir, "index.db"))
	t.Setenv("NO_COLOR", "")
	return &harness{t: t, root: filepath.Join(claude, "projects")}
}

func (h *harness) run(args ...string) (stdout, stderr string, code int) {
	h.t.Helper()
	var out, errb bytes.Buffer
	code = Run(context.Background(), args, &out, &errb)
	return out.String(), errb.String(), code
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, errOut, code := h.run(args...)
	if code != 0 {
		h.t.Fatalf("agentory %v: exit %d\nstderr: %s", args, code, errOut)
	}
	return out
}

func (h *harness) json(v any, args ...string) {
	h.t.Helper()
	out := h.ok(append(args, "--json")...)
	if err := json.Unmarshal([]byte(out), v); err != nil {
		h.t.Fatalf("agentory %v --json: invalid JSON: %v\n%s", args, err, out)
	}
}

type hitsOut struct {
	Query string `json:"query"`
	Count int    `json:"count"`
	Plan  *struct {
		Mode string `json:"mode"`
	} `json:"plan"`
	Hits []struct {
		ID        int64  `json:"id"`
		SessionID string `json:"session_id"`
		AgentID   string `json:"agent_id"`
		Kind      string `json:"kind"`
		Project   string `json:"project"`
		Branch    string `json:"branch"`
		Title     string `json:"title"`
		Snippet   string `json:"snippet"`
		Before    []struct {
			ID int64 `json:"id"`
		} `json:"before"`
		After []struct {
			ID int64 `json:"id"`
		} `json:"after"`
	} `json:"hits"`
}

func (h *harness) search(args ...string) hitsOut {
	h.t.Helper()
	var out hitsOut
	h.json(&out, append([]string{"search"}, args...)...)
	return out
}

// writeBulk synthesizes a session with n records spread over the last
// days: every record mentions "bulkterm", record i also carries a unique
// token "marker<i>".
func (h *harness) writeBulk(project, sid string, n int, now time.Time) string {
	h.t.Helper()
	dir := filepath.Join(h.root, project)
	os.MkdirAll(dir, 0o755)
	var sb strings.Builder
	for i := 0; i < n; i++ {
		ts := now.Add(-time.Duration(n-i) * time.Minute).UTC().Format(time.RFC3339Nano)
		if i < n/2 {
			// First half is old: 40 days ago.
			ts = now.AddDate(0, 0, -40).Add(time.Duration(i) * time.Minute).UTC().Format(time.RFC3339Nano)
		}
		common := fmt.Sprintf(`"uuid":"%s-%d","timestamp":%q,"sessionId":%q,"cwd":"/home/bob/api","gitBranch":"main"`, sid, i, ts, sid)
		if i%2 == 0 {
			fmt.Fprintf(&sb, `{"type":"user",%s,"message":{"role":"user","content":"bulkterm question marker%d"}}`+"\n", common, i)
		} else {
			fmt.Fprintf(&sb, `{"type":"assistant",%s,"message":{"role":"assistant","content":[{"type":"text","text":"bulkterm answer marker%d"},{"type":"tool_use","id":"t%d","name":"Bash","input":{"command":"echo marker%d"}}]}}`+"\n", common, i, i, i)
		}
	}
	p := filepath.Join(dir, sid+".jsonl")
	if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
	return p
}

func TestNoFixtureContainsRealData(t *testing.T) {
	// Guard rail for the privacy rule: fixtures only use synthetic homes.
	filepath.WalkDir(filepath.Join("..", "..", "testdata"), func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		for _, bad := range []string{"/Users/", "sk-ant-", "@gmail.com"} {
			if bytes.Contains(b, []byte(bad)) {
				t.Errorf("%s contains %q; testdata must be synthetic", p, bad)
			}
		}
		return nil
	})
}

func TestEndToEndIndexAndSearch(t *testing.T) {
	h := newHarness(t)
	now := time.Now()
	h.writeBulk("-home-bob-api", "bulk-session", 120, now)

	var idx struct {
		Files, New, Messages int
	}
	h.json(&idx, "index")
	if idx.Files != 3 || idx.New != 3 {
		t.Fatalf("index summary: %+v", idx)
	}

	// All 120 records are searchable (60 prompts + 60 replies; tool_use hidden).
	res := h.search("bulkterm", "-n", "500")
	if res.Count != 120 {
		t.Fatalf("bulkterm hits = %d, want 120", res.Count)
	}
	res = h.search("marker77", "--all", "-n", "10")
	kinds := map[string]bool{}
	for _, hit := range res.Hits {
		kinds[hit.Kind] = true
	}
	if res.Count != 2 || !kinds["reply"] || !kinds["tool_use"] {
		t.Fatalf("marker77 with --all: %+v", res.Hits)
	}

	// -p and --since combined: only the recent half of the bob project.
	res = h.search("bulkterm", "-p", "api", "--since", "7d", "-n", "500")
	if res.Count != 60 {
		t.Fatalf("-p api --since 7d: %d hits, want 60", res.Count)
	}
	res = h.search("bulkterm", "-p", "shop", "-n", "500")
	if res.Count != 0 {
		t.Fatalf("-p shop should exclude the api project, got %d", res.Count)
	}
	res = h.search("bulkterm", "-u", "30d", "-n", "500")
	if res.Count != 60 {
		t.Fatalf("--until 30d: %d hits, want 60", res.Count)
	}
	res = h.search("bulkterm", "-k", "prompt", "-n", "500")
	if res.Count != 60 {
		t.Fatalf("-k prompt: %d hits, want 60", res.Count)
	}
}

func TestSearchFixtureRules(t *testing.T) {
	h := newHarness(t)

	// 2-char Chinese → LIKE fallback.
	res := h.search("折扣", "--explain")
	if res.Plan == nil || res.Plan.Mode != "like" || res.Count != 2 {
		t.Fatalf("折扣: plan=%+v count=%d", res.Plan, res.Count)
	}
	// 4-char Chinese → FTS.
	res = h.search("重复扣了", "--explain")
	if res.Plan.Mode != "fts" || res.Count != 1 || res.Hits[0].Kind != "prompt" {
		t.Fatalf("重复扣了: plan=%+v hits=%+v", res.Plan, res.Hits)
	}
	hit := res.Hits[0]
	if hit.Project != "shop" || hit.Branch != "feat-invoice" || hit.Title != "Fix invoice discount" {
		t.Fatalf("hit metadata: %+v", hit)
	}
	if strings.Contains(hit.Snippet, "system-reminder") || strings.Contains(hit.Snippet, "opened shop/invoice.go") {
		t.Fatalf("system-reminder leaked into the index: %q", hit.Snippet)
	}

	// Default kinds hide tool traffic; --all shows it.
	if res := h.search("caller"); res.Count != 0 {
		t.Fatalf("tool_result must be hidden by default: %+v", res.Hits)
	}
	if res := h.search("caller", "--all"); res.Count != 2 {
		t.Fatalf("--all should expose tool_use and tool_result, got %+v", res.Hits)
	}
	if res := h.search("applyDiscount", "--tool", "grep"); res.Count != 1 || res.Hits[0].Kind != "tool_use" {
		t.Fatalf("--tool grep: %+v", res.Hits)
	}

	// Commands, meta and summaries are classified.
	if res := h.search("/model opus", "-k", "command"); res.Count != 1 {
		t.Fatalf("command: %+v", res.Hits)
	}
	if res := h.search("interrupted"); res.Count != 0 {
		t.Fatal("interruptions are meta and hidden by default")
	}
	if res := h.search("interrupted", "-k", "meta"); res.Count != 1 {
		t.Fatal("interruption should be kind=meta")
	}
	if res := h.search("being continued", "-k", "summary"); res.Count != 1 {
		t.Fatal("compact summary should be kind=summary")
	}
	if res := h.search("Set model to"); res.Count != 0 {
		t.Fatal("local-command-stdout must be stripped")
	}

	// Sub-agent messages are attributed to the parent session but hidden.
	if res := h.search("zanzibar"); res.Count != 0 {
		t.Fatal("sub-agent hits must be opt-in")
	}
	res = h.search("zanzibar", "--include-subagent")
	if res.Count != 1 || res.Hits[0].SessionID != fixtureSession || res.Hits[0].AgentID != "a1b2c3" {
		t.Fatalf("--include-subagent: %+v", res.Hits)
	}
}

func TestContextFlag(t *testing.T) {
	h := newHarness(t)
	res := h.search("tax line", "-C", "2")
	if res.Count != 1 {
		t.Fatalf("setup: %+v", res.Hits)
	}
	hit := res.Hits[0]
	// Visible neighbours before: the reply with the conclusion and the one
	// before it; after: only the compact summary (system is hidden).
	if len(hit.Before) != 2 || len(hit.After) != 1 {
		t.Fatalf("context sizes: before=%d after=%d", len(hit.Before), len(hit.After))
	}

	var shown struct {
		Message struct {
			ID   int64  `json:"id"`
			Text string `json:"text"`
		} `json:"message"`
		Before []json.RawMessage `json:"before"`
		After  []json.RawMessage `json:"after"`
	}
	h.json(&shown, "show", fmt.Sprint(hit.ID), "-C", "1", "--all")
	if shown.Message.ID != hit.ID || len(shown.Before) != 1 || len(shown.After) != 1 {
		t.Fatalf("show -C 1 --all: %+v", shown)
	}
	if !strings.Contains(shown.Message.Text, "[image]") {
		t.Fatalf("image block should be recorded as [image]: %q", shown.Message.Text)
	}
}

func TestHumanOutput(t *testing.T) {
	h := newHarness(t)
	out := h.ok("applyDiscount", "--color", "never")
	if !strings.Contains(out, "shop/feat-invoice") || !strings.Contains(out, "→ agentory show ") || !strings.Contains(out, "a>") {
		t.Fatalf("unexpected human output:\n%s", out)
	}
	if strings.Contains(out, "\x1b[") {
		t.Fatal("no ANSI escapes when color is off")
	}
	out = h.ok("applyDiscount", "--color", "always", "--no-sync")
	if !strings.Contains(out, ansiHit+"applyDiscount"+ansiReset) {
		t.Fatalf("--color always should highlight the match:\n%q", out)
	}
	out = h.ok("折扣", "--explain", "--no-sync")
	if !strings.Contains(out, "mode=like") || !strings.Contains(out, "LIKE") {
		t.Fatalf("--explain should show the LIKE fallback:\n%s", out)
	}

	out = h.ok("show", fixtureSession[:8])
	if !strings.Contains(out, "title   Fix invoice discount") || !strings.Contains(out, "Why does the invoice total") {
		t.Fatalf("show session:\n%s", out)
	}
}

func TestLiveAppendIsSearchable(t *testing.T) {
	h := newHarness(t)
	h.ok("index")
	path := filepath.Join(h.root, "-home-alice-code-shop", fixtureSession+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().UTC().Format(time.RFC3339)
	fmt.Fprintf(f, `{"type":"user","uuid":"live1","timestamp":%q,"sessionId":%q,"cwd":"/home/alice/code/shop","message":{"role":"user","content":"freshly appended wombat"}}`+"\n", ts, fixtureSession)
	// A half-written line at the end must not break anything.
	fmt.Fprint(f, `{"type":"user","uuid":"live2","message":{"role":"user","content":"half wri`)
	f.Close()
	later := time.Now().Add(2 * time.Second)
	os.Chtimes(path, later, later)

	if res := h.search("wombat", "--no-sync"); res.Count != 0 {
		t.Fatal("--no-sync must not update the index")
	}
	if res := h.search("wombat"); res.Count != 1 {
		t.Fatal("auto-sync before query should pick up appended lines")
	}
	var st struct {
		Messages int `json:"messages"`
	}
	h.json(&st, "stats", "--no-sync")
	var idx struct{ Unchanged, Appended, Rebuilt int }
	h.json(&idx, "index")
	if idx.Appended != 0 || idx.Rebuilt != 0 {
		t.Fatalf("nothing changed, nothing should be re-read: %+v", idx)
	}
}

func TestListings(t *testing.T) {
	h := newHarness(t)
	h.writeBulk("-home-bob-api", "bulk-session", 10, time.Now())
	var ss []struct {
		ID, Title, Project string
		NMsg               int `json:"n_msg"`
	}
	h.json(&ss, "sessions")
	if len(ss) != 2 || ss[0].ID != "bulk-session" || ss[1].Title != "Fix invoice discount" || ss[1].Project != "shop" {
		t.Fatalf("sessions: %+v", ss)
	}
	var ps []struct {
		Project  string
		Sessions int
	}
	h.json(&ps, "projects")
	if len(ps) != 2 || ps[0].Project != "api" {
		t.Fatalf("projects: %+v", ps)
	}
	var st struct {
		Files    int
		Sessions int
		ByKind   map[string]int `json:"by_kind"`
	}
	h.json(&st, "stats")
	if st.Files != 3 || st.Sessions != 2 || st.ByKind["command"] != 1 || st.ByKind["summary"] != 1 {
		t.Fatalf("stats: %+v", st)
	}
	out := h.ok("doctor")
	if strings.Contains(out, "FAIL") || !strings.Contains(out, "fts consistency") {
		t.Fatalf("doctor:\n%s", out)
	}
}

func TestErrors(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"search"}, 2, "missing query"},
		{[]string{"x", "-k", "bogus"}, 2, "unknown kind"},
		{[]string{"x", "--since", "someday"}, 2, "cannot parse time"},
		{[]string{"x", "--source", "codex"}, 2, "unknown source"},
		{[]string{"show", "999999"}, 1, "no message"},
		{[]string{"show", "nosuchsession"}, 1, "no session"},
		{[]string{"x", "--bogus-flag"}, 2, "flag provided but not defined"},
	}
	for _, c := range cases {
		_, errOut, code := h.run(c.args...)
		if code != c.code || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: exit %d stderr %q, want %d/%q", c.args, code, errOut, c.code, c.msg)
		}
	}
	if out := h.ok("help", "search"); !strings.Contains(out, "--include-subagent") {
		t.Errorf("help search:\n%s", out)
	}
	if out := h.ok("--version"); !strings.HasPrefix(out, "agentory ") {
		t.Errorf("version: %q", out)
	}
}

func TestFullModeRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.ok("index")
	var idx struct {
		Rebuilt int
		Full    bool
	}
	h.json(&idx, "index", "--full")
	if !idx.Full || idx.Rebuilt != 2 {
		t.Fatalf("--full should rebuild every file: %+v", idx)
	}
	h.json(&idx, "index")
	if !idx.Full || idx.Rebuilt != 0 {
		t.Fatalf("full mode should be remembered: %+v", idx)
	}
	h.json(&idx, "index", "--full=false")
	if idx.Full || idx.Rebuilt != 2 {
		t.Fatalf("--full=false should revert: %+v", idx)
	}
}

// writeSkills adds a session with skill calls (one with arguments far past
// the truncation limit) and typed slash commands.
func (h *harness) writeSkills(now time.Time) {
	h.t.Helper()
	long := strings.Repeat("please review everything carefully ", 200)
	var sb strings.Builder
	for i, body := range []string{
		`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"Skill","input":{"skill":"release-notes","args":"v1"}}]}`,
		`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t2","name":"Skill","input":{"skill":"code-review","args":"` + long + `"}}]}`,
		`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t3","name":"Skill","input":{"skill":"code-review","args":""}}]}`,
		`"type":"user","message":{"role":"user","content":"<command-name>/code-review</command-name>\n<command-args>high</command-args>"}`,
		`"type":"user","message":{"role":"user","content":"<command-name>/clear</command-name>\n<command-args></command-args>"}`,
	} {
		ts := now.Add(time.Duration(i-10) * time.Minute).UTC().Format(time.RFC3339)
		fmt.Fprintf(&sb, `{"uuid":"k%d","timestamp":%q,"sessionId":"skills-session","cwd":"/home/bob/api",%s}`+"\n", i, ts, body)
	}
	dir := filepath.Join(h.root, "-home-bob-api")
	os.MkdirAll(dir, 0o755)
	if err := os.WriteFile(filepath.Join(dir, "skills-session.jsonl"), []byte(sb.String()), 0o644); err != nil {
		h.t.Fatal(err)
	}
}

type topOut struct {
	By      string `json:"by"`
	Total   int    `json:"total"`
	Groups  int    `json:"groups"`
	Buckets []struct {
		Key   string `json:"key"`
		Count int    `json:"count"`
	} `json:"buckets"`
}

func (o topOut) String() string {
	var parts []string
	for _, b := range o.Buckets {
		parts = append(parts, fmt.Sprintf("%s=%d", b.Key, b.Count))
	}
	return strings.Join(parts, " ")
}

func TestTopCommand(t *testing.T) {
	h := newHarness(t)
	h.writeSkills(time.Now())

	var out topOut
	h.json(&out, "top", "--by", "skill", "--since", "7d")
	if out.String() != "code-review=2 release-notes=1" || out.Total != 3 {
		t.Fatalf("top --by skill: %s (total %d)", out, out.Total)
	}
	out = topOut{}
	h.json(&out, "top", "-b", "command", "-s", "7d")
	if out.String() != "/clear=1 /code-review=1" {
		t.Fatalf("top --by command: %s", out)
	}
	// The fixture's own commands are older than a week; without --since
	// they are counted too.
	out = topOut{}
	h.json(&out, "top", "--by", "command")
	if out.Total != 3 {
		t.Fatalf("top --by command (all time): %s", out)
	}
	out = topOut{}
	h.json(&out, "top", "--by", "input:subagent_type")
	if out.String() != "Explore=1" {
		t.Fatalf("top --by input:subagent_type: %s", out)
	}
	txt := h.ok("top", "--by", "skill", "-n", "1", "--color", "never")
	if !strings.Contains(txt, "code-review") || !strings.Contains(txt, "3 messages in 2 groups by skill, showing 1") {
		t.Fatalf("human top output:\n%s", txt)
	}

	for _, args := range [][]string{{"top"}, {"top", "--by", "bogus"}, {"top", "--by", "input:a b"}} {
		if _, errOut, code := h.run(args...); code != 2 {
			t.Errorf("%v: exit %d (%s), want usage error", args, code, errOut)
		}
	}
}

func TestSearchFullText(t *testing.T) {
	h := newHarness(t)
	h.writeSkills(time.Now())
	var res struct {
		Hits []struct {
			Snippet string `json:"snippet"`
			Text    string `json:"text"`
		} `json:"hits"`
	}
	h.json(&res, "search", "carefully", "--tool", "Skill", "--full-text")
	if len(res.Hits) != 1 || !strings.HasPrefix(res.Hits[0].Text, "skill=code-review\nargs=please review") ||
		len(res.Hits[0].Text) <= len(res.Hits[0].Snippet) {
		t.Fatalf("--full-text should return the stored text: %+v", res.Hits)
	}
	res.Hits = nil
	h.json(&res, "search", "carefully", "--tool", "Skill")
	if len(res.Hits) != 1 || res.Hits[0].Text != "" {
		t.Fatal("text is only included with --full-text")
	}
}

func TestTopKeyCommand(t *testing.T) {
	h := newHarness(t)
	h.writeSkills(time.Now())
	txt := h.ok("top", "--by", "command", "--key", "/code-review", "--color", "never")
	if !strings.Contains(txt, "with args 1/1") || strings.Contains(txt, "/clear") {
		t.Fatalf("top --key:\n%s", txt)
	}
	txt = h.ok("top", "--by", "skill", "--color", "never")
	if !strings.Contains(txt, "code-review") || !strings.Contains(txt, "with args 1/2") {
		t.Fatalf("top --by skill with_args:\n%s", txt)
	}
}
