package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/claudecode"
)

// measureFixture: two sessions in two projects with token usage (one
// request spread over two lines, one sub-agent request), turns, tool calls
// with and without errors, and session cost snapshots.
func measureFixture(t *testing.T) *index.DB {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	at := func(day, min int) string {
		return day0.AddDate(0, 0, day).Add(time.Duration(min) * time.Minute).Format(time.RFC3339)
	}
	asst := func(sid, cwd string, day, min int, rid, mdl string, out int, content, extra string) string {
		return fmt.Sprintf(`{"type":"assistant","uuid":"%s-%d","sessionId":%q,"timestamp":%q,"cwd":%q,"requestId":%q,%s`+
			`"message":{"model":%q,"role":"assistant","content":[%s],`+
			`"usage":{"input_tokens":10,"output_tokens":%d,"cache_read_input_tokens":900,"cache_creation_input_tokens":90,`+
			`"cache_creation":{"ephemeral_5m_input_tokens":0,"ephemeral_1h_input_tokens":90}}}}`+"\n",
			rid, min, sid, at(day, min), cwd, rid, extra, mdl, content, out)
	}
	text := func(s string) string { return fmt.Sprintf(`{"type":"text","text":%q}`, s) }
	tool := func(id, name string) string {
		return fmt.Sprintf(`{"type":"tool_use","id":%q,"name":%q,"input":{"command":"x"}}`, id, name)
	}
	result := func(sid, cwd string, day, min int, id string, isErr bool) string {
		return fmt.Sprintf(`{"type":"user","uuid":"r-%s","sessionId":%q,"timestamp":%q,"cwd":%q,"message":{"role":"user","content":`+
			`[{"type":"tool_result","tool_use_id":%q,"is_error":%v,"content":"out"}]}}`+"\n", id, sid, at(day, min), cwd, id, isErr)
	}
	turn := func(sid, cwd string, day, min int, ms int) string {
		return fmt.Sprintf(`{"type":"system","subtype":"turn_duration","sessionId":%q,"timestamp":%q,"cwd":%q,"durationMs":%d,"messageCount":4}`+"\n",
			sid, at(day, min), cwd, ms)
	}
	cost := func(sid string, usd float64) string {
		return fmt.Sprintf(`{"type":"cost-state","sessionId":%q,"totalCostUSD":%g,"totalLinesAdded":100,"totalLinesRemoved":10,"totalDuration":1000}`+"\n", sid, usd)
	}
	prompt := func(sid, cwd string, day, min int, s string) string {
		return fmt.Sprintf(`{"type":"user","uuid":"p-%d","sessionId":%q,"timestamp":%q,"cwd":%q,"message":{"role":"user","content":%q}}`+"\n",
			min, sid, at(day, min), cwd, s)
	}
	files := map[string]string{
		"-w-shop/a.jsonl": prompt("a", "/w/shop", 0, 1, "fix the build") +
			// req1 is written as two lines (text block, then tool_use block).
			asst("a", "/w/shop", 0, 2, "req1", "opus", 50, text("looking"), "") +
			asst("a", "/w/shop", 0, 2, "req1", "opus", 50, tool("t1", "Bash"), "") +
			result("a", "/w/shop", 0, 3, "t1", true) +
			asst("a", "/w/shop", 0, 4, "req2", "opus", 30, tool("t2", "Bash"), "") +
			result("a", "/w/shop", 0, 5, "t2", false) +
			turn("a", "/w/shop", 0, 6, 60000) + cost("a", 2.5) + cost("a", 3.75),
		"-w-shop/a/subagents/agent-x.jsonl": asst("a", "/w/shop", 0, 3, "req3", "haiku", 5, text("sub"), `"agentId":"x","isSidechain":true,`) +
			turn("a", "/w/shop", 0, 3, 1000),
		"-w-api/b.jsonl": prompt("b", "/w/api", 2, 1, "add an endpoint") +
			asst("b", "/w/api", 2, 2, "req4", "sonnet", 20, tool("t3", "Read"), "") +
			turn("b", "/w/api", 2, 3, 30000) + turn("b", "/w/api", 2, 9, 90000) + cost("b", 1.25),
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
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

func mtop(t *testing.T, db *index.DB, by string, f Filter, opt TopOptions) *TopResult {
	t.Helper()
	res, _, err := Top(db, "", by, f, opt)
	if err != nil {
		t.Fatalf("top by %s %+v: %v", by, opt, err)
	}
	return res
}

func TestTopTokensCountEachRequestOnce(t *testing.T) {
	db := measureFixture(t)
	res := mtop(t, db, "project", Filter{}, TopOptions{Measure: MeasureTokens})
	got := map[string]Tokens{}
	for _, b := range res.Buckets {
		got[b.Key] = *b.Tokens
	}
	// shop: req1 (two lines, counted once) + req2; the sub-agent's req3 is
	// excluded by default.
	shop := got["shop"]
	if shop.Requests != 2 || shop.Output != 80 || shop.Input != 20 || shop.CacheRead != 1800 || shop.CacheWrite1h != 180 {
		t.Fatalf("shop tokens = %+v", shop)
	}
	if shop.HitRate != 90 { // 1800 / (20+1800+180)
		t.Fatalf("hit rate = %v, want 90", shop.HitRate)
	}
	if got["api"].Output != 20 || res.All.Tokens.Output != 100 || res.All.Tokens.Requests != 3 {
		t.Fatalf("totals: api=%+v all=%+v", got["api"], res.All.Tokens)
	}

	res = mtop(t, db, "model", Filter{IncludeSubagent: true}, TopOptions{Measure: MeasureTokens})
	if buckets(res) != "opus=2 sonnet=1 haiku=1" {
		t.Fatalf("tokens by model incl. sub-agents: %s", buckets(res))
	}
	res = mtop(t, db, "agent", Filter{}, TopOptions{Measure: MeasureTokens})
	if buckets(res) != "main=3 subagent=1" {
		t.Fatalf("--by agent must include sub-agents: %s", buckets(res))
	}
}

func TestTopTurnsAndCost(t *testing.T) {
	db := measureFixture(t)
	res := mtop(t, db, "project", Filter{}, TopOptions{Measure: MeasureTurns})
	if len(res.Buckets) != 2 || res.Buckets[0].Key != "api" || res.Buckets[0].Turns.Turns != 2 ||
		res.Buckets[0].Turns.DurationMs != 120000 || res.Buckets[0].Turns.AvgMs != 60000 {
		t.Fatalf("turns by project: %+v", res.Buckets)
	}
	res = mtop(t, db, "session", Filter{}, TopOptions{Measure: MeasureCost})
	if len(res.Buckets) != 2 || res.Buckets[0].Key != "a" || res.Buckets[0].Cost.USD != 3.75 ||
		res.All.Cost.USD != 5 || res.All.Cost.LinesAdded != 200 {
		t.Fatalf("cost uses the latest snapshot per session: %+v all=%+v", res.Buckets, res.All.Cost)
	}
	res = mtop(t, db, "day", Filter{Since: day0.AddDate(0, 0, 1)}, TopOptions{Measure: MeasureCost})
	if len(res.Buckets) != 1 || res.Buckets[0].Cost.USD != 1.25 {
		t.Fatalf("cost filtered by session start: %+v", res.Buckets)
	}
}

func TestTopErrorAndMultiDimension(t *testing.T) {
	db := measureFixture(t)
	res := mtop(t, db, "error", Filter{}, TopOptions{})
	if buckets(res) != "error=1 no result=1 ok=1" {
		t.Fatalf("error dimension: %s", buckets(res))
	}
	res = mtop(t, db, "tool,error", Filter{}, TopOptions{})
	if buckets(res) != "Bash / error=1 Bash / ok=1 Read / no result=1" {
		t.Fatalf("tool,error: %s", buckets(res))
	}
	if len(res.Buckets[0].Keys) != 2 || res.Buckets[0].Keys[1] != "error" {
		t.Fatalf("multi-dimension buckets carry their keys: %+v", res.Buckets[0])
	}
	res = mtop(t, db, "project,model", Filter{Kinds: model.AllKinds}, TopOptions{})
	if buckets(res) != "shop / opus=3 api / sonnet=1" {
		t.Fatalf("project,model message counts: %s", buckets(res))
	}
	res = mtop(t, db, "weekday", Filter{Kinds: model.AllKinds}, TopOptions{})
	if len(res.Buckets) != 2 || res.Buckets[0].Key[1] != ' ' {
		t.Fatalf("weekday keys are 'N Day': %s", buckets(res))
	}
}

func TestTopRejectsInvalidCombinations(t *testing.T) {
	db := measureFixture(t)
	cases := []struct {
		q, by string
		f     Filter
		opt   TopOptions
	}{
		{"", "skill", Filter{}, TopOptions{Measure: MeasureTokens}},        // not a requests dimension
		{"", "model", Filter{}, TopOptions{Measure: MeasureCost}},          // cost has no model
		{"build", "project", Filter{}, TopOptions{Measure: MeasureTokens}}, // query terms need messages
		{"", "project", Filter{Tool: "Bash"}, TopOptions{Measure: MeasureTurns}},
		{"", "project,model,day", Filter{}, TopOptions{}},
		{"", "project", Filter{}, TopOptions{Measure: "bogus"}},
	}
	for _, c := range cases {
		_, _, err := Top(db, c.q, c.by, c.f, c.opt)
		if _, ok := err.(ErrUsage); !ok {
			t.Errorf("top %q by %s %+v %+v: err = %v, want usage error", c.q, c.by, c.f, c.opt, err)
		}
	}
}
