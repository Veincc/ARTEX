package agent

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"sync"

	"github.com/Autumn-27/norma/subagent"
	actool "github.com/Autumn-27/norma/tool"
)

// errTreeFull is returned by TreeBudget.acquire when every slot is held by a
// run at least as shallow as the incoming request. A strictly deeper run is
// shed instead (kill-deepest-first) so a root-level spawn is not stuck behind
// a leaf.
var errTreeFull = errors.New("subagent tree at capacity")

// TreeBudget caps concurrent subagent runs across one worker's whole intent
// tree. It is independent of the engine worker pool: one budget per execute,
// shared by the worker's Agent tool and every nested spawn.
//
// Slot depth is subagent.Depth(ctx) at the Agent call (the caller's depth).
// The worker seeds depth 1, so a spawn from the worker is shallower than a
// spawn from a nested subagent. When the tree is full, a shallower request
// cancels the deepest active run's tool-interrupt (settlement, not a parent
// cancel) and takes its slot. A same-or-deeper request is refused immediately
// so an ancestor blocked inside Agent cannot deadlock waiting on its child.
type TreeBudget struct {
	max    int
	mu     sync.Mutex
	active []*budgetSlot
}

type budgetSlot struct {
	depth   int
	shed    func()
	dropped bool
}

// NewTreeBudget returns a budget of maxParallel slots. Non-positive values use
// defaultSubagentMaxParallel (4).
func NewTreeBudget(maxParallel int) *TreeBudget {
	if maxParallel <= 0 {
		maxParallel = defaultSubagentMaxParallel
	}
	return &TreeBudget{max: maxParallel}
}

// Wrap decorates the Agent tool so every dispatch, including nested ones,
// takes a slot. The wrapped tool stays concurrency-safe so one turn can fan
// out several children; this budget is what bounds that fan-out.
//
// A call that is already at maxDepth is forwarded without taking a slot, so
// the depth guard's refusal does not shed a live branch.
func (b *TreeBudget) Wrap(inner actool.CoreTool, maxDepth int) actool.CoreTool {
	if b == nil {
		return inner
	}
	return &budgetTool{CoreTool: inner, budget: b, maxDepth: maxDepth}
}

type budgetTool struct {
	actool.CoreTool
	budget   *TreeBudget
	maxDepth int
}

// IsConcurrencySafe lets the harness run several Agent calls from one turn in
// parallel. TreeBudget, not the harness scheduler, is the tree-wide cap.
func (t *budgetTool) IsConcurrencySafe(json.RawMessage) bool { return true }

func (t *budgetTool) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	if t.maxDepth > 0 && subagent.Depth(ctx) >= t.maxDepth {
		return t.CoreTool.Call(ctx, in, tc)
	}
	release, runCtx, err := t.budget.acquire(ctx)
	if err != nil {
		return actool.Errorf("Error: subagent tree at capacity (" + strconv.Itoa(t.budget.max) + "); a same-or-deeper branch was not preempted. Retry after a branch finishes."), nil
	}
	defer release()
	return t.CoreTool.Call(runCtx, in, tc)
}

func (b *TreeBudget) acquire(ctx context.Context) (release func(), runCtx context.Context, err error) {
	depth := subagent.Depth(ctx)
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.active) >= b.max {
		idx := -1
		deepest := depth
		for i, slot := range b.active {
			if slot.depth > deepest {
				deepest = slot.depth
				idx = i
			} else if idx >= 0 && slot.depth == deepest {
				idx = i // newest leaf at that depth
			}
		}
		if idx < 0 {
			return nil, nil, errTreeFull
		}
		victim := b.active[idx]
		b.active = append(b.active[:idx], b.active[idx+1:]...)
		victim.dropped = true
		if victim.shed != nil {
			victim.shed()
		}
	}
	runCtx, shed := subagent.WithToolInterrupt(ctx)
	slot := &budgetSlot{depth: depth, shed: shed}
	b.active = append(b.active, slot)
	release = func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if slot.dropped {
			return
		}
		slot.dropped = true
		for i, cur := range b.active {
			if cur == slot {
				b.active = append(b.active[:i], b.active[i+1:]...)
				return
			}
		}
	}
	return release, runCtx, nil
}
