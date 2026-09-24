package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/query"
)

const topDesc = `Aggregate the index by one or two dimensions (comma-separated, e.g.
--by project,model). Dimensions:

  skill          skills the agent invoked          (count)
  command        slash commands you typed          (count)
  subagent_type  sub-agents the agent started      (count)
  name, actor    any invocation / user vs agent    (count)
  tool, file     tool calls, files they touched    (count)
  error          tool call outcome: ok | error | no result (count)
  input:<key>    one tool input field              (count)
  kind, role                                        (count)
  model          (count, tokens)
  agent          main vs sub-agent                 (count, tokens, turns)
  project, branch, session, source, day, week, month, hour, weekday (all)

Measures (--measure):
  count   messages matching the query and filters (default); invocation
          dimensions also report how many uses carried arguments
  tokens  API requests: input, output, cache reads/writes and hit rate
  turns   completed agent turns and their duration
  cost    sessions' self-reported cost at API list prices

A query, -k, --role and --tool apply to count only. -n limits the number
of groups (default 20); "total" and "groups" in --json cover all of them.
--key keeps one value of the first dimension, e.g. --by command --key /deploy.`

func (a *app) top(args []string) error {
	var s searchFlags
	var key, measure string
	fs := newFlagSet("top")
	fs.str(&s.by, "b", "by", "", "<dims>", "one or two of: "+strings.Join(query.TopDimensions, " "))
	fs.str(&measure, "m", "measure", "count", "<measure>", strings.Join(query.Measures, " | "))
	fs.str(&key, "", "key", "", "<value>", "only this value of the first dimension")
	s.register(fs, true, false)
	pos, err := a.parseFlags(fs, args, "agentory top --by <dim>[,<dim>] [query] [flags]", topDesc)
	if err != nil {
		return err
	}
	if s.by == "" {
		return errUsage{"missing --by (" + strings.Join(query.TopDimensions, ", ") + ")"}
	}
	f, err := a.filter(&s)
	if err != nil {
		return err
	}
	db, err := a.openIndex(s.noSync, f.Sources)
	if err != nil {
		return err
	}
	defer db.Close()
	res, plan, err := query.Top(db, strings.Join(pos, " "), s.by, f, query.TopOptions{Key: key, Measure: measure})
	if err != nil {
		var u query.ErrUsage
		if errors.As(err, &u) {
			return errUsage{u.Msg}
		}
		return err
	}
	if s.json {
		out := struct {
			*query.TopResult
			Plan *query.Plan `json:"plan,omitempty"`
		}{TopResult: res}
		if s.explain {
			out.Plan = plan
		}
		return a.writeJSON(out)
	}
	r := a.renderer(s.color, nil)
	if s.explain {
		r.explain(plan, 0)
	}
	r.top(res)
	return nil
}

const usageDesc = `Show how one slash command, skill or sub-agent type is used, across what
you typed and what the agent invoked: how often and where, which
arguments followed the name (grouped, most frequent first), whether calls
failed or were interrupted, and what you said next. A leading "/" is
optional. Sub-agent sessions are excluded unless --include-subagent.`

func (a *app) usage(args []string) error {
	var s searchFlags
	var groups, recent int
	fs := newFlagSet("usage")
	fs.str(&s.project, "p", "project", "", "<substr>", "filter by project name or cwd substring")
	fs.str(&s.since, "s", "since", "", "<when>", "only uses after (7d, 2026-09-01, …)")
	fs.str(&s.until, "u", "until", "", "<when>", "only uses before")
	fs.str(&s.branch, "", "branch", "", "<name>", "filter by git branch")
	fs.str(&s.sources, "", "source", "", "<names>", "comma list of sources")
	fs.int(&groups, "n", "groups", 20, "<n>", "argument groups to show")
	fs.int(&recent, "r", "recent", 10, "<n>", "recent uses to show")
	fs.bool(&s.subagent, "", "include-subagent", "include uses inside sub-agents")
	fs.bool(&s.json, "", "json", "machine-readable output")
	fs.bool(&s.noSync, "", "no-sync", "skip the incremental index update")
	fs.str(&s.color, "", "color", "auto", "<when>", "auto | always | never")
	pos, err := a.parseFlags(fs, args, "agentory usage <name> [flags]", usageDesc)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage{"expected exactly one command, skill or sub-agent name"}
	}
	f, err := a.filter(&s)
	if err != nil {
		return err
	}
	db, err := a.openIndex(s.noSync, f.Sources)
	if err != nil {
		return err
	}
	defer db.Close()
	res, err := query.Usage(db, pos[0], f, query.UsageOptions{Groups: groups, Recent: recent})
	if err != nil {
		return err
	}
	if s.json {
		return a.writeJSON(res)
	}
	a.renderer(s.color, nil).usage(res)
	return nil
}

const sqlDesc = `Run one read-only SQL statement against the index (writes are rejected).
See 'agentory schema' for tables and columns. Pass "-" to read the query
from standard input.`

func (a *app) sql(args []string) error {
	var jsonOut, noSync bool
	var limit int
	var timeout time.Duration
	fs := newFlagSet("sql")
	fs.int(&limit, "n", "limit", 1000, "<n>", "maximum rows (0 = no limit)")
	fs.DurationVar(&timeout, "timeout", 30*time.Second, "")
	fs.usage = append(fs.usage, fmt.Sprintf("  %-28s %s", "    --timeout <duration>", "abort after this long (default 30s)"))
	fs.bool(&jsonOut, "", "json", "output an array of objects")
	fs.bool(&noSync, "", "no-sync", "skip the incremental index update")
	pos, err := a.parseFlags(fs, args, `agentory sql "<SELECT …>" [flags]`, sqlDesc)
	if err != nil {
		return err
	}
	q := strings.Join(pos, " ")
	if q == "-" {
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return err
		}
		q = string(b)
	}
	if strings.TrimSpace(q) == "" {
		return errUsage{"missing query"}
	}
	db, err := a.openIndex(noSync, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(a.ctx, timeout)
	defer cancel()
	res, err := query.RunSQL(ctx, db, q, limit)
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("query aborted after %s (raise --timeout)", timeout)
		}
		return err
	}
	if res.Truncated {
		fmt.Fprintf(a.stderr, "showing the first %d rows (use -n to change)\n", limit)
	}
	if jsonOut {
		out := make([]map[string]any, 0, len(res.Rows))
		for _, row := range res.Rows {
			m := make(map[string]any, len(row))
			for i, c := range res.Columns {
				m[c] = row[i]
			}
			out = append(out, m)
		}
		return a.writeJSON(out)
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, strings.Join(res.Columns, "\t"))
	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v == nil {
				cells[i] = "NULL"
			} else {
				cells[i] = truncRunes(query.Flatten(fmt.Sprint(v)), 120)
			}
		}
		fmt.Fprintln(tw, strings.Join(cells, "\t"))
	}
	return tw.Flush()
}

func (a *app) schema(args []string) error {
	fs := newFlagSet("schema")
	if _, err := a.parseFlags(fs, args, "agentory schema", "Describe the index tables for use with 'agentory sql'."); err != nil {
		return err
	}
	_, err := io.WriteString(a.stdout, index.SchemaDoc)
	return err
}

// Rendering.

func plural(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func humanCount(n int64) string {
	switch {
	case n >= 1e9:
		return fmt.Sprintf("%.1fB", float64(n)/1e9)
	case n >= 1e6:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1e4:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	}
	return fmt.Sprint(n)
}

func humanDuration(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}

func (r *renderer) top(res *query.TopResult) {
	width := 8
	for _, b := range res.Buckets {
		width = max(width, len([]rune(b.Key)))
	}
	width = min(width, 48)
	switch res.Measure {
	case query.MeasureTokens:
		fmt.Fprintf(r.w, "%-*s  %7s  %8s  %8s  %8s  %8s  %6s\n", width, "", "requests", "output", "input", "cache rd", "cache wr", "hit")
	case query.MeasureTurns:
		fmt.Fprintf(r.w, "%-*s  %6s  %9s  %8s\n", width, "", "turns", "total", "avg")
	case query.MeasureCost:
		fmt.Fprintf(r.w, "%-*s  %10s  %8s  %s\n", width, "", "usd", "sessions", "lines")
	}
	for _, b := range append(res.Buckets, res.All) {
		key := r.paint(ansiCyan, padRunes(truncRunes(b.Key, width), width))
		if b.Key == "(all)" {
			if res.Measure == query.MeasureCount {
				break
			}
			key = r.paint(ansiBold, padRunes(b.Key, width))
		}
		label := ""
		if b.Label != "" {
			label = "  " + r.paint(ansiDim, b.Label)
		}
		switch res.Measure {
		case query.MeasureTokens:
			t := b.Tokens
			fmt.Fprintf(r.w, "%s  %7d  %8s  %8s  %8s  %8s  %5.1f%%%s\n", key, t.Requests, humanCount(t.Output), humanCount(t.Input),
				humanCount(t.CacheRead), humanCount(t.CacheWrite5m+t.CacheWrite1h), t.HitRate, label)
		case query.MeasureTurns:
			fmt.Fprintf(r.w, "%s  %6d  %9s  %8s%s\n", key, b.Turns.Turns, humanDuration(b.Turns.DurationMs), humanDuration(b.Turns.AvgMs), label)
		case query.MeasureCost:
			fmt.Fprintf(r.w, "%s  %10s  %8d  +%d/-%d%s\n", key, fmt.Sprintf("$%.2f", b.Cost.USD), b.Cost.Sessions,
				b.Cost.LinesAdded, b.Cost.LinesRemoved, label)
		default:
			args := ""
			if b.WithArgs != nil {
				args = fmt.Sprintf("  with args %d/%d", *b.WithArgs, b.Count)
			}
			fmt.Fprintf(r.w, "%6d  %s  last %s%s%s\n", b.Count, key, localTime(b.Last), args, label)
		}
	}
	more := ""
	if res.Groups > len(res.Buckets) {
		more = fmt.Sprintf(", showing %d (use -n for more)", len(res.Buckets))
	}
	unit := map[string]string{query.MeasureTokens: "requests", query.MeasureTurns: "turns", query.MeasureCost: "sessions"}[res.Measure]
	if unit == "" {
		unit = "messages"
	}
	fmt.Fprintf(r.w, "%s in %s by %s%s\n", plural(res.Total, strings.TrimSuffix(unit, "s")), plural(res.Groups, "group"), res.By, more)
}

func (r *renderer) usage(u *query.UsageResult) {
	if u.Total == 0 {
		fmt.Fprintf(r.w, "no uses of %q", u.Name)
		if len(u.Suggestions) > 0 {
			fmt.Fprintf(r.w, "; similar: %s", strings.Join(u.Suggestions, ", "))
		}
		fmt.Fprintln(r.w)
		return
	}
	fmt.Fprintf(r.w, "%s — %s (%d typed by you, %d by the agent), %d with arguments, %s, %d interrupted\n",
		r.paint(ansiBold, u.Name), plural(u.Total, "use"), u.ByActor["user"], u.ByActor["agent"], u.WithArgs,
		plural(u.Errors, "error"), u.Interrupted)
	fmt.Fprintf(r.w, "first %s  last %s\n", localTime(u.First), localTime(u.Last))
	var ps []string
	for i, p := range u.Projects {
		if i == 8 {
			ps = append(ps, "…")
			break
		}
		ps = append(ps, fmt.Sprintf("%s×%d", p.Key, p.Count))
	}
	fmt.Fprintf(r.w, "projects: %s\n\narguments (%d distinct):\n", strings.Join(ps, "  "), u.Distinct)
	for _, g := range u.ArgGroups {
		args := g.Args
		if args == "" {
			args = r.paint(ansiDim, "(no arguments)")
		} else {
			args = truncRunes(args, 90)
		}
		fmt.Fprintf(r.w, "  %5d  %s  %s\n", g.Count, r.paint(ansiDim, g.Last.Local().Format("01-02 15:04")), args)
	}
	fmt.Fprintln(r.w, "\nrecent:")
	for _, in := range u.Recent {
		flags := ""
		if in.Error {
			flags += " " + r.paint(ansiHit, "[error]")
		}
		if in.Interrupted {
			flags += " " + r.paint(ansiYellow, "[interrupted]")
		}
		args := truncRunes(query.Flatten(in.Args), 80)
		if args == "" {
			args = r.paint(ansiDim, "(no arguments)")
		}
		fmt.Fprintf(r.w, "  %s  %s  %-5s  %s  %s%s\n", r.paint(ansiDim, in.Time.Local().Format("01-02 15:04")),
			r.paint(ansiYellow, fmt.Sprintf("#%d", in.ID)), in.Actor, r.paint(ansiCyan, in.Project), args, flags)
		if in.Next != "" {
			label := "→ next: "
			if in.NextSource == "queued" {
				label = "→ next (typed while it ran): "
			}
			fmt.Fprintf(r.w, "      %s\n", r.paint(ansiDim, label+truncRunes(in.Next, 100)))
		}
	}
}
