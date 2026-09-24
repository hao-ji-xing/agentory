package query

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
)

// Bucket is one group of a Top aggregation.
type Bucket struct {
	Key   string    `json:"key"`
	Label string    `json:"label,omitempty"` // e.g. session title
	Count int       `json:"count"`
	Last  time.Time `json:"last"`
	// WithArgs counts invocations that carried arguments (skill and
	// command dimensions only).
	WithArgs *int `json:"with_args,omitempty"`
}

// TopOptions refines a Top aggregation.
type TopOptions struct {
	Key string // only this bucket key
}

// TopResult is the outcome of a Top aggregation.
type TopResult struct {
	By      string   `json:"by"`
	Total   int      `json:"total"`   // matching messages that have a key
	Groups  int      `json:"groups"`  // distinct keys before the limit
	Buckets []Bucket `json:"buckets"` // most frequent first (days: oldest first)
}

// dimension describes how to group messages.
type dimension struct {
	expr  string       // SQL expression yielding the bucket key
	label string       // optional SQL expression for a display label
	kinds []model.Kind // kinds searched unless the filter names some
	tool  string       // implied tool filter
	byKey bool         // order buckets by key instead of count
	// args, when set, is an SQL expression for the invocation arguments.
	args string
}

var inputKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TopDimensions lists the accepted values of --by, for help and errors.
var TopDimensions = []string{
	"skill", "command", "tool", "project", "branch", "session", "day", "kind", "role", "source", "input:<key>",
}

// inputValue extracts the value of "key=" from tool_use text rendered as
// key=value lines. key must match inputKey (it is inlined as a literal).
func inputValue(key string) string {
	t := `(char(10) || m.text || char(10))`
	pat := `char(10) || '` + key + `='`
	start := fmt.Sprintf(`instr(%s, %s)`, t, pat)
	rest := fmt.Sprintf(`substr(%s, %s + %d)`, t, start, len(key)+2)
	return fmt.Sprintf(`CASE WHEN %s > 0 THEN substr(%s, 1, instr(%s, char(10)) - 1) END`, start, rest, rest)
}

func lookupDimension(by string) (dimension, error) {
	toolUse := []model.Kind{model.KindToolUse}
	switch by {
	case "skill":
		return dimension{expr: inputValue("skill"), kinds: toolUse, tool: "Skill",
			args: inputValue("args")}, nil
	case "command":
		return dimension{
			expr:  `CASE WHEN instr(m.text, ' ') > 0 THEN substr(m.text, 1, instr(m.text, ' ') - 1) ELSE m.text END`,
			kinds: []model.Kind{model.KindCommand},
			args:  `CASE WHEN instr(m.text, ' ') > 0 THEN trim(substr(m.text, instr(m.text, ' ') + 1)) END`,
		}, nil
	case "tool":
		return dimension{expr: `m.tool`, kinds: toolUse}, nil
	case "project":
		// Last path element of cwd, falling back to the project key.
		return dimension{expr: `COALESCE(NULLIF(replace(m.cwd, rtrim(m.cwd, replace(m.cwd, '/', '')), ''), ''), s.project)`}, nil
	case "branch":
		return dimension{expr: `m.branch`}, nil
	case "session":
		return dimension{expr: `m.session_id`, label: `s.title`}, nil
	case "day":
		return dimension{expr: `date(m.ts / 1000, 'unixepoch', 'localtime')`, byKey: true}, nil
	case "kind", "role", "source":
		return dimension{expr: `m.` + by}, nil
	}
	if key, ok := strings.CutPrefix(by, "input:"); ok && inputKey.MatchString(key) {
		return dimension{expr: inputValue(key), kinds: []model.Kind{model.KindToolUse}}, nil
	}
	return dimension{}, fmt.Errorf("cannot group by %q (use one of %s)", by, strings.Join(TopDimensions, ", "))
}

// Top counts the messages matching q and f, grouped by the given
// dimension. f.Limit caps the number of buckets returned (default 20).
func Top(db *index.DB, q, by string, f Filter, opt TopOptions) (*TopResult, *Plan, error) {
	d, err := lookupDimension(by)
	if err != nil {
		return nil, nil, err
	}
	if len(f.Kinds) == 0 {
		f.Kinds = d.kinds
	}
	if f.Tool == "" {
		f.Tool = d.tool
	}
	p, w := plan(q, f)
	if opt.Key != "" {
		w.add("("+d.expr+") = ?", opt.Key)
	}
	label := `''`
	if d.label != "" {
		label = `COALESCE(max(` + d.label + `), '')`
	}
	withArgs := `-1`
	if d.args != "" {
		withArgs = `sum(CASE WHEN COALESCE(` + d.args + `, '') <> '' THEN 1 ELSE 0 END)`
	}
	p.SQL = "SELECT " + d.expr + " AS k, count(*) AS n, max(m.ts), " + label + ", " + withArgs +
		" FROM msgs m LEFT JOIN sessions s ON s.id = m.session_id" + w.sql() +
		" GROUP BY k HAVING k IS NOT NULL AND k <> ''"
	p.Args = w.args
	rows, err := db.Query(p.SQL, p.Args...)
	if err != nil {
		return nil, p, err
	}
	defer rows.Close()
	res := &TopResult{By: by, Buckets: []Bucket{}}
	for rows.Next() {
		var b Bucket
		var last int64
		var n int
		if err := rows.Scan(&b.Key, &b.Count, &last, &b.Label, &n); err != nil {
			return nil, p, err
		}
		if n >= 0 {
			b.WithArgs = &n
		}
		b.Last = time.UnixMilli(last)
		res.Buckets = append(res.Buckets, b)
		res.Total += b.Count
	}
	if err := rows.Err(); err != nil {
		return nil, p, err
	}
	sort.SliceStable(res.Buckets, func(i, j int) bool {
		a, b := res.Buckets[i], res.Buckets[j]
		if d.byKey {
			return a.Key < b.Key
		}
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Key < b.Key
	})
	res.Groups = len(res.Buckets)
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(res.Buckets) > limit {
		if d.byKey {
			res.Buckets = res.Buckets[len(res.Buckets)-limit:] // most recent days
		} else {
			res.Buckets = res.Buckets[:limit]
		}
	}
	return res, p, nil
}
