package server

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
)

type subagentInner struct {
	starts []string
	bash   []string
}

func (s *subagentInner) PreToolUse(_ context.Context, name string, input []byte) (bool, string, []byte) {
	if name == "Bash" {
		s.bash = append(s.bash, string(input))
		return true, "denied by guard", nil
	}
	return false, "", nil
}
func (s *subagentInner) PostToolUse(context.Context, string, []byte, []byte, bool) {}
func (s *subagentInner) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}
func (s *subagentInner) FireSubagentStart(_ context.Context, agentType string) {
	s.starts = append(s.starts, agentType)
}

func TestSteerHooksFireSubagentStartDelegates(t *testing.T) {
	inner := &subagentInner{}
	var got db.Activity
	h := steerHooks{
		inner:  inner,
		drain:  func() (string, bool) { return "", false },
		nudges: &atomic.Int64{},
		limit:  defaultEmptyTurnNudges,
		label:  "work#1 · #9",
		emit: func(a db.Activity) {
			got = a
		},
	}
	h.FireSubagentStart(context.Background(), "recon")
	if len(inner.starts) != 1 || inner.starts[0] != "recon" {
		t.Fatalf("inner starts = %v", inner.starts)
	}
	if got.Kind != "subagent_start" || got.Tool != "Agent" {
		t.Fatalf("activity = %+v", got)
	}
	if got.Summary == "" {
		t.Fatal("empty subagent-start summary")
	}

	block, msg, _ := h.PreToolUse(context.Background(), "Bash", []byte(`{"command":"id"}`))
	if !block || msg == "" {
		t.Fatalf("nested Bash was not denied by the inner guard: block=%v msg=%q", block, msg)
	}
	if len(inner.bash) != 1 {
		t.Fatalf("inner saw %d Bash calls", len(inner.bash))
	}
}
