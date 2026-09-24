package query

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/claudecode"
)

// topFixture indexes synthetic sessions with skill calls, slash commands
// and sub-agent launches spread over three days and two projects.
func topFixture(t *testing.T) *index.DB {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	n := 0
	rec := func(sid, cwd string, day int, body string) string {
		n++
		ts := day0.AddDate(0, 0, day).Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
		return fmt.Sprintf(`{"uuid":"x%d","sessionId":%q,"timestamp":%q,"cwd":%q,"gitBranch":"main",%s}`, n, sid, ts, cwd, body) + "\n"
	}
	skill := func(name, args string) string {
		return fmt.Sprintf(`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Skill","input":{"skill":%q,"args":%q}}]}`, name, args)
	}
	task := func(kind string) string {
		return fmt.Sprintf(`"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Task","input":{"subagent_type":%q,"prompt":"look around"}}]}`, kind)
	}
	cmd := func(name, args string) string {
		return fmt.Sprintf(`"type":"user","message":{"role":"user","content":"<command-name>%s</command-name>\n<command-args>%s</command-args>"}`, name, args)
	}
	prompt := func(text string) string {
		return fmt.Sprintf(`"type":"user","message":{"role":"user","content":%q}`, text)
	}
	longArgs := strings.Repeat("very long arguments ", 300) // > truncation limit
	files := map[string]string{
		"-w-shop/a.jsonl": rec("a", "/w/shop", 0, prompt("release the invoice fix")) +
			rec("a", "/w/shop", 0, skill("commit", "fix invoice")) +
			rec("a", "/w/shop", 0, skill("commit", longArgs)) +
			rec("a", "/w/shop", 1, skill("review", "")) +
			rec("a", "/w/shop", 1, cmd("/deploy", "prod")) +
			rec("a", "/w/shop", 1, cmd("/deploy", "")) +
			rec("a", "/w/shop", 1, cmd("/clear", "")) +
			rec("a", "/w/shop", 1, task("Explore")),
		"-w-api/b.jsonl": rec("b", "/w/api", 2, skill("commit", "api change")) +
			rec("b", "/w/api", 2, task("Explore")) +
			rec("b", "/w/api", 2, task("Plan")) +
			// Another tool that happens to take a "skill" argument must not count.
			rec("b", "/w/api", 2, `"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t","name":"Lookup","input":{"skill":"decoy"}}]}`) +
			rec("b", "/w/api", 2, prompt("invoice totals for the api")),
	}
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(content), 0o644)
	}
	os.WriteFile(filepath.Join(root, "-w-shop", "a-title.jsonl"),
		[]byte(`{"type":"custom-title","sessionId":"a","customTitle":"Invoice release"}`+"\n"), 0o644)
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

// localDay is the local calendar date of fixture day d (days group by the
// local time zone).
func localDay(d int) string { return day0.AddDate(0, 0, d).Local().Format("2006-01-02") }

func top(t *testing.T, db *index.DB, q, by string, f Filter) *TopResult {
	t.Helper()
	res, _, err := Top(db, q, by, f, TopOptions{})
	if err != nil {
		t.Fatalf("top by %s: %v", by, err)
	}
	return res
}

func buckets(r *TopResult) string {
	var parts []string
	for _, b := range r.Buckets {
		parts = append(parts, fmt.Sprintf("%s=%d", b.Key, b.Count))
	}
	return strings.Join(parts, " ")
}

func TestTopDimensions(t *testing.T) {
	db := topFixture(t)
	cases := []struct {
		q, by string
		f     Filter
		want  string
	}{
		// The long-args call still counts: skill= survives truncation.
		{"", "skill", Filter{}, "commit=3 review=1"},
		{"", "command", Filter{}, "/deploy=2 /clear=1"},
		{"", "input:subagent_type", Filter{}, "Explore=2 Plan=1"},
		{"", "tool", Filter{}, "Skill=4 Task=3 Lookup=1"},
		{"", "project", Filter{Kinds: model.AllKinds}, "shop=8 api=5"},
		{"", "day", Filter{Kinds: model.AllKinds}, fmt.Sprintf("%s=3 %s=5 %s=5", localDay(0), localDay(1), localDay(2))},
		// Filters and query terms narrow the population.
		{"", "skill", Filter{Project: "api"}, "commit=1"},
		{"", "skill", Filter{Since: day0.AddDate(0, 0, 1)}, "commit=1 review=1"}, // ties by key
		{"invoice", "skill", Filter{}, "commit=1"},
		{"invoice", "project", Filter{}, "api=1 shop=1"},
		// Limit keeps the most frequent buckets.
		{"", "skill", Filter{Limit: 1}, "commit=3"},
	}
	for _, c := range cases {
		res := top(t, db, c.q, c.by, c.f)
		if got := buckets(res); got != c.want {
			t.Errorf("top %q by %s %+v = %s, want %s", c.q, c.by, c.f, got, c.want)
		}
	}
}

func TestTopTotalsAndLabels(t *testing.T) {
	db := topFixture(t)
	res := top(t, db, "", "skill", Filter{Limit: 1})
	if res.Total != 4 || res.Groups != 2 || len(res.Buckets) != 1 {
		t.Fatalf("total/groups must ignore the bucket limit: %+v", res)
	}
	if want := day0.AddDate(0, 0, 2); res.Buckets[0].Last.Before(want) {
		t.Fatalf("last use of commit = %v, want on %v", res.Buckets[0].Last, want)
	}
	res = top(t, db, "", "session", Filter{Kinds: model.AllKinds})
	if len(res.Buckets) != 2 || res.Buckets[0].Key != "a" || res.Buckets[0].Label != "Invoice release" {
		t.Fatalf("session buckets should carry the title: %+v", res.Buckets)
	}
}

func TestTopRejectsUnknownDimension(t *testing.T) {
	db := topFixture(t)
	for _, by := range []string{"bogus", "input:", "input:x'; DROP TABLE msgs; --"} {
		if _, _, err := Top(db, "", by, Filter{}, TopOptions{}); err == nil {
			t.Errorf("by %q should be rejected", by)
		}
	}
}

func TestTopArgsAndKey(t *testing.T) {
	db := topFixture(t)
	res, _, err := Top(db, "", "command", Filter{}, TopOptions{})
	if err != nil {
		t.Fatal(err)
	}
	withArgs := map[string]int{}
	for _, b := range res.Buckets {
		if b.WithArgs == nil {
			t.Fatalf("command buckets must report with_args: %+v", b)
		}
		withArgs[b.Key] = *b.WithArgs
	}
	if withArgs["/deploy"] != 1 || withArgs["/clear"] != 0 {
		t.Fatalf("with_args = %v, want /deploy=1 /clear=0", withArgs)
	}
	res, _, err = Top(db, "", "skill", Filter{}, TopOptions{Key: "commit"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Buckets) != 1 || res.Groups != 1 || res.Buckets[0].Key != "commit" || *res.Buckets[0].WithArgs != 3 {
		t.Fatalf("--key commit: %+v", res)
	}
	res, _, _ = Top(db, "", "project", Filter{Kinds: model.AllKinds}, TopOptions{})
	if res.Buckets[0].WithArgs != nil {
		t.Fatalf("with_args only for skill/command: %+v", res.Buckets[0])
	}
}

func TestZoneOffsetExprMatchesGo(t *testing.T) {
	db := topFixture(t)
	for _, name := range []string{"UTC", "Asia/Shanghai", "America/New_York", "Australia/Lord_Howe"} {
		loc, err := time.LoadLocation(name)
		if err != nil {
			t.Skipf("zoneinfo unavailable: %v", err)
		}
		expr := zoneOffsetExpr(loc, time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC))
		if name == "Asia/Shanghai" && expr != "28800" {
			t.Errorf("zone without transitions should be a constant, got %s", expr)
		}
		// Instants around DST changes and far apart.
		for _, ts := range []time.Time{
			time.Date(2026, 3, 8, 6, 59, 0, 0, time.UTC), time.Date(2026, 3, 8, 7, 1, 0, 0, time.UTC),
			time.Date(2026, 11, 1, 5, 59, 0, 0, time.UTC), time.Date(2026, 11, 1, 6, 1, 0, 0, time.UTC),
			time.Date(2026, 4, 5, 14, 30, 0, 0, time.UTC), time.Date(2003, 7, 1, 12, 0, 0, 0, time.UTC),
			time.Date(2026, 9, 24, 23, 30, 0, 0, time.UTC),
		} {
			var got string
			q := `SELECT strftime('%Y-%m-%d %H:%M', m.ts / 1000 + ` + expr + `, 'unixepoch') FROM (SELECT ? AS ts) m`
			if err := db.QueryRow(q, ts.UnixMilli()).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if want := ts.In(loc).Format("2006-01-02 15:04"); got != want {
				t.Errorf("%s at %v: SQL %s, Go %s", name, ts, got, want)
			}
		}
	}
}
