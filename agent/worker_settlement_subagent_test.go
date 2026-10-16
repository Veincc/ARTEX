package agent

import (
	"context"
	"iter"
	"strings"
	"sync"
	"testing"

	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestWorkerSettlementDisablesAgentOnlyWhenEnabled(t *testing.T) {
	off := workerSettlementDisabled(false)
	if len(off) != 1 || off[0] != "Bash" {
		t.Fatalf("flag off settlement = %v, want [Bash]", off)
	}
	on := workerSettlementDisabled(true)
	if len(on) != 2 || on[0] != "Bash" || on[1] != "Agent" {
		t.Fatalf("flag on settlement = %v, want [Bash Agent]", on)
	}
	w := NewWorker(nil, "m", t.TempDir(), nil, 0, 0)
	if w.subagentConfig() != nil {
		t.Fatal("unset resolver must be off")
	}
	w.ConfigureSubagents(func() *SubagentConfig { return nil })
	if w.subagentConfig() != nil {
		t.Fatal("nil resolver result must be off")
	}
	w.ConfigureSubagents(func() *SubagentConfig {
		return &SubagentConfig{MaxDepth: defaultSubagentMaxDepth, MaxParallel: defaultSubagentMaxParallel}
	})
	if w.subagentConfig() == nil {
		t.Fatal("expected enabled config")
	}
}

// schemaLogProvider returns an LS call on the first turn and a wrap-up line
// after that, recording the tool names advertised on every model call.
type schemaLogProvider struct {
	mu    sync.Mutex
	calls [][]string
}

func (p *schemaLogProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	names := make([]string, 0, len(req.Tools))
	for _, schema := range req.Tools {
		names = append(names, schema.Name)
	}
	p.mu.Lock()
	p.calls = append(p.calls, names)
	turn := len(p.calls)
	p.mu.Unlock()
	if turn == 1 {
		return streamEvents("", "ls1", "LS", `{}`)
	}
	return streamEvents("wrapped up", "", "", "")
}

func (p *schemaLogProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeFromStream{stream: p.Stream}.Complete(ctx, req)
}

func TestSubagentSettlementHidesBashAndAgent(t *testing.T) {
	prev := subagentTurnBudget["recon"]
	subagentTurnBudget["recon"] = 1
	t.Cleanup(func() { subagentTurnBudget["recon"] = prev })

	p := &schemaLogProvider{}
	w := NewWorker(p, "m", t.TempDir(), nil, 0, 0)
	cfg := &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	dir := t.TempDir()
	tsx := NewToolSet(nil, "w")
	ctx, tools, reg := w.attachSubagents(context.Background(), workerBaseTools(tsx), 4, nil, cfg)
	fillSubagentRegistry(reg, tools)
	agent, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("no Agent tool")
	}
	res, err := agent.Call(ctx, []byte(`{"agent_type":"recon","prompt":"list"}`), &actool.ToolContext{WorkingDir: dir})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if !strings.Contains(res.Flatten(), "wrapped up") {
		t.Fatalf("result %q, want settlement text", res.Flatten())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) < 2 {
		t.Fatalf("calls = %d, want an exploration turn and a settlement turn", len(p.calls))
	}
	if !containsName(p.calls[0], "Bash") || !containsName(p.calls[0], "Agent") {
		t.Fatalf("exploration turn lost Bash/Agent: %v", p.calls[0])
	}
	if containsName(p.calls[1], "Bash") || containsName(p.calls[1], "Agent") {
		t.Fatalf("settlement still advertised Bash/Agent: %v", p.calls[1])
	}
}
