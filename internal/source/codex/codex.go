// Package codex parses OpenAI Codex session transcripts:
//
//	$CODEX_HOME/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl
//	$CODEX_HOME/archived_sessions/rollout-<time>-<thread id>.jsonl
//	$CODEX_HOME/session_index.jsonl   (thread titles)
//
// Unlike Claude Code, a Codex line does not repeat its context: the session
// id, working directory and git branch are written once in the leading
// session_meta record, and the model once per turn in turn_context. The
// source therefore implements model.FileSource and parses each file with a
// parser that carries that context from line to line.
//
// Codex writes most facts twice, as a response_item (what the model saw) and
// as an event_msg (what the UI showed). One stream is used per fact so that
// nothing is indexed twice:
//
//	prompt       event_msg user_message (≤ 0.148), item_completed UserMessage (≥ 0.153)
//	reply        response_item message, role assistant
//	think        event_msg agent_reasoning (reasoning summaries)
//	tool_use     response_item function_call, custom_tool_call, web_search_call, tool_search_call
//	tool_result  response_item function_call_output, custom_tool_call_output
//	usage        event_msg token_count
//	turn         event_msg task_complete
//
// User-role response_items are skipped: they repeat the prompt and add
// injected context (AGENTS.md, environment_context). Developer messages
// (harness instructions) are skipped too.
package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hao-ji-xing/agentory/internal/model"
	"github.com/hao-ji-xing/agentory/internal/source/claudecode"
)

// Name is the value stored in the `source` column.
const Name = "codex"

// indexFile holds one {id, thread_name} line per titled thread.
const indexFile = "session_index.jsonl"

// Source implements model.Source and model.FileSource for Codex.
type Source struct {
	home string
}

// New returns a Source rooted at $CODEX_HOME, falling back to ~/.codex.
func New() *Source { return NewWithHome(DefaultHome()) }

// NewWithHome returns a Source that reads the Codex home directory home.
func NewWithHome(home string) *Source { return &Source{home: filepath.Clean(home)} }

// DefaultHome resolves the Codex home directory.
func DefaultHome() string {
	if d := os.Getenv("CODEX_HOME"); d != "" {
		return d
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".codex"
	}
	return filepath.Join(home, ".codex")
}

func (s *Source) Name() string { return Name }

// Roots lists the two transcript directories and the title index. A file
// root is walked like a directory with a single entry.
func (s *Source) Roots() []string {
	return []string{
		filepath.Join(s.home, "sessions"),
		filepath.Join(s.home, "archived_sessions"),
		filepath.Join(s.home, indexFile),
	}
}

func (s *Source) isIndex(path string) bool {
	return filepath.Clean(path) == filepath.Join(s.home, indexFile)
}

// Match accepts rollout-*.jsonl below either transcript directory, and the
// title index.
func (s *Source) Match(path string) bool {
	if s.isIndex(path) {
		return true
	}
	base := filepath.Base(path)
	if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
		return false
	}
	for _, root := range s.Roots()[:2] {
		if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
			return true
		}
	}
	return false
}

// ProjectOf derives the project key from the session's working directory,
// encoded the way Claude Code names its project directories
// (/home/alice/shop → -home-alice-shop), so that one project filter matches
// both agents. The directory tree itself is organized by date.
func (s *Source) ProjectOf(path string) string {
	if s.isIndex(path) {
		return ""
	}
	meta, err := readSessionMeta(path)
	if err != nil || meta == nil {
		return ""
	}
	return ProjectKey(meta.CWD)
}

var reNonKey = regexp.MustCompile(`[^A-Za-z0-9-]`)

// ProjectKey encodes a working directory as a project key.
func ProjectKey(cwd string) string {
	if cwd == "" {
		return ""
	}
	return reNonKey.ReplaceAllString(cwd, "-")
}

// ParseLine parses a line without file context. Lines that do not carry
// their session id (most of them) yield nothing; the indexer uses OpenFile.
func (s *Source) ParseLine(line []byte) (model.Parsed, error) {
	return (&parser{}).ParseLine(line)
}

// OpenFile returns a parser for one transcript. It reads the session_meta
// record, and when parsing resumes mid-file, the turn context in force at
// the resume point.
func (s *Source) OpenFile(path string, resumeAt int64) (model.FileParser, error) {
	if s.isIndex(path) {
		return indexParser{}, nil
	}
	p := &parser{thread: threadIDFromPath(path)}
	p.session = p.thread
	if resumeAt <= 0 {
		return p, nil // the first line read will be session_meta
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	br := bufio.NewReaderSize(io.LimitReader(f, resumeAt), 1<<20)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 && carriesContext(line) {
			p.ParseLine(line) // updates the state; the output was indexed before
		}
		if err != nil {
			break
		}
	}
	return p, nil
}

var reThreadID = regexp.MustCompile(`([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.jsonl$`)

// threadIDFromPath extracts the thread id that ends every rollout file name.
func threadIDFromPath(path string) string {
	if m := reThreadID.FindStringSubmatch(filepath.Base(path)); m != nil {
		return m[1]
	}
	return ""
}

// carriesContext reports, without decoding, whether a line may change the
// parser state: session_meta, turn_context or thread_settings_applied. The
// record type sits near the start of every line.
func carriesContext(line []byte) bool {
	head := line[:min(len(line), 256)]
	return bytes.Contains(head, []byte(`"type":"session_meta"`)) ||
		bytes.Contains(head, []byte(`"type":"turn_context"`)) ||
		bytes.Contains(head, []byte(`"type":"thread_settings_applied"`))
}

// readSessionMeta decodes the session_meta record on the first line.
func readSessionMeta(path string) (*sessionMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	line, err := bufio.NewReaderSize(io.LimitReader(f, 16<<20), 256<<10).ReadBytes('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	var r record
	if json.Unmarshal(line, &r) != nil || r.Type != "session_meta" {
		return nil, nil
	}
	var m sessionMeta
	if err := json.Unmarshal(r.Payload, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// indexParser reads session_index.jsonl.
type indexParser struct{}

func (indexParser) ParseLine(line []byte) (model.Parsed, error) {
	var p model.Parsed
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return p, nil
	}
	var e struct {
		ID         string `json:"id"`
		ThreadName string `json:"thread_name"`
	}
	if err := json.Unmarshal(line, &e); err != nil {
		return p, fmt.Errorf("codex: %w", err)
	}
	if title := strings.TrimSpace(e.ThreadName); e.ID != "" && title != "" {
		p.Meta = &model.SessionMeta{SessionID: e.ID, Title: title, TitleRank: model.TitleRankAuto}
	}
	return p, nil
}

// parser carries the context of one transcript.
type parser struct {
	thread  string // this transcript's thread id
	session string // session the messages belong to: the parent thread for sub-agents
	agent   string // the thread id, for sub-agent transcripts only
	slug    string // sub-agent kind, e.g. "guardian"
	cwd     string
	branch  string
	model   string
	sdk     bool // started by `codex exec` rather than a person
}

type record struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type sessionMeta struct {
	ID             string          `json:"id"`
	SessionID      string          `json:"session_id"`
	ParentThreadID string          `json:"parent_thread_id"`
	CWD            string          `json:"cwd"`
	Originator     string          `json:"originator"`
	Source         json.RawMessage `json:"source"`
	Git            *struct {
		Branch string `json:"branch"`
	} `json:"git"`
}

type textItem struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   []textItem      `json:"content"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
	Input     string          `json:"input"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Action    json.RawMessage `json:"action"`
}

type tokenUsage struct {
	Input     int64 `json:"input_tokens"`
	Cached    int64 `json:"cached_input_tokens"`
	Output    int64 `json:"output_tokens"`
	Reasoning int64 `json:"reasoning_output_tokens"`
	Total     int64 `json:"total_tokens"`
}

type eventMsg struct {
	Type        string   `json:"type"`
	Message     string   `json:"message"`
	LocalImages []string `json:"local_images"`
	Images      []string `json:"images"`
	Text        string   `json:"text"`
	Reason      string   `json:"reason"`
	DurationMs  int64    `json:"duration_ms"`
	NumTurns    int64    `json:"num_turns"`
	Item        *struct {
		Type    string     `json:"type"`
		ID      string     `json:"id"`
		Content []textItem `json:"content"`
	} `json:"item"`
	Info *struct {
		Total tokenUsage `json:"total_token_usage"`
		Last  tokenUsage `json:"last_token_usage"`
	} `json:"info"`
	ThreadSettings *struct {
		Model string `json:"model"`
		CWD   string `json:"cwd"`
	} `json:"thread_settings"`
}

// ParseLine implements model.FileParser.
func (p *parser) ParseLine(line []byte) (model.Parsed, error) {
	var out model.Parsed
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return out, nil
	}
	var r record
	if err := json.Unmarshal(line, &r); err != nil {
		return out, fmt.Errorf("codex: %w", err)
	}
	ts, _ := time.Parse(time.RFC3339Nano, r.Timestamp)
	ts = ts.UTC()
	switch r.Type {
	case "session_meta":
		var m sessionMeta
		if err := json.Unmarshal(r.Payload, &m); err != nil {
			return out, fmt.Errorf("codex: session_meta: %w", err)
		}
		p.applyMeta(&m)
	case "turn_context":
		var c struct {
			CWD   string `json:"cwd"`
			Model string `json:"model"`
		}
		if json.Unmarshal(r.Payload, &c) == nil {
			p.cwd = firstNonEmpty(c.CWD, p.cwd)
			p.model = firstNonEmpty(c.Model, p.model)
		}
	case "response_item":
		var it responseItem
		if err := json.Unmarshal(r.Payload, &it); err != nil {
			return out, fmt.Errorf("codex: response_item: %w", err)
		}
		out.Messages = p.responseItem(&it, ts)
	case "event_msg":
		var e eventMsg
		if err := json.Unmarshal(r.Payload, &e); err != nil {
			return out, fmt.Errorf("codex: event_msg: %w", err)
		}
		p.event(&e, ts, &out)
	case "compacted":
		// The summary itself is encrypted; keep a marker of when it happened.
		out.Messages = p.one(model.KindSystem, "system", "[context compacted]", ts)
	}
	return out, nil
}

func (p *parser) applyMeta(m *sessionMeta) {
	if m.ID != "" {
		p.thread = m.ID
	}
	p.session = firstNonEmpty(m.ParentThreadID, m.SessionID, p.thread)
	if p.session != p.thread {
		p.agent = p.thread
		p.slug = subagentKind(m.Source)
	}
	p.cwd = firstNonEmpty(m.CWD, p.cwd)
	if m.Git != nil {
		p.branch = firstNonEmpty(m.Git.Branch, p.branch)
	}
	var src string
	_ = json.Unmarshal(m.Source, &src)
	p.sdk = src == "exec" || m.Originator == "codex_exec"
}

// subagentKind reads {"subagent": {"other": "guardian"}} or
// {"subagent": "review"}.
func subagentKind(raw json.RawMessage) string {
	var v struct {
		Subagent json.RawMessage `json:"subagent"`
	}
	if json.Unmarshal(raw, &v) != nil || len(v.Subagent) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(v.Subagent, &s) == nil {
		return s
	}
	var m map[string]string
	if json.Unmarshal(v.Subagent, &m) == nil {
		for k, val := range m {
			return firstNonEmpty(val, k)
		}
	}
	return ""
}

func (p *parser) base(kind model.Kind, role string, ts time.Time) model.Message {
	m := model.Message{
		SessionID: p.session, AgentID: p.agent, Slug: p.slug,
		Time: ts, Role: role, Kind: kind, CWD: p.cwd, Branch: p.branch,
	}
	if role == "assistant" {
		m.Model = p.model
	}
	return m
}

func (p *parser) one(kind model.Kind, role, text string, ts time.Time) []model.Message {
	if p.session == "" || text == "" {
		return nil
	}
	m := p.base(kind, role, ts)
	m.Text = text
	return []model.Message{m}
}

func (p *parser) responseItem(it *responseItem, ts time.Time) []model.Message {
	if p.session == "" {
		return nil
	}
	switch it.Type {
	case "message":
		if it.Role != "assistant" {
			return nil // user: repeats event prompts plus injected context; developer: instructions
		}
		var parts []string
		for _, c := range it.Content {
			if t := strings.TrimSpace(c.Text); c.Type == "output_text" && t != "" {
				parts = append(parts, t)
			}
		}
		out := p.one(model.KindReply, "assistant", strings.Join(parts, "\n"), ts)
		for i := range out {
			out[i].UUID = it.ID
		}
		return out
	case "function_call", "custom_tool_call", "web_search_call", "tool_search_call":
		m := p.base(model.KindToolUse, "assistant", ts)
		m.UUID, m.ToolUseID, m.Tool = it.ID, it.CallID, it.Name
		switch it.Type {
		case "function_call":
			m.Text = claudecode.RenderInput(argumentsJSON(it.Arguments))
		case "custom_tool_call":
			m.Text = strings.TrimSpace(it.Input)
		case "web_search_call":
			m.Tool, m.Text = "web_search", claudecode.RenderInput(it.Action)
		case "tool_search_call":
			m.Tool, m.Text = "tool_search", claudecode.RenderInput(it.Arguments)
		}
		describeToolUse(&m)
		if m.Text == "" {
			return nil
		}
		return []model.Message{m}
	case "function_call_output", "custom_tool_call_output":
		text := strings.TrimSpace(outputText(it.Output))
		if text == "" && it.CallID == "" {
			return nil
		}
		m := p.base(model.KindToolResult, "user", ts)
		m.ToolUseID, m.Text, m.IsError = it.CallID, text, failed(text)
		return []model.Message{m}
	}
	return nil
}

func (p *parser) event(e *eventMsg, ts time.Time, out *model.Parsed) {
	switch e.Type {
	case "thread_settings_applied":
		if s := e.ThreadSettings; s != nil {
			p.cwd = firstNonEmpty(s.CWD, p.cwd)
			p.model = firstNonEmpty(s.Model, p.model)
		}
	case "user_message":
		text := strings.TrimSpace(e.Message)
		for range len(e.Images) + len(e.LocalImages) {
			text = strings.TrimSpace(text + "\n[image]")
		}
		out.Messages = p.prompt(text, ts)
	case "item_completed":
		if e.Item == nil || e.Item.Type != "UserMessage" {
			return // agent messages, reasoning and commands repeat response_items
		}
		var parts []string
		for _, c := range e.Item.Content {
			switch c.Type {
			case "text":
				parts = append(parts, c.Text)
			case "image", "local_image":
				parts = append(parts, "[image]")
			}
		}
		out.Messages = p.prompt(strings.TrimSpace(strings.Join(parts, "\n")), ts)
		for i := range out.Messages {
			out.Messages[i].UUID = e.Item.ID
		}
	case "agent_reasoning":
		out.Messages = p.one(model.KindThink, "assistant", strings.TrimSpace(e.Text), ts)
	case "turn_aborted":
		text := "[Turn aborted: " + e.Reason + "]"
		if e.Reason == "interrupted" {
			text = "[Request interrupted by user]" // the marker Claude Code writes
		}
		out.Messages = p.one(model.KindMeta, "user", text, ts)
	case "thread_rolled_back":
		out.Messages = p.one(model.KindSystem, "system", fmt.Sprintf("[rolled back %d turn(s)]", e.NumTurns), ts)
	case "task_complete":
		if p.session != "" && e.DurationMs > 0 {
			out.Turn = &model.Turn{SessionID: p.session, AgentID: p.agent, CWD: p.cwd, Branch: p.branch,
				Time: ts, DurationMs: e.DurationMs}
		}
	case "token_count":
		out.Usage = p.usage(e, ts)
	}
}

var reSkillMention = regexp.MustCompile(`\[\$([A-Za-z0-9_.:-]+)\]\([^)]*\)`)

// prompt builds a prompt message. Mentioning a skill as [$name](path) is
// how a Codex user invokes one; it is recorded like a typed command.
func (p *parser) prompt(text string, ts time.Time) []model.Message {
	out := p.one(model.KindPrompt, "user", text, ts)
	if len(out) == 0 {
		return nil
	}
	m := &out[0]
	switch {
	case p.sdk:
		m.PromptSource = "sdk"
	case strings.HasPrefix(text, "Automation:"):
		m.PromptSource = "automation"
	default:
		m.PromptSource = "typed"
	}
	if sm := reSkillMention.FindStringSubmatch(text); sm != nil {
		m.InvKind, m.InvName = model.InvCommand, sm[1]
		m.InvArgs = strings.TrimSpace(reSkillMention.ReplaceAllString(text, ""))
	}
	return out
}

// usage turns a token_count event into one API request. Codex reports no
// request id; the running thread total is unique per request and repeats
// when the same count is emitted twice, which the upsert then absorbs.
// OpenAI counts cached tokens inside input_tokens, so they are split out.
func (p *parser) usage(e *eventMsg, ts time.Time) *model.Usage {
	if e.Info == nil || p.session == "" || e.Info.Total.Total == 0 {
		return nil
	}
	u := e.Info.Last
	return &model.Usage{
		RequestID: p.thread + ":" + strconv.FormatInt(e.Info.Total.Total, 10),
		SessionID: p.session, AgentID: p.agent, Model: p.model, CWD: p.cwd, Branch: p.branch, Time: ts,
		Input: max(u.Input-u.Cached, 0), Output: u.Output, CacheRead: u.Cached, Thinking: u.Reasoning,
	}
}

// argumentsJSON unwraps function_call arguments, a JSON object encoded as a
// string.
func argumentsJSON(raw json.RawMessage) json.RawMessage {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return json.RawMessage(s)
	}
	return raw
}

// outputText flattens a tool output: a string, or a list of input_text items.
func outputText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var items []textItem
	if json.Unmarshal(raw, &items) != nil {
		return ""
	}
	var parts []string
	for _, it := range items {
		switch it.Type {
		case "input_text", "output_text", "text":
			parts = append(parts, it.Text)
		case "input_image":
			parts = append(parts, "[image]")
		}
	}
	return strings.Join(parts, "\n")
}

var (
	reExitCode  = regexp.MustCompile(`(?:Process exited with code|Exit code:) (\d+)`)
	rePatchFile = regexp.MustCompile(`(?m)^\*\*\* (?:Update|Add|Delete) File: (.+)$`)
	// Codex uses a skill by reading its SKILL.md; .system holds built-ins.
	reSkillFile = regexp.MustCompile(`skills/(?:\.system/)?([A-Za-z0-9_][A-Za-z0-9_.-]*)/SKILL\.md`)
)

// failed reports whether a tool output says the command failed.
func failed(text string) bool {
	head := text[:min(len(text), 400)]
	if strings.HasPrefix(head, "Script failed") {
		return true
	}
	if m := reExitCode.FindStringSubmatch(head); m != nil {
		return m[1] != "0"
	}
	return false
}

// describeToolUse fills the file and invocation fields of a tool call.
func describeToolUse(m *model.Message) {
	if m.Tool == "apply_patch" {
		if f := rePatchFile.FindStringSubmatch(m.Text); f != nil {
			m.FilePath = strings.TrimSpace(f[1])
		}
		return // editing a SKILL.md is not using the skill
	}
	if s := reSkillFile.FindStringSubmatch(m.Text); s != nil {
		m.InvKind, m.InvName = model.InvSkill, s[1]
	}
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
