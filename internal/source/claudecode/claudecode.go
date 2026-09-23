// Package claudecode parses Claude Code session transcripts
// (~/.claude/projects/<project>/<session>.jsonl).
package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/haojixing/agentory/internal/model"
)

// Name is the value stored in the `source` column.
const Name = "claude"

// Source implements model.Source for Claude Code.
type Source struct {
	root string
}

// New returns a Source rooted at $CLAUDE_CONFIG_DIR/projects, falling back
// to ~/.claude/projects.
func New() *Source {
	return NewWithRoot(DefaultRoot())
}

// NewWithRoot returns a Source that scans root.
func NewWithRoot(root string) *Source {
	return &Source{root: filepath.Clean(root)}
}

// DefaultRoot resolves the Claude Code projects directory.
func DefaultRoot() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "projects")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".claude", "projects")
	}
	return filepath.Join(home, ".claude", "projects")
}

func (s *Source) Name() string    { return Name }
func (s *Source) Roots() []string { return []string{s.root} }

// Match accepts *.jsonl files below the root, including sub-agent
// transcripts in <session>/subagents/.
func (s *Source) Match(path string) bool {
	if !strings.HasSuffix(path, ".jsonl") {
		return false
	}
	rel, err := filepath.Rel(s.root, path)
	return err == nil && rel != "." && !strings.HasPrefix(rel, "..")
}

// ProjectOf returns the project directory name (the first path component
// below the root), e.g. "-Users-alice-code-demo".
func (s *Source) ProjectOf(path string) string {
	rel, err := filepath.Rel(s.root, path)
	if err != nil {
		return ""
	}
	first, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
	return first
}

// record is the union of the top-level fields we care about.
type record struct {
	Type             string          `json:"type"`
	UUID             string          `json:"uuid"`
	ParentUUID       string          `json:"parentUuid"`
	Timestamp        string          `json:"timestamp"`
	SessionID        string          `json:"sessionId"`
	CWD              string          `json:"cwd"`
	GitBranch        string          `json:"gitBranch"`
	AgentID          string          `json:"agentId"`
	Slug             json.RawMessage `json:"slug"`
	IsMeta           bool            `json:"isMeta"`
	IsCompactSummary bool            `json:"isCompactSummary"`
	Message          *struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Content     json.RawMessage `json:"content"` // system records
	Subtype     string          `json:"subtype"`
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	AgentName   string          `json:"agentName"`
}

type block struct {
	Type     string          `json:"type"`
	Text     string          `json:"text"`
	Thinking string          `json:"thinking"`
	Name     string          `json:"name"`
	Input    json.RawMessage `json:"input"`
	Content  json.RawMessage `json:"content"` // tool_result payload
}

// ParseLine implements model.Source.
func (s *Source) ParseLine(line []byte) ([]model.Message, *model.SessionMeta, error) {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return nil, nil, nil
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return nil, nil, fmt.Errorf("claudecode: %w", err)
	}
	switch r.Type {
	case "ai-title":
		return nil, titleMeta(&r, r.AITitle, model.TitleRankAuto), nil
	case "agent-name":
		return nil, titleMeta(&r, r.AgentName, model.TitleRankAuto), nil
	case "custom-title":
		return nil, titleMeta(&r, r.CustomTitle, model.TitleRankCustom), nil
	case "user":
		return parseUser(&r), nil, nil
	case "assistant":
		return parseAssistant(&r), nil, nil
	case "system":
		return parseSystem(&r), nil, nil
	}
	return nil, nil, nil
}

func titleMeta(r *record, title string, rank int) *model.SessionMeta {
	title = strings.TrimSpace(title)
	if title == "" || r.SessionID == "" {
		return nil
	}
	return &model.SessionMeta{SessionID: r.SessionID, Title: title, TitleRank: rank}
}

func base(r *record, role string) model.Message {
	m := model.Message{
		SessionID:  r.SessionID,
		AgentID:    r.AgentID,
		UUID:       r.UUID,
		ParentUUID: r.ParentUUID,
		Role:       role,
		CWD:        r.CWD,
		Branch:     r.GitBranch,
	}
	var slug string
	if len(r.Slug) > 0 && json.Unmarshal(r.Slug, &slug) == nil {
		m.Slug = slug
	}
	if t, err := time.Parse(time.RFC3339Nano, r.Timestamp); err == nil {
		m.Time = t.UTC()
	}
	return m
}

func parseUser(r *record) []model.Message {
	if r.Message == nil {
		return nil
	}
	content := r.Message.Content
	var out []model.Message
	emit := func(kind model.Kind, text string) {
		if text == "" {
			return
		}
		m := base(r, "user")
		m.Kind, m.Text = kind, text
		out = append(out, m)
	}

	// Plain string content.
	var str string
	if json.Unmarshal(content, &str) == nil {
		emit(classifyUser(r, str))
		return out
	}

	var blocks []block
	if json.Unmarshal(content, &blocks) != nil {
		return nil
	}
	// Text and image blocks are merged into one message; each tool_result
	// becomes its own message.
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		case "tool_result":
			emit(model.KindToolResult, Clean(renderToolResult(b.Content)))
		}
	}
	if len(parts) > 0 {
		emit(classifyUser(r, strings.Join(parts, "\n")))
	}
	return out
}

// classifyUser applies the cleaning rules and decides the kind of a
// user-authored text. An empty text means "drop it".
func classifyUser(r *record, raw string) (model.Kind, string) {
	if r.IsCompactSummary {
		return model.KindSummary, strings.TrimSpace(raw)
	}
	text := Clean(raw)
	if r.IsMeta {
		return model.KindMeta, text
	}
	if name, args, ok := extractCommand(raw); ok {
		return model.KindCommand, strings.TrimSpace(name + " " + args)
	}
	if isMetaText(text) {
		return model.KindMeta, text
	}
	return model.KindPrompt, text
}

func parseAssistant(r *record) []model.Message {
	if r.Message == nil {
		return nil
	}
	var blocks []block
	if json.Unmarshal(r.Message.Content, &blocks) != nil {
		// Some producers use a bare string.
		var str string
		if json.Unmarshal(r.Message.Content, &str) == nil {
			blocks = []block{{Type: "text", Text: str}}
		}
	}
	var out []model.Message
	for _, b := range blocks {
		m := base(r, "assistant")
		switch b.Type {
		case "text":
			m.Kind, m.Text = model.KindReply, strings.TrimSpace(b.Text)
		case "thinking":
			m.Kind, m.Text = model.KindThink, strings.TrimSpace(b.Thinking)
		case "tool_use":
			m.Kind, m.Tool, m.Text = model.KindToolUse, b.Name, RenderInput(b.Input)
		default:
			continue
		}
		if m.Text != "" {
			out = append(out, m)
		}
	}
	return out
}

func parseSystem(r *record) []model.Message {
	var str string
	if len(r.Content) == 0 || json.Unmarshal(r.Content, &str) != nil {
		return nil
	}
	text := Clean(str)
	if text == "" {
		return nil
	}
	m := base(r, "system")
	m.Kind, m.Text = model.KindSystem, text
	return []model.Message{m}
}

// renderToolResult flattens a tool_result payload, which is either a string
// or an array of blocks (text / image).
func renderToolResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		return str
	}
	var blocks []block
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text":
			parts = append(parts, b.Text)
		case "image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

// RenderInput turns tool_use input into sorted "key=value" lines so that
// argument values are searchable as plain text.
func RenderInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return strings.TrimSpace(string(raw))
	}
	keys := make([]string, 0, len(obj))
	for k := range obj {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		var s string
		if json.Unmarshal(obj[k], &s) == nil {
			sb.WriteString(s)
		} else {
			sb.Write(obj[k])
		}
	}
	return sb.String()
}

var (
	reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)
	reCaveat         = regexp.MustCompile(`(?s)<local-command-caveat>.*?</local-command-caveat>`)
	reStdout         = regexp.MustCompile(`(?s)<local-command-stdout>.*?</local-command-stdout>`)
	reCommandName    = regexp.MustCompile(`(?s)<command-name>(.*?)</command-name>`)
	reCommandArgs    = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// Clean strips injected wrappers that would otherwise drown search results.
func Clean(s string) string {
	s = reSystemReminder.ReplaceAllString(s, "")
	s = reCaveat.ReplaceAllString(s, "")
	s = reStdout.ReplaceAllString(s, "")
	return strings.TrimSpace(s)
}

func extractCommand(s string) (name, args string, ok bool) {
	m := reCommandName.FindStringSubmatch(s)
	if m == nil {
		return "", "", false
	}
	name = strings.TrimSpace(m[1])
	if a := reCommandArgs.FindStringSubmatch(s); a != nil {
		args = strings.TrimSpace(a[1])
	}
	return name, args, name != ""
}

func isMetaText(s string) bool {
	return s == "[Request interrupted by user]" ||
		strings.HasPrefix(s, "[Request interrupted by user") ||
		strings.HasPrefix(s, "Caveat:") ||
		strings.HasPrefix(s, "<task-notification>")
}
