package codex

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hao-ji-xing/agentory/internal/model"
)

const (
	s1 = "0192f0c1-0000-7000-8000-000000000001" // interactive, ≤ 0.148 layout
	s2 = "0192f0c1-0000-7000-8000-000000000002" // codex exec, 0.153 layout
	s3 = "0192f0c1-0000-7000-8000-000000000003" // guardian sub-agent of s1
)

var home = filepath.Join("..", "..", "..", "testdata", "codex")

func fixturePath(t *testing.T, sid string) string {
	t.Helper()
	var found string
	filepath.WalkDir(home, func(p string, d os.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(p, sid+".jsonl") {
			found = p
		}
		return nil
	})
	if found == "" {
		t.Fatalf("fixture for %s not found", sid)
	}
	return found
}

type parsed struct {
	msgs   []model.Message
	usages []model.Usage
	turns  []model.Turn
	metas  []model.SessionMeta
}

// parseFrom parses path through OpenFile starting at byte off.
func parseFrom(t *testing.T, src *Source, path string, off int64) parsed {
	t.Helper()
	fp, err := src.OpenFile(path, off)
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out parsed
	sc := bufio.NewScanner(bytes.NewReader(b[off:]))
	sc.Buffer(nil, 64<<20)
	for sc.Scan() {
		p, err := fp.ParseLine(sc.Bytes())
		if err != nil {
			t.Fatalf("%s: %v", sc.Text(), err)
		}
		out.msgs = append(out.msgs, p.Messages...)
		if p.Usage != nil {
			out.usages = append(out.usages, *p.Usage)
		}
		if p.Turn != nil {
			out.turns = append(out.turns, *p.Turn)
		}
		if p.Meta != nil {
			out.metas = append(out.metas, *p.Meta)
		}
	}
	return out
}

func parseAll(t *testing.T, sid string) parsed {
	return parseFrom(t, NewWithHome(home), fixturePath(t, sid), 0)
}

func ofKind(ms []model.Message, k model.Kind) []model.Message {
	var out []model.Message
	for _, m := range ms {
		if m.Kind == k {
			out = append(out, m)
		}
	}
	return out
}

func TestPromptsComeFromUserEventsOnly(t *testing.T) {
	p := parseAll(t, s1)
	prompts := ofKind(p.msgs, model.KindPrompt)
	if len(prompts) != 2 {
		t.Fatalf("s1 prompts = %d, want 2: %+v", len(prompts), prompts)
	}
	if prompts[0].Text != "Why does the zebraledger total drift after a refund?\n[image]" {
		t.Errorf("first prompt = %q", prompts[0].Text)
	}
	if prompts[0].PromptSource != "typed" {
		t.Errorf("interactive prompt source = %q, want typed", prompts[0].PromptSource)
	}
	for _, m := range p.msgs {
		for _, noise := range []string{"injectedagentsterm", "developersecretterm", "<environment_context>"} {
			if strings.Contains(m.Text, noise) {
				t.Errorf("%s message contains injected context %q", m.Kind, noise)
			}
		}
	}

	// 0.153 writes the prompt as an item_completed UserMessage.
	p2 := parseAll(t, s2)
	prompts = ofKind(p2.msgs, model.KindPrompt)
	if len(prompts) != 1 || prompts[0].Text != "Review the quokkacart checkout diff for security issues." {
		t.Fatalf("s2 prompts = %+v", prompts)
	}
	if prompts[0].PromptSource != "sdk" {
		t.Errorf("codex exec prompt source = %q, want sdk", prompts[0].PromptSource)
	}
}

func TestRepliesAndThinkingAreNotDuplicated(t *testing.T) {
	p := parseAll(t, s1)
	if n := len(ofKind(p.msgs, model.KindReply)); n != 3 {
		t.Errorf("s1 replies = %d, want 3 (agent_message events must be skipped)", n)
	}
	think := ofKind(p.msgs, model.KindThink)
	if len(think) != 1 || think[0].Text != "**Tracing the refund path**" {
		t.Errorf("s1 thinking = %+v", think)
	}
	p2 := parseAll(t, s2)
	if n := len(ofKind(p2.msgs, model.KindReply)); n != 1 {
		t.Errorf("s2 replies = %d, want 1 (item_completed AgentMessage must be skipped)", n)
	}
	if n := len(ofKind(p2.msgs, model.KindThink)); n != 0 {
		t.Errorf("s2 thinking = %d, want 0 (encrypted only)", n)
	}
}

func TestContextIsCarriedFromEarlierLines(t *testing.T) {
	p := parseAll(t, s1)
	for _, m := range p.msgs {
		if m.SessionID != s1 || m.AgentID != "" || m.CWD != "/home/alice/code/erp" || m.Branch != "feat-ledger" {
			t.Fatalf("message lacks context: %+v", m)
		}
	}
	replies := ofKind(p.msgs, model.KindReply)
	if replies[0].Model != "gpt-5.5" || replies[2].Model != "gpt-5.6-terra" {
		t.Errorf("reply models = %q, %q; want the model of each turn", replies[0].Model, replies[2].Model)
	}
	if ofKind(p.msgs, model.KindPrompt)[0].Model != "" {
		t.Error("prompts must not carry a model")
	}
}

func TestSubagentBelongsToParentSession(t *testing.T) {
	p := parseAll(t, s3)
	if len(p.msgs) != 2 {
		t.Fatalf("guardian messages = %d, want 2", len(p.msgs))
	}
	for _, m := range p.msgs {
		if m.SessionID != s1 || m.AgentID != s3 || m.Slug != "guardian" {
			t.Errorf("sub-agent message = session %q agent %q slug %q", m.SessionID, m.AgentID, m.Slug)
		}
	}
}

func TestResumeRecoversContext(t *testing.T) {
	path := fixturePath(t, s1)
	b, _ := os.ReadFile(path)
	off := int64(bytes.Index(b, []byte("[$lark-cli]")))
	off = int64(bytes.LastIndexByte(b[:off], '\n') + 1)
	p := parseFrom(t, NewWithHome(home), path, off)
	replies := ofKind(p.msgs, model.KindReply)
	if len(replies) != 1 {
		t.Fatalf("replies after resume = %d, want 1", len(replies))
	}
	r := replies[0]
	if r.SessionID != s1 || r.Branch != "feat-ledger" || r.Model != "gpt-5.6-terra" {
		t.Errorf("resumed reply = session %q branch %q model %q", r.SessionID, r.Branch, r.Model)
	}

	// A sub-agent's parent session is only in its first line.
	path = fixturePath(t, s3)
	b, _ = os.ReadFile(path)
	off = int64(bytes.IndexByte(b, '\n') + 1)
	for _, m := range parseFrom(t, NewWithHome(home), path, off).msgs {
		if m.SessionID != s1 || m.AgentID != s3 {
			t.Errorf("resumed sub-agent message = session %q agent %q", m.SessionID, m.AgentID)
		}
	}
}

func TestToolCallsAndResults(t *testing.T) {
	p := parseAll(t, s1)
	uses := ofKind(p.msgs, model.KindToolUse)
	var tools []string
	for _, u := range uses {
		tools = append(tools, u.Tool)
	}
	if got := strings.Join(tools, ","); got != "exec_command,exec_command,apply_patch,web_search" {
		t.Fatalf("tools = %s", got)
	}
	if !strings.Contains(uses[1].Text, "cmd=go test ./ledger/...") || uses[1].ToolUseID != "call_2" {
		t.Errorf("function_call = %+v", uses[1])
	}
	if uses[2].FilePath != "/home/alice/code/erp/ledger/refund.go" {
		t.Errorf("apply_patch file = %q", uses[2].FilePath)
	}
	if !strings.Contains(uses[3].Text, "net refund accounting zebraledger") {
		t.Errorf("web search = %q", uses[3].Text)
	}
	errs := map[string]bool{}
	for _, r := range ofKind(p.msgs, model.KindToolResult) {
		errs[r.ToolUseID] = r.IsError
	}
	want := map[string]bool{"call_1": false, "call_2": true, "call_3": false}
	for id, e := range want {
		if got, ok := errs[id]; !ok || got != e {
			t.Errorf("result %s error = %v (present %v), want %v", id, got, ok, e)
		}
	}
	p2 := parseAll(t, s2)
	res := ofKind(p2.msgs, model.KindToolResult)
	if len(res) != 1 || !res[0].IsError || !strings.Contains(res[0].Text, "fatal: bad revision quokkacart") {
		t.Errorf("custom tool result = %+v", res)
	}
}

func TestSkillInvocations(t *testing.T) {
	p := parseAll(t, s1)
	var inv []string
	for _, m := range p.msgs {
		if m.InvKind != "" {
			inv = append(inv, m.InvKind+":"+m.InvName+":"+m.InvArgs)
		}
	}
	want := []string{
		"skill:lark-cli:",
		"command:lark-cli:post the zebraledger fix to the team chat",
	}
	if strings.Join(inv, "|") != strings.Join(want, "|") {
		t.Errorf("invocations = %q, want %q", inv, want)
	}
}

func TestInterruptAndCompaction(t *testing.T) {
	meta := ofKind(parseAll(t, s1).msgs, model.KindMeta)
	if len(meta) != 1 || meta[0].Text != "[Request interrupted by user]" {
		t.Errorf("interrupt markers = %+v", meta)
	}
	sys := ofKind(parseAll(t, s2).msgs, model.KindSystem)
	if len(sys) != 1 || sys[0].Text != "[context compacted]" {
		t.Errorf("system messages = %+v", sys)
	}
}

func TestUsageAndTurns(t *testing.T) {
	p := parseAll(t, s1)
	ids := map[string]model.Usage{}
	for _, u := range p.usages {
		ids[u.RequestID] = u
	}
	if len(ids) != 2 {
		t.Fatalf("distinct requests = %d, want 2 (a repeated count shares its id)", len(ids))
	}
	u := ids[s1+":1050"]
	if u.Input != 400 || u.CacheRead != 600 || u.Output != 50 || u.Thinking != 10 || u.Model != "gpt-5.5" {
		t.Errorf("first request = %+v; cached tokens must be split out of input", u)
	}
	if len(p.turns) != 1 || p.turns[0].DurationMs != 9200 || p.turns[0].SessionID != s1 {
		t.Errorf("turns = %+v", p.turns)
	}

	p2 := parseAll(t, s2)
	if len(p2.usages) != 1 || p2.usages[0].Input != 4000 || p2.usages[0].Model != "gpt-5.6-terra" {
		t.Errorf("s2 usage = %+v; token_usage_record must not be counted again", p2.usages)
	}
	if len(p2.turns) != 1 || p2.turns[0].DurationMs != 4200 {
		t.Errorf("s2 turns = %+v", p2.turns)
	}
}

func TestTitles(t *testing.T) {
	p := parseFrom(t, NewWithHome(home), filepath.Join(home, "session_index.jsonl"), 0)
	if len(p.metas) != 3 {
		t.Fatalf("titles = %+v", p.metas)
	}
	last := p.metas[1]
	if last.SessionID != s1 || last.Title != "Fix zebraledger refund drift" || last.TitleRank != model.TitleRankAuto {
		t.Errorf("title = %+v", last)
	}
}

func TestMatchAndProject(t *testing.T) {
	src := NewWithHome("/h/.codex")
	cases := map[string]bool{
		"/h/.codex/sessions/2026/09/02/rollout-2026-09-02T10-00-00-" + s1 + ".jsonl": true,
		"/h/.codex/archived_sessions/rollout-2026-09-02T10-00-00-" + s1 + ".jsonl":   true,
		"/h/.codex/session_index.jsonl":                                              true,
		"/h/.codex/history.jsonl":                                                    false,
		"/h/.codex/sessions/2026/09/02/notes.jsonl":                                  false,
		"/h/.codex/.tmp/rollout-2026-09-02T10-00-00-" + s1 + ".jsonl":                false,
	}
	for p, want := range cases {
		if got := src.Match(p); got != want {
			t.Errorf("Match(%s) = %v, want %v", p, got, want)
		}
	}
	real := NewWithHome(home)
	if got := real.ProjectOf(fixturePath(t, s1)); got != "-home-alice-code-erp" {
		t.Errorf("ProjectOf = %q", got)
	}
	if got := real.ProjectOf(filepath.Join(home, "session_index.jsonl")); got != "" {
		t.Errorf("ProjectOf(index) = %q", got)
	}
}

func TestStatelessParseLineNeedsContext(t *testing.T) {
	line := []byte(`{"timestamp":"2026-09-02T02:00:03.000Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`)
	p, err := NewWithHome(home).ParseLine(line)
	if err != nil || len(p.Messages) != 0 {
		t.Errorf("ParseLine without session context = %+v, %v; want nothing", p, err)
	}
	if _, err := NewWithHome(home).ParseLine([]byte(`{not json`)); err == nil {
		t.Error("invalid JSON must be an error")
	}
}
