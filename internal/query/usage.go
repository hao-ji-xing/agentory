package query

import (
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/hao-ji-xing/agentory/internal/index"
)

// UsageOptions sizes a Usage report.
type UsageOptions struct {
	Groups int // argument groups to return (default 20)
	Recent int // recent invocations to return (default 10)
}

// ArgGroup is one distinct way a command or skill was called.
type ArgGroup struct {
	Args   string    `json:"args"` // whitespace-normalized; "" = no arguments
	Count  int       `json:"count"`
	Last   time.Time `json:"last"`
	LastID int64     `json:"last_id"`
}

// Invocation is one use of a command, skill or sub-agent.
type Invocation struct {
	ID          int64     `json:"id"`
	Time        time.Time `json:"time"`
	Actor       string    `json:"actor"` // user | agent
	Kind        string    `json:"kind"`  // command | skill | subagent
	Project     string    `json:"project"`
	SessionID   string    `json:"session_id"`
	Args        string    `json:"args"`
	Error       bool      `json:"error"`       // the call's result was an error
	Interrupted bool      `json:"interrupted"` // the user interrupted before the next prompt
	Next        string    `json:"next,omitempty"`
	NextID      int64     `json:"next_id,omitempty"`
	// NextSource is how the next prompt was entered: "typed", or "queued"
	// when the user wrote it while this invocation was still running.
	NextSource string `json:"next_source,omitempty"`
}

// UsageResult describes how one command, skill or sub-agent type is used.
type UsageResult struct {
	Name        string         `json:"name"`
	Total       int            `json:"total"`
	ByActor     map[string]int `json:"by_actor"`
	ByKind      map[string]int `json:"by_kind"`
	WithArgs    int            `json:"with_args"`
	Errors      int            `json:"errors"`
	Interrupted int            `json:"interrupted"`
	First       time.Time      `json:"first"`
	Last        time.Time      `json:"last"`
	Projects    []Bucket       `json:"projects"`
	ArgGroups   []ArgGroup     `json:"arg_groups"` // most frequent first
	Distinct    int            `json:"distinct_args"`
	Recent      []Invocation   `json:"recent"`
	Suggestions []string       `json:"suggestions,omitempty"` // similar names when nothing matched
}

const usageSQL = `SELECT m.id, m.ts, m.session_id, m.inv_kind, m.inv_args, m.cwd, COALESCE(s.project, ''),
	CASE WHEN m.tool_use_id <> '' THEN EXISTS (SELECT 1 FROM msgs r WHERE r.tool_use_id = m.tool_use_id
		AND r.tool_use_id <> '' AND r.kind = 'tool_result' AND r.is_error) ELSE 0 END,
	COALESCE(n.id, 0),
	EXISTS (SELECT 1 FROM msgs x WHERE x.file_id = m.file_id AND x.seq > m.seq
		AND x.seq < COALESCE(n.seq, 9223372036854775807)
		AND x.kind = 'meta' AND x.text LIKE '[Request interrupted%')
	FROM msgs m
	LEFT JOIN sessions s ON s.id = m.session_id
	LEFT JOIN msgs n ON n.id = (SELECT p.id FROM msgs p WHERE p.file_id = m.file_id AND p.seq > m.seq
		AND p.kind = 'prompt' ORDER BY p.seq LIMIT 1)`

// Usage reports how the named command, skill or sub-agent type was used:
// who invoked it, with which arguments, whether it failed or was
// interrupted, and what the user said next. A leading "/" is ignored.
func Usage(db *index.DB, name string, f Filter, opt UsageOptions) (*UsageResult, error) {
	name = strings.TrimPrefix(strings.TrimSpace(name), "/")
	w := &where{}
	w.add("m.inv_kind IN ('command', 'skill', 'subagent')") // lets msgs_inv seek by name
	w.add("m.inv_name = ?", name)
	f.applyCommon(w)
	rows, err := db.Query(usageSQL+w.sql()+" ORDER BY m.ts DESC, m.id DESC", w.args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &UsageResult{Name: name, ByActor: map[string]int{}, ByKind: map[string]int{},
		ArgGroups: []ArgGroup{}, Projects: []Bucket{}, Recent: []Invocation{}}
	groups := map[string]*ArgGroup{}
	projects := map[string]*Bucket{}
	for rows.Next() {
		var in Invocation
		var ts int64
		var cwd, projectKey string
		if err := rows.Scan(&in.ID, &ts, &in.SessionID, &in.Kind, &in.Args, &cwd, &projectKey,
			&in.Error, &in.NextID, &in.Interrupted); err != nil {
			return nil, err
		}
		in.Time = time.UnixMilli(ts)
		in.Project = index.ProjectLabel(cwd, projectKey)
		in.Actor = "agent"
		if in.Kind == "command" {
			in.Actor = "user"
		}
		res.Total++
		res.ByActor[in.Actor]++
		res.ByKind[in.Kind]++
		if in.Args != "" {
			res.WithArgs++
		}
		if in.Error {
			res.Errors++
		}
		if in.Interrupted {
			res.Interrupted++
		}
		if res.Last.IsZero() {
			res.Last = in.Time
		}
		res.First = in.Time

		key := Flatten(in.Args)
		if g := groups[key]; g != nil {
			g.Count++
		} else {
			groups[key] = &ArgGroup{Args: key, Count: 1, Last: in.Time, LastID: in.ID}
		}
		if b := projects[in.Project]; b != nil {
			b.Count++
		} else {
			projects[in.Project] = &Bucket{Key: in.Project, Count: 1, Last: in.Time}
		}
		if len(res.Recent) < orDefault(opt.Recent, 10) {
			res.Recent = append(res.Recent, in)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, g := range groups {
		res.ArgGroups = append(res.ArgGroups, *g)
	}
	res.Distinct = len(res.ArgGroups)
	sort.Slice(res.ArgGroups, func(i, j int) bool {
		a, b := res.ArgGroups[i], res.ArgGroups[j]
		if a.Count != b.Count {
			return a.Count > b.Count
		}
		return a.Last.After(b.Last)
	})
	if n := orDefault(opt.Groups, 20); len(res.ArgGroups) > n {
		res.ArgGroups = res.ArgGroups[:n]
	}
	for _, b := range projects {
		res.Projects = append(res.Projects, *b)
	}
	sort.Slice(res.Projects, func(i, j int) bool {
		if res.Projects[i].Count != res.Projects[j].Count {
			return res.Projects[i].Count > res.Projects[j].Count
		}
		return res.Projects[i].Key < res.Projects[j].Key
	})
	if err := fillNext(db, res.Recent); err != nil {
		return nil, err
	}
	if res.Total == 0 {
		res.Suggestions, err = similarNames(db, name)
	}
	return res, err
}

func orDefault(n, def int) int {
	if n <= 0 {
		return def
	}
	return n
}

// fillNext loads a snippet of the prompt that followed each invocation.
func fillNext(db *index.DB, evs []Invocation) error {
	for i := range evs {
		if evs[i].NextID == 0 {
			continue
		}
		var text string
		err := db.QueryRow(`SELECT text, prompt_source FROM msgs WHERE id = ?`, evs[i].NextID).Scan(&text, &evs[i].NextSource)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		evs[i].Next = Snippet(text, nil, 200)
	}
	return nil
}

// similarNames lists invocation names containing name, most used first.
func similarNames(db *index.DB, name string) ([]string, error) {
	rows, err := db.Query(`SELECT inv_name FROM msgs WHERE inv_kind <> '' AND inv_name LIKE ? ESCAPE '\'
		GROUP BY inv_name ORDER BY count(*) DESC LIMIT 10`, likePattern(name))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
