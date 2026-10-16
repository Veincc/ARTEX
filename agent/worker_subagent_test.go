package agent

import (
	"context"
	"encoding/json"
	"iter"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/artex/guard"
	"github.com/Autumn-27/norma/hook"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/subagent"
	actool "github.com/Autumn-27/norma/tool"
	"github.com/Autumn-27/norma/transcript"
)

func toolNames(tools []actool.CoreTool) []string {
	out := make([]string, 0, len(tools))
	for _, t := range tools {
		out = append(out, t.Name())
	}
	return out
}

func containsName(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

func workerBaseTools(tsx *ToolSet) []actool.CoreTool {
	return append(tsx.WorkerTools(), defaultToolsExcept("MultiEdit", "Glob", "Grep")...)
}

// streamEvents yields a single assistant turn: a tool call when toolName is
// set, otherwise a text end-turn.
func streamEvents(text, toolID, toolName, toolInput string) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		if toolName != "" {
			yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: toolID, ToolName: toolName}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: toolInput}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			return
		}
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: text}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

type completeFromStream struct {
	stream func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error]
}

func (c completeFromStream) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return c.stream(ctx, req)
}

func (c completeFromStream) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range c.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

func messageBlob(m llm.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		b.WriteString(c.Text)
		for _, inner := range c.Content {
			b.WriteString(inner.Text)
		}
	}
	return b.String()
}

func TestSubagentInjectionOffIsUnchanged(t *testing.T) {
	w := NewWorker(nil, "m", t.TempDir(), nil, 0, 0)
	tsx := NewToolSet(nil, "w")
	base := workerBaseTools(tsx)
	ctx, tools, reg := w.attachSubagents(context.Background(), base, 7, nil, nil)
	if reg != nil {
		t.Fatal("flag off must not allocate a subagent registry")
	}
	if subagent.Depth(ctx) != 0 {
		t.Fatalf("flag off must not seed depth, got %d", subagent.Depth(ctx))
	}
	if containsName(toolNames(tools), "Agent") {
		t.Fatal("flag off exposed Agent")
	}
	if got := workerSettlementDisabled(false); len(got) != 1 || got[0] != "Bash" {
		t.Fatalf("flag off settlement tools = %v", got)
	}
}

func TestSubagentInjectionAddsAgent(t *testing.T) {
	w := NewWorker(nil, "m", t.TempDir(), nil, 0, 0)
	w.ConfigureSubagents(func() *SubagentConfig {
		return &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	})
	tsx := NewToolSet(nil, "w")
	tsx.SetOwnerNode(7)
	ctx, tools, reg := w.attachSubagents(context.Background(), workerBaseTools(tsx), 7, nil, w.subagentConfig())
	if reg == nil {
		t.Fatal("expected parent registry")
	}
	if subagent.Depth(ctx) != 1 {
		t.Fatalf("worker depth = %d, want 1", subagent.Depth(ctx))
	}
	fillSubagentRegistry(reg, tools)
	agent, ok := reg.Get("Agent")
	if !ok || agent.Name() != "Agent" {
		t.Fatal("registry missing Agent after injection")
	}
	if !containsName(toolNames(tools), "Agent") {
		t.Fatal("worker tool list missing Agent")
	}
	for role, def := range subagentDefs(7) {
		if !containsName(def.Tools, "Agent") {
			t.Fatalf("%s allow-list dropped Agent", role)
		}
		if !strings.Contains(strings.Join(def.Prompt, "\n"), "7") {
			t.Fatalf("%s prompt missing intent id", role)
		}
	}
}

// nestingProvider spawns Agent once per subagent run, then stops. It records
// whether a child saw the depth guard (Agent still in the filter) or an
// unknown-tool error (Agent filtered out).
type nestingProvider struct {
	mu         sync.Mutex
	sawDepth   bool
	sawUnknown bool
	spawns     int
}

func (p *nestingProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	var blob string
	for _, m := range req.Messages {
		blob += messageBlob(m)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(blob, "recursion depth") {
		p.sawDepth = true
		return streamEvents("depth-blocked", "", "", "")
	}
	if strings.Contains(blob, "unknown tool") {
		p.sawUnknown = true
		return streamEvents("missing-agent", "", "", "")
	}
	if !strings.Contains(blob, "tool_result") && !strings.Contains(blob, "depth-blocked") && !strings.Contains(blob, "child-ok") {
		// First turn of a run has only the seed user text.
		hasResult := false
		for _, m := range req.Messages {
			for _, c := range m.Content {
				if c.Type == llm.BlockToolResult {
					hasResult = true
				}
			}
		}
		if !hasResult {
			p.spawns++
			return streamEvents("", "nest1", "Agent", `{"agent_type":"recon","prompt":"deeper"}`)
		}
	}
	return streamEvents("child-ok", "", "", "")
}

func (p *nestingProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeFromStream{stream: p.Stream}.Complete(ctx, req)
}

func TestChildFilterKeepsAgentUntilDepthCap(t *testing.T) {
	p := &nestingProvider{}
	w := NewWorker(p, "m", t.TempDir(), nil, 0, 0)
	cfg := &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	w.ConfigureSubagents(func() *SubagentConfig { return cfg })
	tsx := NewToolSet(nil, "w")
	ctx, tools, reg := w.attachSubagents(context.Background(), workerBaseTools(tsx), 11, nil, cfg)
	fillSubagentRegistry(reg, tools)
	agent, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("no Agent tool")
	}
	res, err := agent.Call(ctx, []byte(`{"agent_type":"recon","prompt":"start"}`), &actool.ToolContext{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("top-level spawn failed: %s", res.Flatten())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.sawUnknown {
		t.Fatal("child filter dropped Agent (unknown tool)")
	}
	if !p.sawDepth {
		t.Fatalf("depth guard did not fire; spawns=%d result=%s", p.spawns, res.Flatten())
	}
	if p.spawns < 2 {
		t.Fatalf("spawns=%d, want the two levels under the worker to both attempt Agent", p.spawns)
	}
}

func TestGuardBlocksNestedBash(t *testing.T) {
	var executed bool
	fakeBash := actool.Build(actool.Spec{
		Name:        "Bash",
		Description: "fake bash",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string"},
			},
			"required": []any{"command"},
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(context.Context, json.RawMessage, *actool.ToolContext) (actool.Result, error) {
			executed = true
			return actool.Text("ran"), nil
		},
	})
	g := guard.New()
	g.Hooks().On(hook.PreToolUse, func(_ context.Context, ev hook.Event) hook.Result {
		if ev.ToolName == "Bash" {
			return hook.Result{Decision: "block", Message: "denied by test policy"}
		}
		return hook.Result{}
	})
	var started []string
	g.Hooks().On(hook.SubagentStart, func(_ context.Context, ev hook.Event) hook.Result {
		started = append(started, ev.AgentType)
		return hook.Result{}
	})

	prov := &bashThenStop{}
	w := NewWorker(prov, "m", t.TempDir(), nil, 0, 0)
	cfg := &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	tsx := NewToolSet(nil, "w")
	base := append(tsx.WorkerTools(), fakeBash)
	ctx, tools, reg := w.attachSubagents(context.Background(), base, 3, g.Hooks(), cfg)
	fillSubagentRegistry(reg, tools)
	agent, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("no Agent tool")
	}
	// ctx is already depth 1 (the worker). The exploit subagent is the first
	// nested level and must still pass through the worker's Guard.
	res, err := agent.Call(ctx, []byte(`{"agent_type":"exploit","prompt":"confirm"}`), &actool.ToolContext{WorkingDir: t.TempDir()})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if executed {
		t.Fatal("Guard did not block nested Bash; the command ran")
	}
	if !strings.Contains(res.Flatten(), "bash-blocked") {
		t.Fatalf("result %q, want bash-blocked", res.Flatten())
	}
	if len(started) == 0 {
		t.Fatal("Guard SubagentStart hook did not fire")
	}
}

type bashThenStop struct {
	mu sync.Mutex
	n  int
}

func (p *bashThenStop) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	var blob string
	for _, m := range req.Messages {
		blob += messageBlob(m)
	}
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	if strings.Contains(blob, "Blocked by hook") {
		return streamEvents("bash-blocked", "", "", "")
	}
	hasResult := false
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if c.Type == llm.BlockToolResult {
				hasResult = true
			}
		}
	}
	if !hasResult {
		return streamEvents("", "bash1", "Bash", `{"command":"echo hi"}`)
	}
	return streamEvents("unexpected:"+blob, "", "", "")
}

func (p *bashThenStop) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeFromStream{stream: p.Stream}.Complete(ctx, req)
}

func TestSubagentTranscriptNestsUnderWorkerSession(t *testing.T) {
	dir := t.TempDir()
	tx := transcript.NewStore(dir)
	prov := completeFromStream{stream: func(context.Context, llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		return streamEvents("sidechain-ok", "", "", "")
	}}
	w := NewWorker(prov, "m", dir, tx, 0, 0)
	cfg := &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	tsx := NewToolSet(nil, "w")
	ctx, tools, reg := w.attachSubagents(context.Background(), workerBaseTools(tsx), 9, nil, cfg)
	fillSubagentRegistry(reg, tools)
	agent, _ := reg.Get("Agent")
	sid := WorkerSessionID(1, 9)
	ctx = transcript.WithSessionID(ctx, sid)
	res, err := agent.Call(ctx, []byte(`{"agent_type":"recon","prompt":"look"}`), &actool.ToolContext{WorkingDir: dir})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError || !strings.Contains(res.Flatten(), "sidechain-ok") {
		t.Fatalf("result: %s", res.Flatten())
	}
	matches, err := filepath.Glob(filepath.Join(dir, sid, "subagents", "agent-*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatalf("no sidechain under session %s", sid)
	}
	body, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), sid) {
		t.Fatalf("sidechain file missing parent session id: %s", matches[0])
	}
}
