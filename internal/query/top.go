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

// Measures a Top aggregation can compute. Each reads its own fact table.
const (
	MeasureCount  = "count"  // messages (or invocations for skill/command dimensions)
	MeasureTokens = "tokens" // API requests, deduplicated by request id
	MeasureTurns  = "turns"  // completed agent turns and their duration
	MeasureCost   = "cost"   // sessions' self-reported cost
)

// Measures lists the accepted values of --measure.
var Measures = []string{MeasureCount, MeasureTokens, MeasureTurns, MeasureCost}

// Tokens sums the token usage of API requests.
type Tokens struct {
	Requests     int     `json:"requests"`
	Input        int64   `json:"input"` // uncached
	Output       int64   `json:"output"`
	CacheRead    int64   `json:"cache_read"`
	CacheWrite5m int64   `json:"cache_write_5m"`
	CacheWrite1h int64   `json:"cache_write_1h"`
	Thinking     int64   `json:"thinking"`
	HitRate      float64 `json:"cache_hit_rate"` // % of input read from cache
}

// Total is every input and output token.
func (t *Tokens) Total() int64 {
	return t.Input + t.CacheRead + t.CacheWrite5m + t.CacheWrite1h + t.Output
}

func (t *Tokens) add(o *Tokens) {
	t.Input += o.Input
	t.Output += o.Output
	t.CacheRead += o.CacheRead
	t.CacheWrite5m += o.CacheWrite5m
	t.CacheWrite1h += o.CacheWrite1h
	t.Thinking += o.Thinking
}

func (t *Tokens) finish() {
	if in := t.Input + t.CacheRead + t.CacheWrite5m + t.CacheWrite1h; in > 0 {
		t.HitRate = float64(int64(float64(t.CacheRead)/float64(in)*1000+0.5)) / 10
	}
}

// Turns sums completed agent turns.
type Turns struct {
	Turns      int   `json:"turns"`
	DurationMs int64 `json:"duration_ms"`
	AvgMs      int64 `json:"avg_ms"`
}

// Cost sums sessions' self-reported cost (API list prices as computed by the
// agent; not necessarily what a subscription is billed).
type Cost struct {
	Sessions     int     `json:"sessions"`
	USD          float64 `json:"usd"`
	LinesAdded   int64   `json:"lines_added"`
	LinesRemoved int64   `json:"lines_removed"`
}

// Bucket is one group of a Top aggregation.
type Bucket struct {
	Key   string    `json:"key"`            // keys joined by " / "
	Keys  []string  `json:"keys,omitempty"` // one per dimension, when several
	Label string    `json:"label,omitempty"`
	Count int       `json:"count"` // rows of the fact table
	Last  time.Time `json:"last"`
	// WithArgs counts invocations that carried arguments (invocation
	// dimensions only).
	WithArgs *int    `json:"with_args,omitempty"`
	Tokens   *Tokens `json:"tokens,omitempty"`
	Turns    *Turns  `json:"turns,omitempty"`
	Cost     *Cost   `json:"cost,omitempty"`
}

// TopResult is the outcome of a Top aggregation.
type TopResult struct {
	By      string   `json:"by"`
	Measure string   `json:"measure"`
	Groups  int      `json:"groups"`  // distinct keys before the limit
	Total   int      `json:"total"`   // fact rows in all groups
	All     Bucket   `json:"all"`     // sums over all groups
	Buckets []Bucket `json:"buckets"` // largest first (time dimensions: oldest first)
}

// TopOptions refines a Top aggregation.
type TopOptions struct {
	Key     string // only this value of the first dimension
	Measure string // default MeasureCount
}

type fact int

const (
	factMsgs fact = 1 << iota
	factRequests
	factTurns
	factSessions
	factAll = factMsgs | factRequests | factTurns | factSessions
)

var measureFact = map[string]fact{
	MeasureCount: factMsgs, MeasureTokens: factRequests, MeasureTurns: factTurns, MeasureCost: factSessions,
}

// dimension describes how to group rows of a fact table aliased m (joined
// to sessions as s).
type dimension struct {
	name     string
	expr     string       // bucket key
	cond     string       // extra WHERE condition
	label    string       // optional display label expression
	kinds    []model.Kind // kinds searched unless the filter names some
	facts    fact         // fact tables this dimension applies to
	byKey    bool         // order buckets by key instead of size
	args     string       // invocation arguments, for with_args
	subagent bool         // implies --include-subagent
}

var inputKey = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// TopDimensions lists the accepted values of --by, for help and errors.
var TopDimensions = []string{
	"skill", "command", "subagent_type", "name", "actor", "tool", "error", "file", "input:<key>",
	"project", "branch", "session", "model", "agent", "kind", "role", "source",
	"day", "week", "month", "hour", "weekday",
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

// localTime formats m.ts in the local time zone. SQLite's 'localtime'
// modifier is ~30x slower than arithmetic, so the zone's UTC offsets are
// computed in Go and applied per period.
func localTime(format string) string {
	return `strftime('` + format + `', m.ts / 1000 + ` + zoneOffsetExpr(time.Local, time.Now()) + `, 'unixepoch')`
}

// zoneOffsetExpr returns an SQL expression for the UTC offset in seconds
// of loc at m.ts, covering 2000 until a year after now. Zones without
// transitions yield a constant.
func zoneOffsetExpr(loc *time.Location, now time.Time) string {
	type period struct {
		startMs int64
		offset  int
	}
	var ps []period
	t := time.Date(2000, 1, 1, 0, 0, 0, 0, loc)
	end := now.AddDate(1, 0, 0)
	for t.Before(end) {
		_, off := t.Zone()
		if len(ps) == 0 || ps[len(ps)-1].offset != off {
			ps = append(ps, period{t.UnixMilli(), off})
		}
		_, next := t.ZoneBounds()
		if next.IsZero() || !next.After(t) {
			break
		}
		t = next
	}
	if len(ps) == 1 {
		return fmt.Sprint(ps[0].offset)
	}
	var sb strings.Builder
	sb.WriteString("(CASE")
	for i := len(ps) - 1; i > 0; i-- {
		fmt.Fprintf(&sb, " WHEN m.ts >= %d THEN %d", ps[i].startMs, ps[i].offset)
	}
	fmt.Fprintf(&sb, " ELSE %d END)", ps[0].offset)
	return sb.String()
}

func lookupDimension(by string) (dimension, error) {
	toolUse := []model.Kind{model.KindToolUse}
	inv := func(cond string) dimension {
		return dimension{expr: `m.inv_name`, cond: cond, kinds: model.AllKinds, facts: factMsgs, args: `m.inv_args`}
	}
	d := dimension{facts: factAll}
	switch by {
	case "skill":
		d = inv(`m.inv_kind = 'skill'`)
	case "command":
		d = inv(`m.inv_kind = 'command'`)
		d.expr = `'/' || m.inv_name`
	case "subagent_type":
		d = inv(`m.inv_kind = 'subagent'`)
	case "name":
		d = inv(`m.inv_kind <> ''`)
	case "actor":
		d = inv(`m.inv_kind <> ''`)
		d.expr = `CASE m.inv_kind WHEN 'command' THEN 'user' ELSE 'agent' END`
	case "tool":
		d = dimension{expr: `m.tool`, kinds: toolUse, facts: factMsgs}
	case "file":
		d = dimension{expr: `m.file_path`, kinds: toolUse, facts: factMsgs}
	case "error":
		result := `EXISTS (SELECT 1 FROM msgs r WHERE r.tool_use_id = m.tool_use_id AND r.tool_use_id <> '' AND r.kind = 'tool_result'`
		d = dimension{expr: `CASE WHEN ` + result + ` AND r.is_error) THEN 'error' WHEN ` + result +
			`) THEN 'ok' ELSE 'no result' END`, cond: `m.tool_use_id <> ''`, kinds: toolUse, facts: factMsgs}
	case "kind", "role":
		d = dimension{expr: `m.` + by, facts: factMsgs}
	case "model":
		d = dimension{expr: `m.model`, facts: factMsgs | factRequests}
	case "agent":
		d = dimension{expr: `CASE WHEN m.agent_id = '' THEN 'main' ELSE 'subagent' END`,
			facts: factMsgs | factRequests | factTurns, subagent: true}
	case "project":
		// Last path element of cwd, falling back to the project key.
		d.expr = `COALESCE(NULLIF(replace(m.cwd, rtrim(m.cwd, replace(m.cwd, '/', '')), ''), ''), s.project)`
	case "branch":
		d.expr = `m.branch`
	case "session":
		d.expr, d.label = `m.session_id`, `s.title`
	case "source":
		d.expr = `m.source`
	case "day":
		d.expr, d.byKey = localTime(`%Y-%m-%d`), true
	case "week":
		d.expr, d.byKey = localTime(`%Y-W%W`), true
	case "month":
		d.expr, d.byKey = localTime(`%Y-%m`), true
	case "hour":
		d.expr, d.byKey = localTime(`%H`), true
	case "weekday":
		n := `((CAST(` + localTime(`%w`) + ` AS INTEGER) + 6) % 7)` // Monday = 0
		d.expr, d.byKey = `(`+n+` + 1) || ' ' || substr('MonTueWedThuFriSatSun', `+n+` * 3 + 1, 3)`, true
	default:
		key, ok := strings.CutPrefix(by, "input:")
		if !ok || !inputKey.MatchString(key) {
			return d, fmt.Errorf("cannot group by %q (use one of %s)", by, strings.Join(TopDimensions, ", "))
		}
		d = dimension{expr: inputValue(key), kinds: toolUse, facts: factMsgs}
	}
	d.name = by
	return d, nil
}

// factTable returns the FROM clause of a measure, aliased m and joined to
// sessions as s. Every fact table exposes session_id, agent_id, ts, cwd,
// branch and source, which the common filters and dimensions use.
func factTable(measure string) string {
	var t string
	switch measure {
	case MeasureTokens:
		t = "requests m"
	case MeasureTurns:
		t = "turns m"
	case MeasureCost:
		t = `(SELECT id AS session_id, '' AS agent_id, started_at AS ts, cwd, branch, source,
			cost_usd, lines_added, lines_removed FROM sessions WHERE cost_usd > 0) m`
	default:
		t = "msgs m"
	}
	return " FROM " + t + " LEFT JOIN sessions s ON s.id = m.session_id"
}

func measureCols(measure string) string {
	switch measure {
	case MeasureTokens:
		return `sum(m.tok_in), sum(m.tok_out), sum(m.cache_read), sum(m.cache_w5m), sum(m.cache_w1h), sum(m.tok_think)`
	case MeasureTurns:
		return `sum(m.duration_ms)`
	case MeasureCost:
		return `sum(m.cost_usd), sum(m.lines_added), sum(m.lines_removed)`
	}
	return `0`
}

// ErrUsage marks an invalid combination of arguments.
type ErrUsage struct{ Msg string }

func (e ErrUsage) Error() string { return e.Msg }

// Top aggregates fact rows matching q and f by one or two comma-separated
// dimensions. f.Limit caps the number of buckets returned (default 20).
func Top(db *index.DB, q, by string, f Filter, opt TopOptions) (*TopResult, *Plan, error) {
	measure := opt.Measure
	if measure == "" {
		measure = MeasureCount
	}
	ft, ok := measureFact[measure]
	if !ok {
		return nil, nil, ErrUsage{fmt.Sprintf("unknown measure %q (use one of %s)", measure, strings.Join(Measures, ", "))}
	}
	names := strings.Split(by, ",")
	if len(names) > 2 {
		return nil, nil, ErrUsage{"at most two dimensions can be combined"}
	}
	var dims []dimension
	for _, n := range names {
		d, err := lookupDimension(strings.TrimSpace(n))
		if err != nil {
			return nil, nil, ErrUsage{err.Error()}
		}
		if d.facts&ft == 0 {
			return nil, nil, ErrUsage{fmt.Sprintf("--by %s does not apply to --measure %s", d.name, measure)}
		}
		dims = append(dims, d)
		f.IncludeSubagent = f.IncludeSubagent || d.subagent
	}

	var p *Plan
	var w *where
	if measure == MeasureCount {
		if len(f.Kinds) == 0 {
			for _, d := range dims {
				if d.kinds != nil {
					f.Kinds = d.kinds
					break
				}
			}
		}
		p, w = plan(q, f)
	} else {
		if strings.TrimSpace(q) != "" || len(f.Kinds) > 0 || f.Role != "" || f.Tool != "" {
			return nil, nil, ErrUsage{"a query, --kind, --role and --tool only apply to --measure count"}
		}
		p, w = &Plan{Mode: "scan"}, &where{}
		f.applyCommon(w)
	}

	label, withArgs := `''`, `-1`
	var sel, keys, having []string
	for i, d := range dims {
		k := fmt.Sprintf("k%d", i)
		keys = append(keys, k)
		sel = append(sel, d.expr+" AS "+k)
		having = append(having, k+" IS NOT NULL AND "+k+" <> ''")
		if d.cond != "" {
			w.add(d.cond)
		}
		if d.label != "" {
			label = `COALESCE(max(` + d.label + `), '')`
		}
		if d.args != "" && withArgs == `-1` {
			withArgs = `sum(` + d.args + ` <> '')`
		}
	}
	if opt.Key != "" {
		key := opt.Key
		if dims[0].name == "command" && !strings.HasPrefix(key, "/") {
			key = "/" + key
		}
		w.add("("+dims[0].expr+") = ?", key)
	}
	p.SQL = "SELECT " + strings.Join(sel, ", ") + ", count(*), max(m.ts), " + label + ", " + withArgs + ", " +
		measureCols(measure) + factTable(measure) + w.sql() +
		" GROUP BY " + strings.Join(keys, ", ") + " HAVING " + strings.Join(having, " AND ")
	p.Args = w.args

	rows, err := db.Query(p.SQL, p.Args...)
	if err != nil {
		return nil, p, err
	}
	defer rows.Close()
	res := &TopResult{By: by, Measure: measure, Buckets: []Bucket{}}
	res.All.Key = "(all)"
	for rows.Next() {
		b, err := scanBucket(rows, len(dims), measure)
		if err != nil {
			return nil, p, err
		}
		res.Buckets = append(res.Buckets, b)
		accumulate(&res.All, &b)
	}
	if err := rows.Err(); err != nil {
		return nil, p, err
	}
	finishBucket(&res.All)
	res.Total = res.All.Count
	res.Groups = len(res.Buckets)
	byKey := dims[0].byKey
	sort.SliceStable(res.Buckets, func(i, j int) bool {
		a, b := &res.Buckets[i], &res.Buckets[j]
		if byKey {
			return a.Key < b.Key
		}
		if sa, sb := size(a), size(b); sa != sb {
			return sa > sb
		}
		return a.Key < b.Key
	})
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	if len(res.Buckets) > limit {
		if byKey {
			res.Buckets = res.Buckets[len(res.Buckets)-limit:] // most recent periods
		} else {
			res.Buckets = res.Buckets[:limit]
		}
	}
	return res, p, nil
}

func scanBucket(rows interface{ Scan(...any) error }, nDims int, measure string) (Bucket, error) {
	var b Bucket
	keys := make([]string, nDims)
	var last int64
	var withArgs int
	var dest []any
	for i := range keys {
		dest = append(dest, &keys[i])
	}
	dest = append(dest, &b.Count, &last, &b.Label, &withArgs)
	switch measure {
	case MeasureTokens:
		t := &Tokens{}
		b.Tokens = t
		dest = append(dest, &t.Input, &t.Output, &t.CacheRead, &t.CacheWrite5m, &t.CacheWrite1h, &t.Thinking)
	case MeasureTurns:
		b.Turns = &Turns{}
		dest = append(dest, &b.Turns.DurationMs)
	case MeasureCost:
		b.Cost = &Cost{}
		dest = append(dest, &b.Cost.USD, &b.Cost.LinesAdded, &b.Cost.LinesRemoved)
	default:
		var zero int
		dest = append(dest, &zero)
	}
	if err := rows.Scan(dest...); err != nil {
		return b, err
	}
	b.Key = strings.Join(keys, " / ")
	if nDims > 1 {
		b.Keys = keys
	}
	b.Last = time.UnixMilli(last)
	if withArgs >= 0 {
		b.WithArgs = &withArgs
	}
	finishBucket(&b)
	return b, nil
}

// finishBucket derives counts and ratios from the sums.
func finishBucket(b *Bucket) {
	switch {
	case b.Tokens != nil:
		b.Tokens.Requests = b.Count
		b.Tokens.finish()
	case b.Turns != nil:
		b.Turns.Turns = b.Count
		if b.Count > 0 {
			b.Turns.AvgMs = b.Turns.DurationMs / int64(b.Count)
		}
	case b.Cost != nil:
		b.Cost.Sessions = b.Count
		b.Cost.USD = float64(int64(b.Cost.USD*100+0.5)) / 100
	}
}

func accumulate(all, b *Bucket) {
	all.Count += b.Count
	if b.Last.After(all.Last) {
		all.Last = b.Last
	}
	if b.WithArgs != nil {
		if all.WithArgs == nil {
			all.WithArgs = new(int)
		}
		*all.WithArgs += *b.WithArgs
	}
	switch {
	case b.Tokens != nil:
		if all.Tokens == nil {
			all.Tokens = &Tokens{}
		}
		all.Tokens.add(b.Tokens)
	case b.Turns != nil:
		if all.Turns == nil {
			all.Turns = &Turns{}
		}
		all.Turns.DurationMs += b.Turns.DurationMs
	case b.Cost != nil:
		if all.Cost == nil {
			all.Cost = &Cost{}
		}
		all.Cost.USD += b.Cost.USD
		all.Cost.LinesAdded += b.Cost.LinesAdded
		all.Cost.LinesRemoved += b.Cost.LinesRemoved
	}
}

// size orders buckets by the measure's main quantity.
func size(b *Bucket) float64 {
	switch {
	case b.Tokens != nil:
		return float64(b.Tokens.Total())
	case b.Turns != nil:
		return float64(b.Turns.DurationMs)
	case b.Cost != nil:
		return b.Cost.USD
	}
	return float64(b.Count)
}
