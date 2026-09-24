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

	"github.com/hao-ji-xing/agentory/internal/model"
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
	PromptSource     string          `json:"promptSource"`
	RequestID        string          `json:"requestId"`
	Message          *struct {
		ID      string          `json:"id"`
		Role    string          `json:"role"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *apiUsage       `json:"usage"`
	} `json:"message"`
	Content     json.RawMessage `json:"content"` // system records
	Subtype     string          `json:"subtype"`
	DurationMs  int64           `json:"durationMs"`   // system turn_duration
	MsgCount    int64           `json:"messageCount"` // system turn_duration
	AITitle     string          `json:"aiTitle"`
	CustomTitle string          `json:"customTitle"`
	AgentName   string          `json:"agentName"`
	Attachment  *struct {
		Type        string          `json:"type"`
		CommandMode string          `json:"commandMode"`
		Prompt      json.RawMessage `json:"prompt"`
		Origin      *struct {
			Kind string `json:"kind"`
		} `json:"origin"`
	} `json:"attachment"`
	// cost-state
	TotalCostUSD      float64 `json:"totalCostUSD"`
	TotalLinesAdded   int64   `json:"totalLinesAdded"`
	TotalLinesRemoved int64   `json:"totalLinesRemoved"`
	TotalDuration     int64   `json:"totalDuration"`
}

type apiUsage struct {
	Input         int64 `json:"input_tokens"`
	Output        int64 `json:"output_tokens"`
	CacheRead     int64 `json:"cache_read_input_tokens"`
	CacheCreation int64 `json:"cache_creation_input_tokens"`
	Creation      *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
	OutputDetails *struct {
		Thinking int64 `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type block struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"` // tool_use
	Text      string          `json:"text"`
	Thinking  string          `json:"thinking"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"` // tool_result
	IsError   bool            `json:"is_error"`    // tool_result
	Content   json.RawMessage `json:"content"`     // tool_result payload
}

// ParseLine implements model.Source.
func (s *Source) ParseLine(line []byte) (model.Parsed, error) {
	var p model.Parsed
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return p, nil
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return p, fmt.Errorf("claudecode: %w", err)
	}
	switch r.Type {
	case "ai-title":
		p.Meta = titleMeta(&r, r.AITitle, model.TitleRankAuto)
	case "agent-name":
		p.Meta = titleMeta(&r, r.AgentName, model.TitleRankAuto)
	case "custom-title":
		p.Meta = titleMeta(&r, r.CustomTitle, model.TitleRankCustom)
	case "cost-state":
		if r.SessionID != "" {
			p.Meta = &model.SessionMeta{SessionID: r.SessionID, Cost: &model.SessionCost{
				USD: r.TotalCostUSD, LinesAdded: r.TotalLinesAdded,
				LinesRemoved: r.TotalLinesRemoved, DurationMs: r.TotalDuration,
			}}
		}
	case "user":
		if r.Message != nil {
			p.Messages = parseUser(&r, r.Message.Content, r.PromptSource)
		}
	case "attachment":
		p.Messages = parseQueued(&r)
	case "assistant":
		p.Messages = parseAssistant(&r)
		p.Usage = usageOf(&r)
	case "system":
		p.Messages = parseSystem(&r)
		if r.Subtype == "turn_duration" && r.DurationMs > 0 {
			b := base(&r, "system")
			p.Turn = &model.Turn{SessionID: b.SessionID, AgentID: b.AgentID, CWD: b.CWD, Branch: b.Branch,
				Time: b.Time, DurationMs: r.DurationMs, Messages: r.MsgCount}
		}
	}
	return p, nil
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

// parseUser handles user content, which is a string or a block array.
func parseUser(r *record, content json.RawMessage, source string) []model.Message {
	var out []model.Message
	emitText := func(raw string) {
		c := classifyUser(r, raw)
		if c.text == "" {
			return
		}
		m := base(r, "user")
		m.Kind, m.Text, m.PromptSource = c.kind, c.text, source
		if c.kind == model.KindCommand {
			m.InvKind, m.InvName, m.InvArgs = model.InvCommand, strings.TrimPrefix(c.name, "/"), c.args
		}
		out = append(out, m)
	}

	// Plain string content.
	var str string
	if json.Unmarshal(content, &str) == nil {
		emitText(str)
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
			// Keep empty results that link to a call: they carry the outcome.
			if text := Clean(renderToolResult(b.Content)); text != "" || b.ToolUseID != "" {
				m := base(r, "user")
				m.Kind, m.Text, m.ToolUseID, m.IsError = model.KindToolResult, text, b.ToolUseID, b.IsError
				out = append(out, m)
			}
		}
	}
	if len(parts) > 0 {
		emitText(strings.Join(parts, "\n"))
	}
	return out
}

// parseQueued recovers input the user typed while the agent was busy. Such
// input is recorded only as a queued_command attachment.
func parseQueued(r *record) []model.Message {
	a := r.Attachment
	if a == nil || a.Type != "queued_command" || a.CommandMode != "prompt" || len(a.Prompt) == 0 {
		return nil
	}
	if a.Origin != nil && a.Origin.Kind != "human" {
		return nil // automatic continuations and coordinator messages
	}
	return parseUser(r, a.Prompt, "queued")
}

type classified struct {
	kind       model.Kind
	text       string
	name, args string // for commands
}

// classifyUser applies the cleaning rules and decides the kind of a
// user-authored text. An empty text means "drop it".
func classifyUser(r *record, raw string) classified {
	if r.IsCompactSummary {
		return classified{kind: model.KindSummary, text: strings.TrimSpace(raw)}
	}
	text := Clean(raw)
	if r.IsMeta {
		return classified{kind: model.KindMeta, text: text}
	}
	if name, args, ok := extractCommand(raw); ok {
		return classified{kind: model.KindCommand, text: strings.TrimSpace(name + " " + args), name: name, args: args}
	}
	if isMetaText(text) {
		return classified{kind: model.KindMeta, text: text}
	}
	return classified{kind: model.KindPrompt, text: text}
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
		m.Model, m.RequestID = r.Message.Model, requestID(r)
		switch b.Type {
		case "text":
			m.Kind, m.Text = model.KindReply, strings.TrimSpace(b.Text)
		case "thinking":
			m.Kind, m.Text = model.KindThink, strings.TrimSpace(b.Thinking)
		case "tool_use":
			m.Kind, m.Tool, m.Text, m.ToolUseID = model.KindToolUse, b.Name, RenderInput(b.Input), b.ID
			describeToolUse(&m, b.Input)
		default:
			continue
		}
		if m.Text != "" {
			out = append(out, m)
		}
	}
	return out
}

// describeToolUse fills the file and invocation fields of a tool call.
func describeToolUse(m *model.Message, raw json.RawMessage) {
	var in map[string]json.RawMessage
	if json.Unmarshal(raw, &in) != nil {
		return
	}
	str := func(k string) string {
		var s string
		if json.Unmarshal(in[k], &s) == nil {
			return strings.TrimSpace(s)
		}
		if v := in[k]; len(v) > 0 && string(v) != "null" {
			return string(v)
		}
		return ""
	}
	m.FilePath = str("file_path")
	if m.FilePath == "" {
		m.FilePath = str("notebook_path")
	}
	switch m.Tool {
	case "Skill":
		if name := strings.TrimPrefix(str("skill"), "/"); name != "" {
			m.InvKind, m.InvName, m.InvArgs = model.InvSkill, name, str("args")
		}
	case "Agent", "Task":
		name := str("subagent_type")
		if name == "" {
			name = "(default)"
		}
		m.InvKind, m.InvName, m.InvArgs = model.InvSubagent, name, str("description")
	}
}

func requestID(r *record) string {
	if r.RequestID != "" {
		return r.RequestID
	}
	return r.Message.ID
}

func usageOf(r *record) *model.Usage {
	if r.Message == nil || r.Message.Usage == nil {
		return nil
	}
	id := requestID(r)
	if id == "" {
		return nil
	}
	u := r.Message.Usage
	b := base(r, "assistant")
	out := &model.Usage{
		RequestID: id, SessionID: b.SessionID, AgentID: b.AgentID, Model: r.Message.Model,
		CWD: b.CWD, Branch: b.Branch, Time: b.Time,
		Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
	}
	if u.Creation != nil {
		out.CacheWrite5m, out.CacheWrite1h = u.Creation.Ephemeral5m, u.Creation.Ephemeral1h
	} else {
		out.CacheWrite5m = u.CacheCreation // older records do not split by TTL
	}
	if u.OutputDetails != nil {
		out.Thinking = u.OutputDetails.Thinking
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

// shortValue is the longest value rendered in the leading group of
// RenderInput.
const shortValue = 200

// RenderInput turns tool_use input into "key=value" lines so that argument
// values are searchable as plain text. Short values come first (then long
// ones), each group sorted by key: when the text is truncated for storage,
// identifying fields such as skill or subagent_type survive a long prompt.
func RenderInput(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return strings.TrimSpace(string(raw))
	}
	type kv struct{ k, v string }
	pairs := make([]kv, 0, len(obj))
	for k, raw := range obj {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			s = string(raw)
		}
		pairs = append(pairs, kv{k, s})
	}
	sort.Slice(pairs, func(i, j int) bool {
		si, sj := len(pairs[i].v) <= shortValue, len(pairs[j].v) <= shortValue
		if si != sj {
			return si
		}
		return pairs[i].k < pairs[j].k
	})
	var sb strings.Builder
	for i, p := range pairs {
		if i > 0 {
			sb.WriteByte('\n')
		}
		sb.WriteString(p.k)
		sb.WriteByte('=')
		sb.WriteString(p.v)
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
// The cheap substring checks skip the regexes for the common case of large
// tool output without any wrapper.
func Clean(s string) string {
	if strings.Contains(s, "<system-reminder>") {
		s = reSystemReminder.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "<local-command-caveat>") {
		s = reCaveat.ReplaceAllString(s, "")
	}
	if strings.Contains(s, "<local-command-stdout>") {
		s = reStdout.ReplaceAllString(s, "")
	}
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
