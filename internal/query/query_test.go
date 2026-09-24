package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/claudecode"
)

var day0 = time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)

type rec struct {
	kind  string // user | assistant | tool | sub
	text  string
	dayAt int // days after day0
}

func line(sid, cwd string, i int, r rec) string {
	ts := day0.AddDate(0, 0, r.dayAt).Add(time.Duration(i) * time.Minute).Format(time.RFC3339)
	common := fmt.Sprintf(`"uuid":"%s-%d","sessionId":%q,"timestamp":%q,"cwd":%q,"gitBranch":"main"`, sid, i, sid, ts, cwd)
	switch r.kind {
	case "assistant":
		return fmt.Sprintf(`{"type":"assistant",%s,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, common, r.text)
	case "tool":
		return fmt.Sprintf(`{"type":"user",%s,"message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":%q}]}}`, common, r.text)
	case "sub":
		return fmt.Sprintf(`{"type":"assistant",%s,"agentId":"ag1","isSidechain":true,"message":{"role":"assistant","content":[{"type":"text","text":%q}]}}`, common, r.text)
	default:
		return fmt.Sprintf(`{"type":"user",%s,"message":{"role":"user","content":%q}}`, common, r.text)
	}
}

// fixture builds an index with two projects and returns it.
func fixture(t *testing.T) *index.DB {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	files := map[string]struct {
		sid, cwd string
		recs     []rec
	}{
		"-work-erp/s-erp.jsonl": {"s-erp", "/work/erp", []rec{
			{"user", "PO 的库存锁定为什么会双扣？", 0},
			{"assistant", "库存锁定的口径是先锁后扣。", 0},
			{"user", "那发票识别呢", 0},
			{"assistant", "发票走 invoice queue 异步识别。", 0},
			{"tool", "invoice queue depth=3 发票", 0},
			{"user", "deadlock in the schedule line worker", 5},
			{"assistant", "The deadlock comes from lock ordering.", 5},
		}},
		"-work-web/s-web.jsonl": {"s-web", "/work/web", []rec{
			{"user", "render deadlock in React effect", 2},
			{"assistant", "Effect loops cause the render storm.", 2},
			{"user", "发票页面样式错位", 2},
		}},
		"-work-erp/s-erp/subagents/agent-ag1.jsonl": {"s-erp", "/work/erp", []rec{
			{"sub", "subagent found deadlock evidence", 5},
		}},
	}
	for rel, f := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		var sb strings.Builder
		for i, r := range f.recs {
			sb.WriteString(line(f.sid, f.cwd, i, r) + "\n")
		}
		if err := os.WriteFile(p, []byte(sb.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := index.Open(filepath.Join(dir, "index.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Sync(context.Background(), []model.Source{claudecode.NewWithRoot(root)}, index.Options{}); err != nil {
		t.Fatal(err)
	}
	return db
}

func texts(ms []Message) []string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Text)
	}
	sort.Strings(out)
	return out
}

func search(t *testing.T, db *index.DB, q string, f Filter) ([]Message, *Plan) {
	t.Helper()
	ms, p, err := Search(db, q, f)
	if err != nil {
		t.Fatalf("search %q: %v", q, err)
	}
	return ms, p
}

func expect(t *testing.T, got []Message, want ...string) {
	t.Helper()
	sort.Strings(want)
	if g := texts(got); strings.Join(g, "|") != strings.Join(want, "|") {
		t.Fatalf("hits = %q\nwant  %q", g, want)
	}
}

func TestSearchChineseTwoCharsUsesLike(t *testing.T) {
	db := fixture(t)
	ms, p := search(t, db, "发票", Filter{})
	if p.Mode != "like" || p.Terms[0].FTS {
		t.Fatalf("2-char term must use LIKE, plan=%+v", p)
	}
	expect(t, ms, "那发票识别呢", "发票走 invoice queue 异步识别。", "发票页面样式错位")
}

func TestSearchChineseFourCharsUsesFTS(t *testing.T) {
	db := fixture(t)
	ms, p := search(t, db, "库存锁定", Filter{})
	if p.Mode != "fts" || p.Match != `"库存锁定"` {
		t.Fatalf("4-char term must use FTS MATCH, plan=%+v", p)
	}
	expect(t, ms, "PO 的库存锁定为什么会双扣？", "库存锁定的口径是先锁后扣。")
}

func TestSearchEnglishAndMultiTerm(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "DEADLOCK", Filter{})
	expect(t, ms, "deadlock in the schedule line worker", "The deadlock comes from lock ordering.", "render deadlock in React effect")

	ms, p := search(t, db, "deadlock render", Filter{})
	if p.Mode != "fts" || p.Match != `"deadlock" AND "render"` {
		t.Fatalf("plan=%+v", p)
	}
	expect(t, ms, "render deadlock in React effect")

	// Quoted phrase is one term.
	ms, _ = search(t, db, `"lock ordering"`, Filter{})
	expect(t, ms, "The deadlock comes from lock ordering.")
}

func TestSearchMixedFTSAndLike(t *testing.T) {
	db := fixture(t)
	ms, p := search(t, db, "invoice 发票", Filter{})
	if p.Mode != "fts+like" {
		t.Fatalf("plan=%+v", p)
	}
	expect(t, ms, "发票走 invoice queue 异步识别。")
}

func TestSearchKindsAndAll(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "queue", Filter{})
	expect(t, ms, "发票走 invoice queue 异步识别。") // tool_result hidden by default

	all := append([]model.Kind{}, model.AllKinds...)
	ms, _ = search(t, db, "queue", Filter{Kinds: all})
	expect(t, ms, "发票走 invoice queue 异步识别。", "invoice queue depth=3 发票")

	ms, _ = search(t, db, "deadlock", Filter{Kinds: []model.Kind{model.KindPrompt}})
	expect(t, ms, "deadlock in the schedule line worker", "render deadlock in React effect")
}

func TestSearchProjectAndSince(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "deadlock", Filter{Project: "erp"})
	expect(t, ms, "deadlock in the schedule line worker", "The deadlock comes from lock ordering.")

	ms, _ = search(t, db, "deadlock", Filter{Since: day0.AddDate(0, 0, 3)})
	expect(t, ms, "deadlock in the schedule line worker", "The deadlock comes from lock ordering.")

	ms, _ = search(t, db, "deadlock", Filter{Until: day0.AddDate(0, 0, 3)})
	expect(t, ms, "render deadlock in React effect")

	ms, _ = search(t, db, "发票", Filter{Project: "web", Since: day0.AddDate(0, 0, 1)})
	expect(t, ms, "发票页面样式错位")
}

func TestSearchSubagentToggle(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "evidence", Filter{})
	expect(t, ms)
	ms, _ = search(t, db, "evidence", Filter{IncludeSubagent: true})
	expect(t, ms, "subagent found deadlock evidence")
	if ms[0].AgentID != "ag1" || ms[0].SessionID != "s-erp" {
		t.Fatalf("subagent hit should belong to the parent session: %+v", ms[0])
	}
}

func TestSearchRoleAndLimitAndOrder(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "deadlock", Filter{Role: "assistant"})
	expect(t, ms, "The deadlock comes from lock ordering.")

	ms, _ = search(t, db, "deadlock", Filter{Limit: 1})
	if len(ms) != 1 || ms[0].Text != "The deadlock comes from lock ordering." {
		t.Fatalf("limit 1 should return the newest hit, got %q", texts(ms))
	}
}

func TestSearchEscapesLikeWildcards(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "%", Filter{})
	expect(t, ms)
}

func TestContext(t *testing.T) {
	db := fixture(t)
	ms, _ := search(t, db, "那发票", Filter{})
	if len(ms) != 1 {
		t.Fatalf("setup: %q", texts(ms))
	}
	before, after, err := Context(db, ms[0], 2, nil)
	if err != nil {
		t.Fatal(err)
	}
	// tool_result is not a default kind, so it is skipped rather than counted.
	eq := func(got []Message, want ...string) {
		t.Helper()
		var g []string
		for _, m := range got {
			g = append(g, m.Text)
		}
		if strings.Join(g, "|") != strings.Join(want, "|") {
			t.Fatalf("context = %q, want %q", g, want)
		}
	}
	eq(before, "PO 的库存锁定为什么会双扣？", "库存锁定的口径是先锁后扣。")
	eq(after, "发票走 invoice queue 异步识别。", "deadlock in the schedule line worker")

	// At the end of a session the context must not spill into another
	// session or the sub-agent transcript.
	ms, _ = search(t, db, "lock ordering", Filter{})
	before, after, _ = Context(db, ms[0], 3, nil)
	eq(after)
	eq(before, "那发票识别呢", "发票走 invoice queue 异步识别。", "deadlock in the schedule line worker")

	ms, _ = search(t, db, "样式错位", Filter{})
	before, after, _ = Context(db, ms[0], 5, nil)
	eq(before, "render deadlock in React effect", "Effect loops cause the render storm.")
	eq(after)
}

func TestSessionsAndProjects(t *testing.T) {
	db := fixture(t)
	ss, err := Sessions(db, Filter{})
	if err != nil || len(ss) != 2 || ss[0].ID != "s-erp" {
		t.Fatalf("sessions: %+v %v", ss, err)
	}
	if ss[0].NMsg != 7 || ss[0].Project != "erp" || ss[0].FirstPrompt != "PO 的库存锁定为什么会双扣？" {
		t.Fatalf("session row: %+v", ss[0])
	}
	ss, _ = Sessions(db, Filter{Project: "web"})
	if len(ss) != 1 || ss[0].ID != "s-web" {
		t.Fatalf("project filter: %+v", ss)
	}
	s, err := FindSession(db, "s-w")
	if err != nil || s.ID != "s-web" {
		t.Fatalf("prefix lookup: %+v %v", s, err)
	}
	if _, err := FindSession(db, "s-"); err == nil {
		t.Fatal("ambiguous prefix must fail")
	}
	msgs, _ := SessionMessages(db, "s-web", nil, false)
	if len(msgs) != 3 || msgs[0].Text != "render deadlock in React effect" {
		t.Fatalf("session messages: %q", texts(msgs))
	}
	ps, err := Projects(db)
	if err != nil || len(ps) != 2 || ps[0].Project != "erp" || ps[0].Sessions != 1 {
		t.Fatalf("projects: %+v %v", ps, err)
	}
	st, err := GetStats(db)
	if err != nil || st.Messages != 11 || st.ByKind["tool_result"] != 1 || st.Sessions != 2 {
		t.Fatalf("stats: %+v %v", st, err)
	}
}

func TestParseTerms(t *testing.T) {
	got := ParseTerms(`  发票  "lock  ordering" ab 库存锁定 `)
	want := []Term{{"发票", false}, {"lock  ordering", true}, {"ab", false}, {"库存锁定", true}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSnippetAndHighlight(t *testing.T) {
	terms := ParseTerms("needle")
	text := strings.Repeat("a", 100) + " NEEDLE " + strings.Repeat("b", 100)
	s := Snippet(text, terms, 40)
	if !strings.Contains(s, "NEEDLE") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") {
		t.Fatalf("snippet should center on the match: %q", s)
	}
	if got := Highlight("发票 and 发票", ParseTerms("发票"), "[", "]"); got != "[发票] and [发票]" {
		t.Fatalf("highlight: %q", got)
	}
	if got := Highlight("Deadlock", ParseTerms("deadLOCK lock"), "[", "]"); got != "[Deadlock]" {
		t.Fatalf("overlapping terms must merge: %q", got)
	}
}

func TestSearchToolTextIsScanned(t *testing.T) {
	db := fixture(t)
	// "depth" only occurs in a tool_result, which is not full-text indexed.
	ms, p := search(t, db, "depth", Filter{Kinds: model.AllKinds})
	if !p.ToolScan || len(ms) != 1 || ms[0].Kind != "tool_result" {
		t.Fatalf("--all must find tool output by scanning: plan=%+v hits=%q", p, texts(ms))
	}
	// Mixed with a short LIKE term and a prompt hit for the same terms.
	ms, _ = search(t, db, "invoice 发票", Filter{Kinds: model.AllKinds})
	expect(t, ms, "发票走 invoice queue 异步识别。", "invoice queue depth=3 发票")
	if _, p := search(t, db, "depth", Filter{}); p.ToolScan {
		t.Fatal("default kinds must not scan tool text")
	}
}

func TestSearchToolScanHonoursFilters(t *testing.T) {
	db := fixture(t)
	all := model.AllKinds
	cases := []struct {
		f    Filter
		want int
	}{
		{Filter{Kinds: all}, 1},
		{Filter{Kinds: all, Since: day0.AddDate(0, 0, 1)}, 0}, // the tool_result is from day 0
		{Filter{Kinds: all, Until: day0.AddDate(0, 0, 1)}, 1},
		{Filter{Kinds: []model.Kind{model.KindToolUse}}, 0}, // it is a tool_result
		{Filter{Kinds: all, Tool: "Bash"}, 0},               // tool results carry no tool name
		{Filter{Kinds: all, Project: "web"}, 0},
	}
	for _, c := range cases {
		ms, p := search(t, db, "depth", c.f)
		if len(ms) != c.want || !p.ToolScan {
			t.Errorf("%+v: %d hits (tool scan %v), want %d", c.f, len(ms), p.ToolScan, c.want)
		}
	}
	// Terms and pushed-down filters bind in the right order.
	ms, _ := search(t, db, "invoice depth", Filter{Kinds: all, Since: day0.AddDate(0, 0, -1), Until: day0.AddDate(0, 0, 1)})
	expect(t, ms, "invoice queue depth=3 发票")
}
