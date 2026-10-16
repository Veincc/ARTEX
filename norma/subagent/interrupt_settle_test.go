package subagent

import (
	"context"
	"encoding/json"
	"iter"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// scriptedProvider returns one tool call on the first turn of a run and a
// fixed wrap-up line afterwards, recording the tool names advertised on the
// wrap-up turn.
type settleProvider struct {
	mu        sync.Mutex
	turns     int
	wrapTools []string
}

func (p *settleProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	p.mu.Lock()
	p.turns++
	turn := p.turns
	if turn > 1 {
		for _, schema := range req.Tools {
			p.wrapTools = append(p.wrapTools, schema.Name)
		}
	}
	p.mu.Unlock()
	return func(yield func(llm.StreamEvent, error) bool) {
		if turn == 1 {
			yield(llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: "hold1", ToolName: "Hold"}, nil)
			yield(llm.StreamEvent{Type: llm.SEToolInputJSON, Text: `{}`}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "tool_use"}, nil)
			yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
			return
		}
		yield(llm.StreamEvent{Type: llm.SETextDelta, Text: "wrapped up"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: "end_turn"}, nil)
		yield(llm.StreamEvent{Type: llm.SEMessageStop}, nil)
	}
}

func (p *settleProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	acc := llm.NewAccumulator()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		acc.Add(ev)
	}
	return acc.Message(), acc.StopReason, acc.Usage, nil
}

// TestShedEntersSettlementWithoutParentCancel locks the tree-budget shed path:
// closing the tool-interrupt channel aborts the in-flight tool and runs the
// settlement turn with DisabledTools hidden, while the parent context stays live.
func TestShedEntersSettlementWithoutParentCancel(t *testing.T) {
	started := make(chan struct{})
	var once sync.Once
	hold := tool.Build(tool.Spec{
		Name:        "Hold",
		Description: "blocks until its context is cancelled",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			once.Do(func() { close(started) })
			<-ctx.Done()
			return tool.Text("released"), nil
		},
	})
	prov := &settleProvider{}
	agentTool := NewTool(Config{
		Provider:    prov,
		ParentTools: tool.NewRegistry(hold),
		MaxDepth:    3,
		Agents: map[string]Definition{
			"recon": {
				Prompt:         []string{"recon"},
				Tools:          []string{"Hold"},
				PermissionMode: permission.ModeBypass,
				MaxTurns:       8,
			},
		},
		Settlement: &harness.Settlement{
			Prompt:        "wrap up now",
			DisabledTools: []string{"Hold"},
			MaxTurns:      2,
		},
	})
	ctx, shed := WithToolInterrupt(WithDepth(context.Background(), 1))
	var (
		res tool.Result
		err error
	)
	done := make(chan struct{})
	go func() {
		res, err = agentTool.Call(ctx, []byte(`{"agent_type":"recon","prompt":"hold"}`), &tool.ToolContext{})
		close(done)
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("Hold did not start")
	}
	if ctx.Err() != nil {
		t.Fatal("parent context cancelled before shed")
	}
	shed()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("shed did not finish the subagent")
	}
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if ctx.Err() != nil {
		t.Fatal("shed cancelled the parent context")
	}
	if !strings.Contains(res.Flatten(), "wrapped up") {
		t.Fatalf("result %q, want settlement text", res.Flatten())
	}
	prov.mu.Lock()
	defer prov.mu.Unlock()
	for _, name := range prov.wrapTools {
		if name == "Hold" {
			t.Fatalf("settlement advertised disabled tool Hold: %v", prov.wrapTools)
		}
	}
	if prov.turns < 2 {
		t.Fatalf("turns = %d, want a settlement turn", prov.turns)
	}
}
