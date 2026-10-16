package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Autumn-27/artex/db"
	actool "github.com/Autumn-27/norma/tool"
)

// Intent roles. Empty means the historical generic worker: one agent does
// recon, exploit, and validate for the claimed intent. recon / exploit /
// validate specialize the same worker (prompt + tool allow-list) so the
// planner can chain them recon → exploit → validate across wake-ups.
const (
	roleRecon    = "recon"
	roleExploit  = "exploit"
	roleValidate = "validate"
)

// roleSpec is one specialized role, shared by a claimed intent (top-level
// worker) and by Option A nested subagents. Lead and Tools are the single
// source; subagentDefs only adds the nested-agent pin.
type roleSpec struct {
	Description string
	Lead        string
	Tools       []string
}

func roleSpecs() map[string]roleSpec {
	return map[string]roleSpec{
		roleRecon: {
			Description: "对当前意图里一个窄目标做隔离侦察，返回发现摘要。可写资产和事实，不报漏洞。",
			Lead:        "你是侦察子代理。只收集当前意图范围内的信息（指纹、入口、参数、已有流量），把新资产 insert_assets、结论 record_fact。不要报告漏洞，不要扩大到别的意图。",
			Tools: []string{
				"Bash", "Read", "LS", "sleep",
				"list_assets", "list_findings", "node_detail",
				"search_all_worker_traces", "get_worker_trace",
				"record_fact", "insert_assets",
				"traffic_search", "traffic_get", "traffic_blob",
				"Agent",
			},
		},
		roleExploit: {
			Description: "针对当前意图里一个已怀疑的点做触发确认，拿到证据后写回事实或漏洞。",
			Lead:        "你是利用确认子代理。只验证当前意图里已经点出的那一个怀疑点，用你真实执行得到的输出作证据。确认不了就 record_fact 记 inferred，不要把猜测写成 report_finding。",
			Tools: []string{
				"Bash", "Read", "Write", "Edit", "LS", "sleep",
				"record_fact", "report_finding", "insert_assets",
				"list_assets", "list_findings", "node_detail",
				"traffic_search", "traffic_get", "traffic_blob",
				"Agent",
			},
		},
		roleValidate: {
			Description: "复测一条候选发现是否可复现，返回结论。",
			Lead:        "你是复测子代理。只重放候选发现的复现步骤，判断这次能否再次观察到同一结果。复现成功用 report_finding（或在已有事实上补 record_fact）；复现失败用 record_fact 写明看到了什么。不要发明新的攻击面。",
			Tools: []string{
				"Bash", "Read", "LS", "sleep",
				"record_fact", "report_finding",
				"list_findings", "node_detail",
				"traffic_search", "traffic_get", "traffic_blob",
				"Agent",
			},
		},
	}
}

// normalizeIntentRole accepts recon|exploit|validate (case-insensitive) or
// blank. Anything else is an error so add_intent can tell the model to fix it.
func normalizeIntentRole(role string) (string, error) {
	role = strings.ToLower(strings.TrimSpace(role))
	if role == "" {
		return "", nil
	}
	switch role {
	case roleRecon, roleExploit, roleValidate:
		return role, nil
	default:
		return "", fmt.Errorf("role 必须是 recon、exploit、validate 之一，或留空表示通用执行者；收到 %q", role)
	}
}

// intentRole reads a stored intent payload. Blank, missing, or unrecognized
// values return "" so the worker stays on the generic path.
func intentRole(intent *db.Node) string {
	if intent == nil || len(intent.Payload) == 0 {
		return ""
	}
	var p struct {
		Role string `json:"role"`
	}
	if err := json.Unmarshal(intent.Payload, &p); err != nil {
		return ""
	}
	role, err := normalizeIntentRole(p.Role)
	if err != nil {
		return ""
	}
	return role
}

// activeIntentRole is the role execute should honor. Flag off (or a role-less
// / unrecognized payload) returns "" — the generic worker.
func activeIntentRole(enabled bool, intent *db.Node) string {
	if !enabled {
		return ""
	}
	return intentRole(intent)
}

func roleToolAllow(role string) []string {
	spec, ok := roleSpecs()[role]
	if !ok {
		return nil
	}
	return spec.Tools
}

// filterToolsAllow keeps tools whose names are on the allow-list, in the
// caller's order. Names that are not in the assembled set (Agent when nested
// subagents are off, traffic_* when capture is off) are simply absent.
func filterToolsAllow(tools []actool.CoreTool, allow []string) []actool.CoreTool {
	if len(allow) == 0 {
		return tools
	}
	ok := make(map[string]bool, len(allow))
	for _, name := range allow {
		ok[name] = true
	}
	out := make([]actool.CoreTool, 0, len(allow))
	for _, t := range tools {
		if ok[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// roleWorkerBody is the role-specific system-prompt body for a claimed intent.
// Empty when role is blank or unknown (caller falls back to the generic worker).
func roleWorkerBody(role string) string {
	spec, ok := roleSpecs()[role]
	if !ok {
		return ""
	}
	return spec.Lead + "\n\n你现在作为领到这条意图的执行者运行（角色=" + role +
		"），不是规划者，也不是把侦察、利用、复测一次做完的通用执行者。只做这个角色的事，做完即停。边发现边写回：新资产用 insert_assets，结论用 record_fact 并传 intent_id；只有本角色允许、且你本次真实触发拿到证据时才 report_finding。不要派新意图，不要扩大到别的意图。"
}

// workerSystemForRole selects the system prompt. role == "" is today's generic
// worker prompt (editable worker body + traffic note + artifact spec). A known
// role uses the lifted role lead instead of that generic body; the code-owned
// traffic note and artifact spec are still appended.
func workerSystemForRole(role, proxyAddr, caCert, dataDir, runDir string) string {
	body := roleWorkerBody(role)
	if body == "" {
		return workerSystem(proxyAddr, caCert, dataDir, runDir)
	}
	return body + workerTrafficBlock(caCert != "") + workerArtifactSpec(runDir)
}

// roleDispatchDirective is the code-owned mode line appended to the planner
// system prompt. Off (the default) tells the planner not to set role, so
// planning stays on generic intents. On tells it to chain recon → exploit →
// validate and to record the chain in the persistent TodoStore.
func roleDispatchDirective(enabled bool) string {
	if enabled {
		return "\n\n【角色分派：开】本轮必须按上面的 recon→exploit→validate 串行链派发：add_intent 填写 role（recon、exploit 或 validate）。开局先只派 recon；被 done/finding 唤醒且提示里带上该意图新产生的事实 id 后，再用 TodoWrite 推进清单、把下一步派出去（exploit，然后 validate），parent_ids 填这些事实或发现 id。不要把三步并行下发，也不要留空 role。"
	}
	return "\n\n【角色分派：关】本轮 add_intent 不要填 role。按通用意图派发（一条意图由通用执行者完成侦察、利用与复测），不要拆成 recon、exploit、validate 三条意图。"
}
