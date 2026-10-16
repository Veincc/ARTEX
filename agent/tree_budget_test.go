package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/subagent"
	actool "github.com/Autumn-27/norma/tool"
)

func TestTreeBudgetKillsDeepestFirst(t *testing.T) {
	budget := NewTreeBudget(1)
	hold := make(chan struct{})
	entered := make(chan struct{}, 4)
	inner := actool.Build(actool.Spec{
		Name:        "Agent",
		Description: "test agent",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			entered <- struct{}{}
			ch := subagent.ToolInterrupt(ctx)
			select {
			case <-ch:
				return actool.Text("preempted"), nil
			case <-hold:
				return actool.Text("done"), nil
			case <-ctx.Done():
				return actool.Text("cancelled"), nil
			}
		},
	})
	wrapped := budget.Wrap(inner, 3)

	deepOut := make(chan string, 1)
	go func() {
		res, err := wrapped.Call(subagent.WithDepth(context.Background(), 2), []byte(`{}`), &actool.ToolContext{})
		if err != nil {
			deepOut <- err.Error()
			return
		}
		deepOut <- res.Flatten()
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("deep run did not start")
	}

	shallowOut := make(chan string, 1)
	go func() {
		res, err := wrapped.Call(subagent.WithDepth(context.Background(), 1), []byte(`{}`), &actool.ToolContext{})
		if err != nil {
			shallowOut <- err.Error()
			return
		}
		shallowOut <- res.Flatten()
	}()
	select {
	case got := <-deepOut:
		if !strings.Contains(got, "preempted") {
			t.Fatalf("deepest run = %q, want preempted", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shallower request did not shed the deepest run")
	}

	// The survivor is the depth-1 run. A same-depth request must not kill it.
	refused, err := wrapped.Call(subagent.WithDepth(context.Background(), 1), []byte(`{}`), &actool.ToolContext{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(refused.Flatten(), "capacity") {
		t.Fatalf("same-depth request = %q, want capacity refusal", refused.Flatten())
	}
	select {
	case got := <-shallowOut:
		t.Fatalf("shallow run ended early: %s", got)
	default:
	}
	close(hold)
	select {
	case got := <-shallowOut:
		if !strings.Contains(got, "done") {
			t.Fatalf("shallow run = %q, want done", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("shallow run did not finish after release")
	}
}

func TestTreeBudgetAllowsParallelUnderCap(t *testing.T) {
	budget := NewTreeBudget(2)
	hold := make(chan struct{})
	entered := make(chan struct{}, 2)
	inner := actool.Build(actool.Spec{
		Name:   "Agent",
		Schema: map[string]any{"type": "object", "properties": map[string]any{}},
		Run: func(ctx context.Context, _ json.RawMessage, _ *actool.ToolContext) (actool.Result, error) {
			entered <- struct{}{}
			select {
			case <-subagent.ToolInterrupt(ctx):
				return actool.Text("preempted"), nil
			case <-hold:
				return actool.Text("done"), nil
			}
		},
	})
	wrapped := budget.Wrap(inner, 3)
	outs := make(chan string, 2)
	for i := 0; i < 2; i++ {
		go func() {
			res, err := wrapped.Call(subagent.WithDepth(context.Background(), 1), []byte(`{}`), &actool.ToolContext{})
			if err != nil {
				outs <- err.Error()
				return
			}
			outs <- res.Flatten()
		}()
	}
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatal("a slot under the cap did not start")
		}
	}
	close(hold)
	for i := 0; i < 2; i++ {
		select {
		case got := <-outs:
			if strings.Contains(got, "preempted") || strings.Contains(got, "capacity") {
				t.Fatalf("under-cap run was preempted: %s", got)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("under-cap run did not finish")
		}
	}
}
