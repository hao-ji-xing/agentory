package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/haojixing/agentory/internal/query"
)

const snippetWidth = 160

const (
	ansiReset  = "\x1b[0m"
	ansiBold   = "\x1b[1m"
	ansiDim    = "\x1b[2m"
	ansiCyan   = "\x1b[36m"
	ansiYellow = "\x1b[33m"
	ansiHit    = "\x1b[1;31m"
)

type renderer struct {
	w     io.Writer
	color bool
	terms []query.Term
}

func isTerminal(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func (a *app) renderer(mode string, terms []query.Term) *renderer {
	color := false
	switch mode {
	case "always":
		color = true
	case "never":
	default:
		color = isTerminal(a.stdout) && os.Getenv("NO_COLOR") == "" && os.Getenv("TERM") != "dumb"
	}
	return &renderer{w: a.stdout, color: color, terms: terms}
}

func (r *renderer) paint(code, s string) string {
	if !r.color || s == "" {
		return s
	}
	return code + s + ansiReset
}

func (r *renderer) highlight(s string) string {
	if !r.color {
		return s
	}
	return query.Highlight(s, r.terms, ansiHit, ansiReset)
}

// marker is a compact role/kind tag: u> prompt, a> reply, …
func marker(m query.Message) string {
	var s string
	switch m.Kind {
	case "prompt":
		s = "u>"
	case "command":
		s = "u/"
	case "reply":
		s = "a>"
	case "think":
		s = "a~"
	case "tool_use":
		s = "a$ " + m.Tool
	case "tool_result":
		s = "t<"
	case "summary":
		s = "Σ"
	case "meta":
		s = "m:"
	case "system":
		s = "s:"
	default:
		s = m.Kind
	}
	if m.AgentID != "" {
		tag := m.Slug
		if tag == "" {
			tag = m.AgentID
		}
		s = "[sub:" + tag + "] " + s
	}
	return s
}

func location(project, branch string) string {
	if branch == "" || branch == "HEAD" {
		return project
	}
	return project + "/" + branch
}

func localTime(t time.Time) string {
	if t.UnixMilli() == 0 {
		return "????-??-?? ??:??"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func (r *renderer) header(m query.Message) string {
	return fmt.Sprintf("%s  %s  %s  %s",
		r.paint(ansiBold+ansiYellow, fmt.Sprintf("#%d", m.ID)),
		localTime(m.Time),
		r.paint(ansiCyan, location(m.Project, m.Branch)),
		marker(m))
}

func (r *renderer) hit(m query.Message, before, after []query.Message, n int) {
	for _, c := range before {
		r.contextLine(c)
	}
	fmt.Fprintln(r.w, r.header(m))
	fmt.Fprintf(r.w, "  %s\n", r.highlight(query.Snippet(m.Text, r.terms, snippetWidth)))
	for _, c := range after {
		r.contextLine(c)
	}
	hint := fmt.Sprintf("agentory show %d", m.ID)
	if n > 0 {
		hint += fmt.Sprintf(" -C %d", n)
	} else {
		hint += " -C 3"
	}
	fmt.Fprintf(r.w, "  %s\n", r.paint(ansiDim, "→ "+hint))
}

func (r *renderer) contextLine(m query.Message) {
	line := fmt.Sprintf("   #%d %s %s %s", m.ID, m.Time.Local().Format("15:04"), marker(m),
		query.Snippet(m.Text, nil, snippetWidth-40))
	fmt.Fprintln(r.w, r.paint(ansiDim, line))
}

// full prints a message in full; dim marks surrounding context.
func (r *renderer) full(m query.Message, dim bool) {
	head := r.header(m)
	if m.Title != "" && !dim {
		head += "  " + r.paint(ansiDim, "“"+m.Title+"”")
	}
	if dim {
		head = r.paint(ansiDim, fmt.Sprintf("#%d  %s  %s  %s", m.ID, localTime(m.Time), location(m.Project, m.Branch), marker(m)))
	}
	fmt.Fprintln(r.w, head)
	text := strings.TrimRight(m.Text, "\n")
	if m.NChars > len([]rune(text)) {
		text += fmt.Sprintf("\n[truncated: %d of %d chars stored; re-index with --full for more]", len([]rune(text)), m.NChars)
	}
	for _, l := range strings.Split(text, "\n") {
		if dim {
			l = r.paint(ansiDim, l)
		}
		fmt.Fprintln(r.w, "  "+l)
	}
	fmt.Fprintln(r.w)
}

func (r *renderer) sessionHeader(s query.Session) {
	fmt.Fprintf(r.w, "%s  %s\n", r.paint(ansiBold, s.ID), r.paint(ansiCyan, location(s.Project, s.Branch)))
	if s.Title != "" {
		fmt.Fprintf(r.w, "title   %s\n", s.Title)
	}
	fmt.Fprintf(r.w, "time    %s … %s\n", localTime(s.StartedAt), localTime(s.EndedAt))
	if s.CWD != "" {
		fmt.Fprintf(r.w, "cwd     %s\n", s.CWD)
	}
	fmt.Fprintf(r.w, "msgs    %d\n\n", s.NMsg)
}

func (r *renderer) sessions(ss []query.Session) {
	for _, s := range ss {
		label := s.Title
		if label == "" {
			label = query.Snippet(s.FirstPrompt, nil, 80)
		}
		fmt.Fprintf(r.w, "%s  %s  %-28s %5d msgs  %s\n",
			localTime(s.EndedAt), r.paint(ansiYellow, shortID(s.ID)),
			r.paint(ansiCyan, truncRunes(location(s.Project, s.Branch), 28)), s.NMsg, label)
	}
}

func (r *renderer) projects(ps []query.Project) {
	for _, p := range ps {
		fmt.Fprintf(r.w, "%-24s %5d sessions %7d msgs  last %s  %s\n",
			r.paint(ansiCyan, truncRunes(p.Project, 24)), p.Sessions, p.Messages, localTime(p.LastActive), r.paint(ansiDim, p.CWD))
	}
}

func (r *renderer) explain(p *query.Plan, elapsed time.Duration) {
	var parts []string
	for _, t := range p.Terms {
		how := "LIKE (shorter than 3 chars)"
		if t.FTS {
			how = "FTS5 MATCH"
		}
		parts = append(parts, fmt.Sprintf("%q→%s", t.Text, how))
	}
	fmt.Fprintf(r.w, "plan: mode=%s  terms: %s\n", p.Mode, strings.Join(parts, ", "))
	if p.Match != "" {
		fmt.Fprintf(r.w, "match: %s\n", p.Match)
	}
	fmt.Fprintf(r.w, "sql: %s\nargs: %v\ntime: %s\n\n", strings.Join(strings.Fields(p.SQL), " "), p.Args, elapsed.Round(time.Microsecond))
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func truncRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

// JSON shapes. Hits carry a snippet rather than the whole text so that an
// agent can scan many results cheaply; `show --json` returns full text.

type jsonSearch struct {
	Query string      `json:"query"`
	Plan  *query.Plan `json:"plan,omitempty"`
	Count int         `json:"count"`
	Hits  []jsonHit   `json:"hits"`
}

type jsonHit struct {
	ID        int64       `json:"id"`
	Source    string      `json:"source"`
	SessionID string      `json:"session_id"`
	AgentID   string      `json:"agent_id,omitempty"`
	Time      time.Time   `json:"time"`
	Project   string      `json:"project"`
	Branch    string      `json:"branch,omitempty"`
	CWD       string      `json:"cwd,omitempty"`
	Title     string      `json:"title,omitempty"`
	Role      string      `json:"role"`
	Kind      string      `json:"kind"`
	Tool      string      `json:"tool,omitempty"`
	NChars    int         `json:"n_chars"`
	Snippet   string      `json:"snippet"`
	Show      string      `json:"show"`
	Before    []jsonBrief `json:"before,omitempty"`
	After     []jsonBrief `json:"after,omitempty"`
}

type jsonBrief struct {
	ID      int64     `json:"id"`
	Time    time.Time `json:"time"`
	Role    string    `json:"role"`
	Kind    string    `json:"kind"`
	Tool    string    `json:"tool,omitempty"`
	Snippet string    `json:"snippet"`
}

type jsonShow struct {
	Message query.Message   `json:"message"`
	Before  []query.Message `json:"before"`
	After   []query.Message `json:"after"`
}

type jsonSession struct {
	Session  query.Session   `json:"session"`
	Messages []query.Message `json:"messages"`
}

const jsonSnippetWidth = 300

func toJSONHit(m query.Message, terms []query.Term) jsonHit {
	return jsonHit{
		ID: m.ID, Source: m.Source, SessionID: m.SessionID, AgentID: m.AgentID, Time: m.Time,
		Project: m.Project, Branch: m.Branch, CWD: m.CWD, Title: m.Title, Role: m.Role, Kind: m.Kind,
		Tool: m.Tool, NChars: m.NChars, Snippet: query.Snippet(m.Text, terms, jsonSnippetWidth),
		Show: fmt.Sprintf("agentory show %d -C 3", m.ID),
	}
}

func toJSONCtx(ms []query.Message, terms []query.Term) []jsonBrief {
	out := make([]jsonBrief, 0, len(ms))
	for _, m := range ms {
		out = append(out, jsonBrief{ID: m.ID, Time: m.Time, Role: m.Role, Kind: m.Kind, Tool: m.Tool,
			Snippet: query.Snippet(m.Text, terms, jsonSnippetWidth)})
	}
	return out
}

func nonNil(ms []query.Message) []query.Message {
	if ms == nil {
		return []query.Message{}
	}
	return ms
}
