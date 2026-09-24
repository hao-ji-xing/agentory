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

func usageFixture(t *testing.T) *index.DB {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "projects")
	n := 0
	rec := func(cwd, body string) string {
		n++
		ts := day0.Add(time.Duration(n) * time.Minute).Format(time.RFC3339)
		return fmt.Sprintf(`{"uuid":"u%d","sessionId":"s","timestamp":%q,"cwd":%q,%s}`, n, ts, cwd, body) + "\n"
	}
	cmd := func(args string) string {
		return fmt.Sprintf(`"type":"user","message":{"role":"user","content":"<command-name>/ship</command-name>\n<command-args>%s</command-args>"}`, args)
	}
	prompt := func(s string) string { return fmt.Sprintf(`"type":"user","message":{"role":"user","content":%q}`, s) }
	content := rec("/w/shop", cmd("v1  to prod")) + // args are whitespace-normalized when grouped
		rec("/w/shop", prompt("looks good")) +
		rec("/w/shop", cmd("")) +
		rec("/w/shop", `"type":"user","message":{"role":"user","content":[{"type":"text","text":"[Request interrupted by user]"}]}`) +
		rec("/w/shop", prompt("stop, use v2")) +
		rec("/w/api", `"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"t9","name":"Skill","input":{"skill":"ship","args":"v1 to prod"}}]}`) +
		rec("/w/api", `"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t9","is_error":true,"content":"failed"}]}`) +
		rec("/w/api", cmd("v1 to prod"))
	p := filepath.Join(root, "-w", "s.jsonl")
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte(content), 0o644)
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

func TestUsage(t *testing.T) {
	db := usageFixture(t)
	res, err := Usage(db, "/ship", Filter{}, UsageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Name != "ship" || res.Total != 4 || res.ByActor["user"] != 3 || res.ByActor["agent"] != 1 ||
		res.WithArgs != 3 || res.Errors != 1 || res.Interrupted != 1 {
		t.Fatalf("summary: %+v", res)
	}
	if len(res.ArgGroups) != 2 || res.ArgGroups[0].Args != "v1 to prod" || res.ArgGroups[0].Count != 3 ||
		res.ArgGroups[1].Args != "" || res.Distinct != 2 {
		t.Fatalf("argument groups: %+v", res.ArgGroups)
	}
	if len(res.Projects) != 2 || res.Projects[0].Key != "api" || res.Projects[0].Count != 2 {
		t.Fatalf("projects: %+v", res.Projects)
	}
	// Recent is newest first: the last /ship has no following prompt; the
	// empty /ship was interrupted and followed by "stop, use v2"; the first
	// one was followed by "looks good".
	r := res.Recent
	if len(r) != 4 || r[0].Next != "" || r[1].Actor != "agent" || !r[1].Error {
		t.Fatalf("recent[0..1]: %+v", r[:2])
	}
	if !r[2].Interrupted || r[2].Next != "stop, use v2" || r[3].Interrupted || r[3].Next != "looks good" {
		t.Fatalf("recent[2..3]: %+v", r[2:])
	}

	res, _ = Usage(db, "ship", Filter{Project: "api"}, UsageOptions{Recent: 1, Groups: 1})
	if res.Total != 2 || len(res.Recent) != 1 || len(res.ArgGroups) != 1 {
		t.Fatalf("filters and limits: %+v", res)
	}
	res, _ = Usage(db, "shi", Filter{}, UsageOptions{})
	if res.Total != 0 || strings.Join(res.Suggestions, ",") != "ship" {
		t.Fatalf("suggestions for an unknown name: %+v", res.Suggestions)
	}
}

func TestRunSQLIsReadOnlyAndBounded(t *testing.T) {
	db := usageFixture(t)
	ctx := context.Background()
	res, err := RunSQL(ctx, db, `SELECT actor, name, count(*) AS n FROM invocations GROUP BY actor, name ORDER BY actor`, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Columns, ",") != "actor,name,n" || len(res.Rows) != 2 || res.Rows[0][0] != "agent" || res.Rows[1][2] != int64(3) {
		t.Fatalf("result: %+v", res)
	}
	res, _ = RunSQL(ctx, db, `SELECT id FROM msgs`, 2)
	if len(res.Rows) != 2 || !res.Truncated {
		t.Fatalf("row limit: %+v", res)
	}
	for _, q := range []string{`DELETE FROM msgs`, `UPDATE sessions SET title='x'`, `CREATE TABLE x(a)`, `DROP TABLE msgs`} {
		if _, err := RunSQL(ctx, db, q, 0); err == nil {
			t.Errorf("%s must be rejected", q)
		}
	}
	var n int
	db.QueryRow(`SELECT count(*) FROM msgs`).Scan(&n)
	if n == 0 {
		t.Fatal("data was modified")
	}
	// Writes work again for the indexer afterwards.
	if _, err := db.Exec(`INSERT INTO meta(key, value) VALUES('probe', '1')`); err != nil {
		t.Fatalf("query_only leaked: %v", err)
	}
	tctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	slow := `WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM c) SELECT count(*) FROM c`
	start := time.Now()
	if _, err := RunSQL(tctx, db, slow, 0); err == nil || time.Since(start) > 5*time.Second {
		t.Fatalf("timeout not enforced: err=%v after %v", err, time.Since(start))
	}
}
