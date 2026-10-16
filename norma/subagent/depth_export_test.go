package subagent

import (
	"context"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// TestWithDepthBlocksThirdNestedLevel locks the Option A depth contract:
// a host seeds the root at depth 1 and MaxDepth is 3, so the root (depth 1)
// and the next level (depth 2) may spawn, and an Agent call at depth 3 — the
// third nested level — is refused.
func TestWithDepthBlocksThirdNestedLevel(t *testing.T) {
	if Depth(context.Background()) != 0 {
		t.Fatal("bare context must be depth 0")
	}
	if got := Depth(WithDepth(context.Background(), 1)); got != 1 {
		t.Fatalf("Depth(WithDepth(1)) = %d, want 1", got)
	}

	agentTool := NewTool(Config{
		Provider: fakeProvider{reply: "ok"},
		MaxDepth: 3,
		Agents: map[string]Definition{
			"recon": {Prompt: []string{"p"}, PermissionMode: permission.ModeBypass},
		},
	})
	call := func(depth int) tool.Result {
		t.Helper()
		res, err := agentTool.Call(WithDepth(context.Background(), depth), []byte(`{"agent_type":"recon","prompt":"x"}`), &tool.ToolContext{})
		if err != nil {
			t.Fatalf("depth %d: %v", depth, err)
		}
		return res
	}
	if res := call(1); res.IsError {
		t.Fatalf("depth 1 (worker root) should spawn, got %q", res.Flatten())
	}
	if res := call(2); res.IsError {
		t.Fatalf("depth 2 (first nested level) should spawn, got %q", res.Flatten())
	}
	res := call(3)
	if !res.IsError || !strings.Contains(res.Flatten(), "recursion depth") {
		t.Fatalf("depth 3 (third nested level) should be refused, got %q", res.Flatten())
	}
}
