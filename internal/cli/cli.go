// Package cli implements the agentory command line.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/query"
	"github.com/hao-ji-xing/agentory/internal/source"
)

// Version is set at build time via -ldflags "-X ...cli.Version=v1.2.3".
var Version = "dev"

// app carries per-invocation state.
type app struct {
	stdout, stderr io.Writer
	now            time.Time
	ctx            context.Context
}

// errUsage marks errors that should be followed by a usage hint.
type errUsage struct{ msg string }

func (e errUsage) Error() string { return e.msg }

type command struct {
	name    string
	summary string
	run     func(a *app, args []string) error
}

func commands() []command {
	return []command{
		{"search", "Search messages (default command)", (*app).search},
		{"show", "Show a message with context, or a whole session", (*app).show},
		{"sessions", "List sessions", (*app).sessions},
		{"projects", "List projects", (*app).projects},
		{"index", "Build or update the index", (*app).index},
		{"stats", "Show index statistics", (*app).stats},
		{"doctor", "Check environment and index health", (*app).doctor},
		{"watch", "Keep the index updated as files change", (*app).watch},
		{"version", "Print version", (*app).version},
		{"help", "Show help", (*app).help},
	}
}

// Main is the process entry point.
func Main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(Run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

// Run executes the command line and returns the exit code.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	a := &app{stdout: stdout, stderr: stderr, now: time.Now(), ctx: ctx}
	if len(args) == 0 {
		a.help(nil)
		return 2
	}
	name, rest := args[0], args[1:]
	switch name {
	case "-h", "--help":
		name = "help"
	case "-v", "--version":
		name = "version"
	}
	cmd := command{name: "search", run: (*app).search}
	matched := false
	for _, c := range commands() {
		if c.name == name {
			cmd, matched = c, true
			break
		}
	}
	if !matched {
		rest = args // bare query: agentory <query>
	}
	err := cmd.run(a, rest)
	switch {
	case err == nil:
		return 0
	case errors.Is(err, flag.ErrHelp):
		return 0
	case errors.As(err, new(errUsage)):
		fmt.Fprintf(stderr, "agentory %s: %v\nRun 'agentory help %s' for usage.\n", cmd.name, err, cmd.name)
		return 2
	default:
		fmt.Fprintf(stderr, "agentory: %v\n", err)
		return 1
	}
}

func (a *app) version([]string) error {
	fmt.Fprintf(a.stdout, "agentory %s\n", Version)
	return nil
}

const helpHeader = `agentory - search your AI coding agent conversation history

Usage:
  agentory <query> [flags]           same as 'agentory search'
  agentory <command> [args] [flags]

Commands:
`

func (a *app) help(args []string) error {
	if len(args) > 0 {
		for _, c := range commands() {
			if c.name == args[0] && c.name != "help" {
				return c.run(a, []string{"--help"})
			}
		}
	}
	var sb strings.Builder
	sb.WriteString(helpHeader)
	for _, c := range commands() {
		fmt.Fprintf(&sb, "  %-10s %s\n", c.name, c.summary)
	}
	sb.WriteString("\nRun 'agentory help <command>' for flags.\n")
	io.WriteString(a.stdout, sb.String())
	return nil
}

func (a *app) usage(fs *flagSet, synopsis, desc string) {
	fmt.Fprintf(a.stdout, "Usage: %s\n\n%s\n\nFlags:\n%s\n", synopsis, desc, fs.help())
}

// parseFlags parses and turns flag errors into usage errors.
func (a *app) parseFlags(fs *flagSet, args []string, synopsis, desc string) ([]string, error) {
	pos, err := fs.parse(args)
	if errors.Is(err, flag.ErrHelp) {
		a.usage(fs, synopsis, desc)
		return nil, err
	}
	if err != nil {
		return nil, errUsage{err.Error()}
	}
	return pos, nil
}

// openIndex opens the database and, unless noSync, brings it up to date.
func (a *app) openIndex(noSync bool, sourceNames []string) (*index.DB, error) {
	db, err := index.Open(index.DefaultPath())
	if err != nil {
		return nil, err
	}
	if noSync {
		return db, nil
	}
	srcs, err := source.Select(sourceNames)
	if err != nil {
		db.Close()
		return nil, err
	}
	opt := index.Options{}
	if db.Meta("last_sync") == "" && isTerminal(a.stderr) {
		fmt.Fprintln(a.stderr, "Building the index for the first time…")
		opt.Progress = a.progress()
	}
	if _, err := db.Sync(a.ctx, srcs, opt); err != nil {
		db.Close()
		return nil, fmt.Errorf("sync: %w", err)
	}
	return db, nil
}

func (a *app) progress() func(done, total int) {
	last := time.Time{}
	return func(done, total int) {
		if done == total || time.Since(last) > 100*time.Millisecond {
			last = time.Now()
			fmt.Fprintf(a.stderr, "\r  %d/%d files", done, total)
			if done == total {
				fmt.Fprintln(a.stderr)
			}
		}
	}
}

// searchFlags are shared by search and show.
type searchFlags struct {
	project, since, until, kinds, role, tool, branch, sources, color string
	limit, context                                                   int
	all, subagent, json, noSync, explain                             bool
}

func (s *searchFlags) register(fs *flagSet, withLimit bool) {
	fs.str(&s.project, "p", "project", "", "<substr>", "filter by project name or cwd substring")
	fs.str(&s.since, "s", "since", "", "<when>", "only messages after (7d, 12h, 2026-09-01, today)")
	fs.str(&s.until, "u", "until", "", "<when>", "only messages before (a bare date includes that day)")
	fs.str(&s.kinds, "k", "kind", "", "<kinds>", "comma list: prompt,reply,think,command,summary,tool_use,tool_result,meta,system")
	fs.str(&s.role, "", "role", "", "<role>", "user | assistant | system")
	fs.str(&s.tool, "", "tool", "", "<name>", "filter by tool name (implies tool_use)")
	fs.str(&s.branch, "", "branch", "", "<name>", "filter by git branch")
	fs.str(&s.sources, "", "source", "", "<names>", "comma list of sources (available: "+strings.Join(source.Names(), ",")+")")
	if withLimit {
		fs.int(&s.limit, "n", "limit", 20, "<n>", "maximum number of results")
	}
	fs.int(&s.context, "C", "context", 0, "<n>", "show n messages before and after each hit")
	fs.bool(&s.all, "", "all", "include tool_use, tool_result, meta and system messages")
	fs.bool(&s.subagent, "", "include-subagent", "include sub-agent (sidechain) messages")
	fs.bool(&s.json, "", "json", "machine-readable output")
	fs.bool(&s.noSync, "", "no-sync", "skip the incremental index update before querying")
	fs.bool(&s.explain, "", "explain", "print the query plan (FTS5 MATCH or LIKE fallback)")
	fs.str(&s.color, "", "color", "auto", "<when>", "auto | always | never")
}

func (a *app) filter(s *searchFlags) (query.Filter, error) {
	f := query.Filter{
		Project: s.project, Role: s.role, Tool: s.tool, Branch: s.branch,
		Sources: splitList(s.sources), IncludeSubagent: s.subagent, Limit: s.limit,
	}
	var err error
	if f.Since, err = parseTime(s.since, a.now, false); err != nil {
		return f, errUsage{err.Error()}
	}
	if f.Until, err = parseTime(s.until, a.now, true); err != nil {
		return f, errUsage{err.Error()}
	}
	if f.Kinds, err = parseKinds(s.kinds); err != nil {
		return f, errUsage{err.Error()}
	}
	switch {
	case len(f.Kinds) > 0:
	case s.all:
		f.Kinds = model.AllKinds
	case s.tool != "":
		f.Kinds = []model.Kind{model.KindToolUse}
	}
	switch s.role {
	case "", "user", "assistant", "system":
	default:
		return f, errUsage{fmt.Sprintf("unknown role %q", s.role)}
	}
	if _, err := source.Select(f.Sources); err != nil {
		return f, errUsage{err.Error()}
	}
	if s.context < 0 || s.limit < 0 {
		return f, errUsage{"--context and --limit must not be negative"}
	}
	return f, nil
}

const searchDesc = `Search messages. Terms are ANDed and matched as case-insensitive substrings;
use double quotes for a phrase. Terms of 3+ characters use the FTS5 trigram
index; shorter terms (e.g. 2-character Chinese words) fall back to LIKE.
The index is updated incrementally before every query unless --no-sync.`

func (a *app) search(args []string) error {
	var s searchFlags
	fs := newFlagSet("search")
	s.register(fs, true)
	pos, err := a.parseFlags(fs, args, "agentory search <query> [flags]", searchDesc)
	if err != nil {
		return err
	}
	f, err := a.filter(&s)
	if err != nil {
		return err
	}
	q := strings.Join(pos, " ")
	if strings.TrimSpace(q) == "" && f.Project == "" && f.Since.IsZero() && f.Until.IsZero() && f.Tool == "" && f.Branch == "" {
		return errUsage{"missing query"}
	}
	db, err := a.openIndex(s.noSync, f.Sources)
	if err != nil {
		return err
	}
	defer db.Close()

	start := time.Now()
	hits, plan, err := query.Search(db, q, f)
	if err != nil {
		return err
	}
	elapsed := time.Since(start)
	r := a.renderer(s.color, plan.Terms)

	type ctxMsgs struct{ before, after []query.Message }
	ctxs := make([]ctxMsgs, len(hits))
	if s.context > 0 {
		for i, h := range hits {
			b, af, err := query.Context(db, h, s.context, f.Kinds)
			if err != nil {
				return err
			}
			ctxs[i] = ctxMsgs{b, af}
		}
	}

	if s.json {
		out := jsonSearch{Query: q, Count: len(hits), Hits: make([]jsonHit, 0, len(hits))}
		if s.explain {
			out.Plan = plan
		}
		for i, h := range hits {
			jh := toJSONHit(h, plan.Terms)
			if s.context > 0 {
				jh.Before, jh.After = toJSONCtx(ctxs[i].before, plan.Terms), toJSONCtx(ctxs[i].after, plan.Terms)
			}
			out.Hits = append(out.Hits, jh)
		}
		return a.writeJSON(out)
	}

	if s.explain {
		r.explain(plan, elapsed)
	}
	for i, h := range hits {
		if i > 0 {
			fmt.Fprintln(a.stdout)
		}
		r.hit(h, ctxs[i].before, ctxs[i].after, s.context)
	}
	if len(hits) == 0 {
		fmt.Fprintln(a.stderr, "no matches")
	}
	return nil
}

const showDesc = `Show one message in full (by numeric id, as printed by search) with -C
messages of context, or a whole session (by id or unique id prefix).`

func (a *app) show(args []string) error {
	var s searchFlags
	fs := newFlagSet("show")
	s.register(fs, false)
	pos, err := a.parseFlags(fs, args, "agentory show <msg-id|session-id> [-C N] [flags]", showDesc)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage{"expected exactly one message id or session id"}
	}
	f, err := a.filter(&s)
	if err != nil {
		return err
	}
	// show never needs a fresh sync to find an id it printed earlier, but a
	// session may have grown since.
	db, err := a.openIndex(s.noSync, f.Sources)
	if err != nil {
		return err
	}
	defer db.Close()
	r := a.renderer(s.color, nil)

	id := strings.TrimPrefix(pos[0], "#")
	if n, err := strconv.ParseInt(id, 10, 64); err == nil {
		m, err := query.Get(db, n)
		if err != nil {
			return err
		}
		kinds := f.Kinds
		if len(kinds) == 0 {
			kinds = model.DefaultKinds
		}
		before, after, err := query.Context(db, m, s.context, kinds)
		if err != nil {
			return err
		}
		if s.json {
			return a.writeJSON(jsonShow{Message: m, Before: nonNil(before), After: nonNil(after)})
		}
		for _, c := range before {
			r.full(c, true)
		}
		r.full(m, false)
		for _, c := range after {
			r.full(c, true)
		}
		return nil
	}

	sess, err := query.FindSession(db, id)
	if err != nil {
		return err
	}
	msgs, err := query.SessionMessages(db, sess.ID, f.Kinds, s.subagent)
	if err != nil {
		return err
	}
	if s.json {
		return a.writeJSON(jsonSession{Session: sess, Messages: nonNil(msgs)})
	}
	r.sessionHeader(sess)
	for _, m := range msgs {
		r.full(m, false)
	}
	return nil
}

func (a *app) sessions(args []string) error {
	var s searchFlags
	fs := newFlagSet("sessions")
	fs.str(&s.project, "p", "project", "", "<substr>", "filter by project name or cwd substring")
	fs.str(&s.since, "s", "since", "", "<when>", "active after (7d, 2026-09-01, …)")
	fs.str(&s.until, "u", "until", "", "<when>", "started before")
	fs.str(&s.branch, "", "branch", "", "<name>", "filter by git branch")
	fs.str(&s.sources, "", "source", "", "<names>", "comma list of sources")
	fs.int(&s.limit, "n", "limit", 20, "<n>", "maximum number of sessions")
	fs.bool(&s.json, "", "json", "machine-readable output")
	fs.bool(&s.noSync, "", "no-sync", "skip the incremental index update")
	fs.str(&s.color, "", "color", "auto", "<when>", "auto | always | never")
	pos, err := a.parseFlags(fs, args, "agentory sessions [flags]", "List sessions, most recently active first.")
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errUsage{"unexpected argument " + strconv.Quote(pos[0])}
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
	ss, err := query.Sessions(db, f)
	if err != nil {
		return err
	}
	if s.json {
		if ss == nil {
			ss = []query.Session{}
		}
		return a.writeJSON(ss)
	}
	a.renderer(s.color, nil).sessions(ss)
	return nil
}

func (a *app) projects(args []string) error {
	var jsonOut, noSync bool
	fs := newFlagSet("projects")
	fs.bool(&jsonOut, "", "json", "machine-readable output")
	fs.bool(&noSync, "", "no-sync", "skip the incremental index update")
	if _, err := a.parseFlags(fs, args, "agentory projects [flags]", "List projects, most recently active first."); err != nil {
		return err
	}
	db, err := a.openIndex(noSync, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	ps, err := query.Projects(db)
	if err != nil {
		return err
	}
	if jsonOut {
		if ps == nil {
			ps = []query.Project{}
		}
		return a.writeJSON(ps)
	}
	a.renderer("auto", nil).projects(ps)
	return nil
}

const indexDesc = `Build or update the index. Only new bytes of grown files are read; files
that were rewritten, truncated, or indexed under a different --full mode are
re-indexed from scratch. --full keeps up to 40000 characters of tool input and
output instead of 2000; the choice is remembered (use --full=false to revert).`

func (a *app) index(args []string) error {
	var rebuild, prune, quiet, verbose, jsonOut bool
	var sources string
	fs := newFlagSet("index")
	fullSet := false
	var full bool
	fs.BoolFunc("full", "store tool text up to 40000 chars (remembered)", func(v string) error {
		b, err := strconv.ParseBool(v)
		full, fullSet = b, true
		return err
	})
	fs.usage = append(fs.usage, fmt.Sprintf("  %-28s %s", "    --full[=false]", "store tool text up to 40000 chars (remembered)"))
	fs.bool(&rebuild, "", "rebuild", "drop the index and rebuild from scratch")
	fs.bool(&prune, "", "prune", "forget files that no longer exist on disk")
	fs.str(&sources, "", "source", "", "<names>", "comma list of sources")
	fs.bool(&quiet, "q", "quiet", "no progress output")
	fs.bool(&verbose, "v", "verbose", "list files that had to be re-indexed from scratch, with the reason")
	fs.bool(&jsonOut, "", "json", "machine-readable summary")
	pos, err := a.parseFlags(fs, args, "agentory index [--full] [--rebuild] [--prune]", indexDesc)
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		return errUsage{"unexpected argument " + strconv.Quote(pos[0])}
	}
	srcs, err := source.Select(splitList(sources))
	if err != nil {
		return errUsage{err.Error()}
	}
	db, err := index.Open(index.DefaultPath())
	if err != nil {
		return err
	}
	defer db.Close()
	if rebuild {
		if err := db.Reset(); err != nil {
			return err
		}
	}
	if fullSet {
		if err := db.SetFullMode(full); err != nil {
			return err
		}
	}
	opt := index.Options{Prune: prune}
	if verbose {
		opt.OnRebuild = func(path, reason string) { fmt.Fprintf(a.stderr, "rebuilt %s: %s\n", path, reason) }
	}
	if !quiet && !jsonOut && isTerminal(a.stderr) {
		opt.Progress = a.progress()
	}
	st, err := db.Sync(a.ctx, srcs, opt)
	if err != nil {
		return err
	}
	size := dbSize(db.Path)
	if jsonOut {
		return a.writeJSON(map[string]any{
			"files": st.Files, "unchanged": st.Unchanged, "new": st.New, "appended": st.Appended,
			"rebuilt": st.Rebuilt, "pruned": st.Pruned, "messages": st.Messages, "bad_lines": st.BadLines,
			"seconds": st.Duration.Seconds(), "db_path": db.Path, "db_bytes": size, "full": db.FullMode(),
		})
	}
	if !quiet {
		fmt.Fprintf(a.stdout, "%d files: %d new, %d appended, %d rebuilt, %d unchanged", st.Files, st.New, st.Appended, st.Rebuilt, st.Unchanged)
		if st.Pruned > 0 {
			fmt.Fprintf(a.stdout, ", %d pruned", st.Pruned)
		}
		fmt.Fprintf(a.stdout, "\n%d messages indexed in %s", st.Messages, st.Duration.Round(time.Millisecond))
		if st.BadLines > 0 {
			fmt.Fprintf(a.stdout, " (%d unparseable lines skipped)", st.BadLines)
		}
		fmt.Fprintf(a.stdout, "\nindex: %s (%s)\n", db.Path, humanBytes(size))
	}
	return nil
}

// dbSize is the on-disk size including the WAL.
func dbSize(path string) int64 {
	var n int64
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			n += fi.Size()
		}
	}
	return n
}

func (a *app) stats(args []string) error {
	var jsonOut, noSync bool
	fs := newFlagSet("stats")
	fs.bool(&jsonOut, "", "json", "machine-readable output")
	fs.bool(&noSync, "", "no-sync", "skip the incremental index update")
	if _, err := a.parseFlags(fs, args, "agentory stats [--json]", "Show index statistics."); err != nil {
		return err
	}
	db, err := a.openIndex(noSync, nil)
	if err != nil {
		return err
	}
	defer db.Close()
	st, err := query.GetStats(db)
	if err != nil {
		return err
	}
	st.DBBytes = dbSize(db.Path)
	if jsonOut {
		return a.writeJSON(st)
	}
	w := a.stdout
	fmt.Fprintf(w, "index     %s (%s)\n", st.DBPath, humanBytes(st.DBBytes))
	fmt.Fprintf(w, "files     %d\nsessions  %d\nmessages  %d\n", st.Files, st.Sessions, st.Messages)
	for _, k := range model.AllKinds {
		if n := st.ByKind[string(k)]; n > 0 {
			fmt.Fprintf(w, "  %-12s %d\n", k, n)
		}
	}
	for src, n := range st.BySource {
		fmt.Fprintf(w, "source    %s: %d\n", src, n)
	}
	if !st.Oldest.IsZero() {
		fmt.Fprintf(w, "range     %s … %s\n", st.Oldest.Local().Format("2006-01-02"), st.Newest.Local().Format("2006-01-02 15:04"))
	}
	fmt.Fprintf(w, "full mode %v\nlast sync %s\n", st.FullMode, st.LastSync)
	return nil
}

func (a *app) writeJSON(v any) error {
	enc := json.NewEncoder(a.stdout)
	enc.SetEscapeHTML(false)
	return enc.Encode(v)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
