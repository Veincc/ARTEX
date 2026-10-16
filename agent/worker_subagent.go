package agent

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/norma/harness"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/subagent"
	actool "github.com/Autumn-27/norma/tool"
)

// Depth accounting (Option A). norma treats a bare context as depth 0 and
// refuses an Agent call when depth >= MaxDepth, then runs the child at
// depth+1. The worker seeds depth 1 with subagent.WithDepth before its
// session, and the default MaxDepth is 3, so:
//
//	worker (depth 1) may spawn
//	level 2 may spawn
//	level 3's Agent call is refused (3 >= 3)
//
// That is the worker plus two further levels. Seeding here, rather than
// shrinking MaxDepth, keeps norma's "3" meaning a three-deep tree whose root
// is the worker.

const (
	defaultSubagentMaxDepth    = 3
	defaultSubagentMaxParallel = 4
	// subagentSettleTurns is the wrap-up budget for a subagent that hits its
	// own turn cap or is shed by the tree budget. Short on purpose: the turn
	// can only write back (Bash and Agent are hidden).
	subagentSettleTurns = 2
)

// subagentTurnBudget is the per-role exploration cap (tool-turns). Tests may
// lower a role to force settlement; production defaults are recon/exploit 16,
// validate 12.
var subagentTurnBudget = map[string]int{
	"recon": 16, "exploit": 16, "validate": 12,
}

// SubagentConfig is the per-run wiring for worker-rooted norma subagents.
// A nil config (the resolver returns nil, or none is installed) leaves the
// worker on the flag-off path: no Agent tool, no depth seed, no tree budget,
// and settlement still hides only Bash.
type SubagentConfig struct {
	MaxDepth    int
	MaxParallel int
	// CanUseTool is the worker's permission callback. ARTEX normally leaves
	// this nil and uses permission.ModeBypass plus Guard hooks.
	CanUseTool permission.CanUseTool
}

// ConfigureSubagents installs a per-run resolver. nil, or a resolver that
// returns nil, disables subagents for that run. Read inside execute so a
// settings toggle applies to the next intent without rebuilding the worker.
func (w *Worker) ConfigureSubagents(fn func() *SubagentConfig) { w.subagents = fn }

func (w *Worker) subagentConfig() *SubagentConfig {
	if w.subagents == nil {
		return nil
	}
	return w.subagents()
}

// workerSettlementDisabled is the tool list hidden during SDK wrap-up.
// Subagents off → Bash only, the historical worker policy. Subagents on →
// Bash and Agent, so a settling worker cannot spawn another branch.
func workerSettlementDisabled(subagentsOn bool) []string {
	if subagentsOn {
		return []string{"Bash", "Agent"}
	}
	return []string{"Bash"}
}

// attachSubagents appends the Agent tool and seeds depth 1 when sc is non-nil.
// The returned registry is the parent tool set subagents filter against; the
// caller fills it with the post-augmentation tool list (the same instances the
// worker runs, including this Agent tool) so record_fact / report_finding /
// insert_assets stay on the worker's ToolSet. sc == nil returns ctx and base
// unchanged and a nil registry.
func (w *Worker) attachSubagents(ctx context.Context, base []actool.CoreTool, intentID int64, hooks harness.HookRunner, sc *SubagentConfig) (context.Context, []actool.CoreTool, *actool.Registry) {
	if sc == nil {
		return ctx, base, nil
	}
	depth := sc.MaxDepth
	if depth <= 0 {
		depth = defaultSubagentMaxDepth
	}
	parallel := sc.MaxParallel
	if parallel <= 0 {
		parallel = defaultSubagentMaxParallel
	}
	reg := actool.NewRegistry()
	cfg := subagent.Config{
		Provider:    w.prov,
		ParentTools: reg,
		Agents:      subagentDefs(intentID),
		MaxDepth:    depth,
		CanUseTool:  sc.CanUseTool,
		Hooks:       hooks,
		Transcript:  w.tx,
		Settlement: &harness.Settlement{
			Prompt:        settleWrapUpPrompt,
			DisabledTools: []string{"Bash", "Agent"},
			MaxTurns:      subagentSettleTurns,
		},
	}
	agentTool := NewTreeBudget(parallel).Wrap(pinIntentPrompt(subagent.NewTool(cfg), intentID), depth)
	out := make([]actool.CoreTool, len(base), len(base)+1)
	copy(out, base)
	out = append(out, agentTool)
	return subagent.WithDepth(ctx, 1), out, reg
}

// fillSubagentRegistry publishes the worker's live tools, including Agent, to
// the registry nested spawns filter against. No-op when subagents are off.
func fillSubagentRegistry(reg *actool.Registry, tools []actool.CoreTool) {
	if reg == nil {
		return
	}
	for _, t := range tools {
		reg.Add(t)
	}
}

// pinIntentPrompt prepends the worker's intent id onto every Agent dispatch so
// a nested model writes record_fact / report_finding back to that intent even
// when the parent prompt omits it. The system prompt carries the same pin.
type intentPin struct {
	actool.CoreTool
	intentID int64
}

func pinIntentPrompt(inner actool.CoreTool, intentID int64) actool.CoreTool {
	return &intentPin{CoreTool: inner, intentID: intentID}
}

func (p *intentPin) Call(ctx context.Context, in json.RawMessage, tc *actool.ToolContext) (actool.Result, error) {
	var body struct {
		AgentType string `json:"agent_type"`
		Prompt    string `json:"prompt"`
	}
	if err := json.Unmarshal(in, &body); err != nil {
		return p.CoreTool.Call(ctx, in, tc)
	}
	body.Prompt = fmt.Sprintf("【intent_id=%d】写回 record_fact / report_finding 时必须传 intent_id=%d，归属当前这条意图，不要改成别的 id。insert_assets 登记的资产也属于这条意图。\n\n%s", p.intentID, p.intentID, body.Prompt)
	raw, err := json.Marshal(body)
	if err != nil {
		return p.CoreTool.Call(ctx, in, tc)
	}
	return p.CoreTool.Call(ctx, raw, tc)
}

func subagentDefs(intentID int64) map[string]subagent.Definition {
	id := fmt.Sprintf("%d", intentID)
	common := "你在授权渗透测试里执行【当前意图】的一个子任务，不是新的规划者。intent_id 固定为 " + id +
		"。写回规则与执行者相同：新资产用 insert_assets；探索结论用 record_fact 并传 intent_id=" + id +
		"；只有你本次真实触发、拿到可复现证据时才用 report_finding，并传同一个 intent_id。" +
		"不要动手做这条意图以外的事。需要把一块独立子任务隔离开时，可以调用 Agent 再委派一层（recon / exploit / validate）；层数有上限，被拒绝就自己做完并写回。" +
		"边做边写回，不要攒到最后。"
	specs := roleSpecs()
	out := make(map[string]subagent.Definition, len(specs))
	for name, spec := range specs {
		out[name] = subagent.Definition{
			Description:    spec.Description,
			Prompt:         []string{spec.Lead, common},
			Tools:          append([]string{}, spec.Tools...),
			PermissionMode: permission.ModeBypass,
			MaxTurns:       subagentTurnBudget[name],
		}
	}
	return out
}
