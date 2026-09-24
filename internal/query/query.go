// Package query implements search and read access over the index.
package query

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/hao-ji-xing/agentory/internal/index"
	"github.com/hao-ji-xing/agentory/internal/model"
)

// MinFTSChars is the shortest term the trigram tokenizer can match.
const MinFTSChars = 3

// Filter narrows a search or listing.
type Filter struct {
	Project         string // substring of project key or cwd
	Since, Until    time.Time
	Kinds           []model.Kind
	Role            string
	Tool            string
	Branch          string
	Sources         []string
	IncludeSubagent bool
	Limit           int
}

// Term is one search term and the strategy used for it.
type Term struct {
	Text string `json:"text"`
	FTS  bool   `json:"fts"` // false: LIKE fallback
}

// Plan describes how a query is executed (for --explain).
type Plan struct {
	Terms []Term `json:"terms"`
	Mode  string `json:"mode"` // fts | like | fts+like | scan
	Match string `json:"match,omitempty"`
	SQL   string `json:"sql"`
	Args  []any  `json:"args"`
}

// Message is a stored message plus its session context.
type Message struct {
	ID        int64     `json:"id"`
	Source    string    `json:"source"`
	SessionID string    `json:"session_id"`
	AgentID   string    `json:"agent_id,omitempty"`
	Slug      string    `json:"slug,omitempty"`
	Time      time.Time `json:"time"`
	Role      string    `json:"role"`
	Kind      string    `json:"kind"`
	Tool      string    `json:"tool,omitempty"`
	CWD       string    `json:"cwd,omitempty"`
	Branch    string    `json:"branch,omitempty"`
	Project   string    `json:"project"`
	Title     string    `json:"title,omitempty"`
	NChars    int       `json:"n_chars"`
	Text      string    `json:"text"`

	fileID int64
	seq    int64
}

// ParseTerms splits a query into terms. Double quotes group words into one
// term. Each term is matched as a case-insensitive substring.
func ParseTerms(q string) []Term {
	var terms []Term
	add := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		terms = append(terms, Term{Text: s, FTS: utf8.RuneCountInString(s) >= MinFTSChars})
	}
	var cur strings.Builder
	inQuote := false
	for _, r := range q {
		switch {
		case r == '"':
			if inQuote {
				add(cur.String())
				cur.Reset()
			}
			inQuote = !inQuote
		case unicode.IsSpace(r) && !inQuote:
			add(cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	add(cur.String())
	return terms
}

func ftsPhrase(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

func likePattern(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(s) + "%"
}

const msgCols = `m.id, m.file_id, m.seq, m.source, m.session_id, m.agent_id, m.slug, m.ts, m.role, m.kind, m.tool,
	m.cwd, m.branch, m.n_chars, m.text, COALESCE(s.project, ''), COALESCE(s.title, '')`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var ts int64
	err := sc.Scan(&m.ID, &m.fileID, &m.seq, &m.Source, &m.SessionID, &m.AgentID, &m.Slug, &ts, &m.Role, &m.Kind,
		&m.Tool, &m.CWD, &m.Branch, &m.NChars, &m.Text, &m.Project, &m.Title)
	if err != nil {
		return m, err
	}
	m.Time = time.UnixMilli(ts)
	m.Project = index.ProjectLabel(m.CWD, m.Project)
	return m, nil
}

// where accumulates SQL conditions.
type where struct {
	conds []string
	args  []any
}

func (w *where) add(cond string, args ...any) {
	w.conds = append(w.conds, cond)
	w.args = append(w.args, args...)
}

func (w *where) sql() string {
	if len(w.conds) == 0 {
		return ""
	}
	return " WHERE " + strings.Join(w.conds, " AND ")
}

func inList(n int) string {
	return "(" + strings.TrimSuffix(strings.Repeat("?,", n), ",") + ")"
}

func (f *Filter) kinds() []model.Kind {
	if len(f.Kinds) == 0 {
		return model.DefaultKinds
	}
	return f.Kinds
}

func (f *Filter) apply(w *where) {
	ks := f.kinds()
	args := make([]any, len(ks))
	for i, k := range ks {
		args[i] = string(k)
	}
	w.add("m.kind IN "+inList(len(ks)), args...)
	if !f.IncludeSubagent {
		w.add("m.agent_id = ''")
	}
	if f.Project != "" {
		p := likePattern(f.Project)
		w.add(`(s.project LIKE ? ESCAPE '\' OR m.cwd LIKE ? ESCAPE '\')`, p, p)
	}
	if !f.Since.IsZero() {
		w.add("m.ts >= ?", f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		w.add("m.ts < ?", f.Until.UnixMilli())
	}
	if f.Role != "" {
		w.add("m.role = ?", f.Role)
	}
	if f.Tool != "" {
		w.add("m.tool = ? COLLATE NOCASE", f.Tool)
	}
	if f.Branch != "" {
		w.add("m.branch = ?", f.Branch)
	}
	if len(f.Sources) > 0 {
		args := make([]any, len(f.Sources))
		for i, s := range f.Sources {
			args[i] = s
		}
		w.add("m.source IN "+inList(len(f.Sources)), args...)
	}
}

// BuildSearch plans a search without running it.
func BuildSearch(q string, f Filter) *Plan {
	p := &Plan{Terms: ParseTerms(q)}
	w := &where{}
	var phrases []string
	nLike := 0
	for _, t := range p.Terms {
		if t.FTS {
			phrases = append(phrases, ftsPhrase(t.Text))
		} else {
			nLike++
		}
	}
	if len(phrases) > 0 {
		p.Match = strings.Join(phrases, " AND ")
		w.add("m.id IN (SELECT rowid FROM msgs_fts WHERE msgs_fts MATCH ?)", p.Match)
	}
	for _, t := range p.Terms {
		if !t.FTS {
			w.add(`m.text LIKE ? ESCAPE '\'`, likePattern(t.Text))
		}
	}
	switch {
	case len(phrases) > 0 && nLike > 0:
		p.Mode = "fts+like"
	case len(phrases) > 0:
		p.Mode = "fts"
	case nLike > 0:
		p.Mode = "like"
	default:
		p.Mode = "scan"
	}
	f.apply(w)
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	p.SQL = "SELECT " + msgCols + " FROM msgs m LEFT JOIN sessions s ON s.id = m.session_id" +
		w.sql() + " ORDER BY m.ts DESC, m.id DESC LIMIT ?"
	p.Args = append(w.args, limit)
	return p
}

// Search runs q with filter f, newest first.
func Search(db *index.DB, q string, f Filter) ([]Message, *Plan, error) {
	p := BuildSearch(q, f)
	msgs, err := collect(db.Query(p.SQL, p.Args...))
	return msgs, p, err
}

func collect(rows *sql.Rows, err error) ([]Message, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// Context returns up to n messages before and after m from the same
// transcript and session, restricted to the given kinds.
func Context(db *index.DB, m Message, n int, kinds []model.Kind) (before, after []Message, err error) {
	if n <= 0 {
		return nil, nil, nil
	}
	if len(kinds) == 0 {
		kinds = model.DefaultKinds
	}
	args := []any{m.fileID, m.SessionID}
	for _, k := range kinds {
		args = append(args, string(k))
	}
	base := "SELECT " + msgCols + " FROM msgs m LEFT JOIN sessions s ON s.id = m.session_id" +
		" WHERE m.file_id = ? AND m.session_id = ? AND m.kind IN " + inList(len(kinds))
	before, err = collect(db.Query(base+" AND m.seq < ? ORDER BY m.seq DESC LIMIT ?", append(args, m.seq, n)...))
	if err != nil {
		return nil, nil, err
	}
	for i, j := 0, len(before)-1; i < j; i, j = i+1, j-1 {
		before[i], before[j] = before[j], before[i]
	}
	after, err = collect(db.Query(base+" AND m.seq > ? ORDER BY m.seq ASC LIMIT ?", append(args, m.seq, n)...))
	return before, after, err
}

// Get returns a message by id.
func Get(db *index.DB, id int64) (Message, error) {
	row := db.QueryRow("SELECT "+msgCols+" FROM msgs m LEFT JOIN sessions s ON s.id = m.session_id WHERE m.id = ?", id)
	m, err := scanMessage(row)
	if err == sql.ErrNoRows {
		return m, fmt.Errorf("no message with id %d", id)
	}
	return m, err
}

// Session is a row of the sessions table.
type Session struct {
	ID          string    `json:"id"`
	Source      string    `json:"source"`
	Project     string    `json:"project"`
	ProjectKey  string    `json:"project_key"`
	CWD         string    `json:"cwd,omitempty"`
	Branch      string    `json:"branch,omitempty"`
	Title       string    `json:"title,omitempty"`
	FirstPrompt string    `json:"first_prompt,omitempty"`
	StartedAt   time.Time `json:"started_at"`
	EndedAt     time.Time `json:"ended_at"`
	NMsg        int       `json:"n_msg"`
}

const sessionCols = `s.id, s.source, s.project, s.cwd, s.branch, s.title, s.first_prompt, s.started_at, s.ended_at, s.n_msg`

func collectSessions(rows *sql.Rows, err error) ([]Session, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var s Session
		var st, en int64
		if err := rows.Scan(&s.ID, &s.Source, &s.ProjectKey, &s.CWD, &s.Branch, &s.Title, &s.FirstPrompt, &st, &en, &s.NMsg); err != nil {
			return nil, err
		}
		s.StartedAt, s.EndedAt = time.UnixMilli(st), time.UnixMilli(en)
		s.Project = index.ProjectLabel(s.CWD, s.ProjectKey)
		out = append(out, s)
	}
	return out, rows.Err()
}

// Sessions lists sessions, most recently active first. Kinds, role and
// tool filters do not apply.
func Sessions(db *index.DB, f Filter) ([]Session, error) {
	w := &where{}
	w.add("s.n_msg > 0")
	if f.Project != "" {
		p := likePattern(f.Project)
		w.add(`(s.project LIKE ? ESCAPE '\' OR s.cwd LIKE ? ESCAPE '\')`, p, p)
	}
	if !f.Since.IsZero() {
		w.add("s.ended_at >= ?", f.Since.UnixMilli())
	}
	if !f.Until.IsZero() {
		w.add("s.started_at < ?", f.Until.UnixMilli())
	}
	if f.Branch != "" {
		w.add("s.branch = ?", f.Branch)
	}
	if len(f.Sources) > 0 {
		args := make([]any, len(f.Sources))
		for i, s := range f.Sources {
			args[i] = s
		}
		w.add("s.source IN "+inList(len(f.Sources)), args...)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 20
	}
	return collectSessions(db.Query("SELECT "+sessionCols+" FROM sessions s"+w.sql()+
		" ORDER BY s.ended_at DESC LIMIT ?", append(w.args, limit)...))
}

// FindSession resolves a full or unique-prefix session id.
func FindSession(db *index.DB, id string) (Session, error) {
	ss, err := collectSessions(db.Query("SELECT "+sessionCols+" FROM sessions s WHERE s.id = ? OR s.id LIKE ? ESCAPE '\\' ORDER BY s.id = ? DESC LIMIT 2",
		id, strings.NewReplacer(`%`, `\%`, `_`, `\_`).Replace(id)+"%", id))
	if err != nil {
		return Session{}, err
	}
	switch {
	case len(ss) == 0:
		return Session{}, fmt.Errorf("no session matches %q", id)
	case len(ss) > 1 && ss[0].ID != id:
		return Session{}, fmt.Errorf("session prefix %q is ambiguous", id)
	}
	return ss[0], nil
}

// SessionMessages returns the messages of a session in transcript order.
func SessionMessages(db *index.DB, id string, kinds []model.Kind, includeSubagent bool) ([]Message, error) {
	if len(kinds) == 0 {
		kinds = model.DefaultKinds
	}
	args := []any{id}
	for _, k := range kinds {
		args = append(args, string(k))
	}
	sub := " AND m.agent_id = ''"
	if includeSubagent {
		sub = ""
	}
	return collect(db.Query("SELECT "+msgCols+" FROM msgs m LEFT JOIN sessions s ON s.id = m.session_id"+
		" WHERE m.session_id = ? AND m.kind IN "+inList(len(kinds))+sub+" ORDER BY m.ts, m.file_id, m.seq", args...))
}

// Project aggregates sessions by project.
type Project struct {
	Project    string    `json:"project"`
	ProjectKey string    `json:"project_key"`
	CWD        string    `json:"cwd,omitempty"`
	Sessions   int       `json:"sessions"`
	Messages   int       `json:"messages"`
	LastActive time.Time `json:"last_active"`
}

// Projects lists projects, most recently active first.
func Projects(db *index.DB) ([]Project, error) {
	rows, err := db.Query(`SELECT project,
		(SELECT cwd FROM sessions s2 WHERE s2.project = s.project AND s2.cwd <> '' ORDER BY ended_at DESC LIMIT 1),
		count(*), sum(n_msg), max(ended_at)
		FROM sessions s WHERE n_msg > 0 GROUP BY project ORDER BY max(ended_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Project
	for rows.Next() {
		var p Project
		var cwd sql.NullString
		var last int64
		if err := rows.Scan(&p.ProjectKey, &cwd, &p.Sessions, &p.Messages, &last); err != nil {
			return nil, err
		}
		p.CWD = cwd.String
		p.Project = index.ProjectLabel(p.CWD, p.ProjectKey)
		p.LastActive = time.UnixMilli(last)
		out = append(out, p)
	}
	return out, rows.Err()
}

// Stats summarizes the index.
type Stats struct {
	Files    int            `json:"files"`
	Sessions int            `json:"sessions"`
	Messages int            `json:"messages"`
	ByKind   map[string]int `json:"by_kind"`
	BySource map[string]int `json:"by_source"`
	Oldest   time.Time      `json:"oldest"`
	Newest   time.Time      `json:"newest"`
	LastSync string         `json:"last_sync,omitempty"`
	FullMode bool           `json:"full_mode"`
	DBPath   string         `json:"db_path"`
	DBBytes  int64          `json:"db_bytes"`
}

// GetStats computes index statistics.
func GetStats(db *index.DB) (Stats, error) {
	st := Stats{ByKind: map[string]int{}, BySource: map[string]int{}, DBPath: db.Path,
		LastSync: db.Meta("last_sync"), FullMode: db.FullMode()}
	if err := db.QueryRow(`SELECT count(*) FROM files`).Scan(&st.Files); err != nil {
		return st, err
	}
	if err := db.QueryRow(`SELECT count(*) FROM sessions WHERE n_msg > 0`).Scan(&st.Sessions); err != nil {
		return st, err
	}
	for q, m := range map[string]map[string]int{
		`SELECT kind, count(*) FROM msgs GROUP BY kind`:     st.ByKind,
		`SELECT source, count(*) FROM msgs GROUP BY source`: st.BySource,
	} {
		rows, err := db.Query(q)
		if err != nil {
			return st, err
		}
		for rows.Next() {
			var k string
			var n int
			if err := rows.Scan(&k, &n); err != nil {
				rows.Close()
				return st, err
			}
			m[k] = n
		}
		rows.Close()
	}
	for _, n := range st.ByKind {
		st.Messages += n
	}
	var lo, hi sql.NullInt64
	db.QueryRow(`SELECT min(ts), max(ts) FROM msgs WHERE ts > 0`).Scan(&lo, &hi)
	if lo.Valid {
		st.Oldest, st.Newest = time.UnixMilli(lo.Int64), time.UnixMilli(hi.Int64)
	}
	var pages, size int64
	db.QueryRow(`PRAGMA page_count`).Scan(&pages)
	db.QueryRow(`PRAGMA page_size`).Scan(&size)
	st.DBBytes = pages * size
	return st, nil
}
