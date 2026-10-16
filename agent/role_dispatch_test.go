package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

func TestIntentRoleRoundTrip(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("role-roundtrip", "round trip role", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(task.ID, true, true) })

	store := d.Exploration(task.ExplorationID)
	tools := NewToolSet(store, "planner")
	tools.SetTaskID(task.ID)
	add := tools.addIntent()

	res, err := add.Call(context.Background(), json.RawMessage(`{"summary":"map the login form","role":"Recon"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("create recon intent: %s", res.Flatten())
	}
	reconID := createdIntentID(t, res.Flatten())
	got := mustIntentPayload(t, store, reconID)
	if got["role"] != "recon" || got["summary"] != "map the login form" {
		t.Fatalf("recon payload = %#v", got)
	}

	res, err = add.Call(context.Background(), json.RawMessage(`{"intents":[{"summary":"confirm the bypass","role":"exploit"},{"summary":"generic sweep"}]}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("batch: %s", res.Flatten())
	}
	var batch struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.Unmarshal([]byte(res.Flatten()), &batch); err != nil || len(batch.IDs) != 2 || batch.IDs[0] == 0 || batch.IDs[1] == 0 {
		t.Fatalf("batch result: %s (%v)", res.Flatten(), err)
	}
	exploit := mustIntentPayload(t, store, batch.IDs[0])
	if exploit["role"] != "exploit" {
		t.Fatalf("exploit payload = %#v", exploit)
	}
	generic := mustIntentPayload(t, store, batch.IDs[1])
	if _, ok := generic["role"]; ok {
		t.Fatalf("role-less intent stored a role: %#v", generic)
	}
	if generic["summary"] != "generic sweep" {
		t.Fatalf("generic payload = %#v", generic)
	}

	res, err = add.Call(context.Background(), json.RawMessage(`{"summary":"nope","role":"root"}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(res.Flatten(), "role") {
		t.Fatalf("invalid role should be a tool error, got %q", res.Flatten())
	}
}

func createdIntentID(t *testing.T, text string) int64 {
	t.Helper()
	var id int64
	if _, err := fmt.Sscanf(text, "intent created: %d", &id); err != nil || id == 0 {
		t.Fatalf("intent id from %q: %v", text, err)
	}
	return id
}

func mustIntentPayload(t *testing.T, store *db.ExplorationStore, id int64) map[string]any {
	t.Helper()
	n, err := store.GetNode(id)
	if err != nil || n == nil || n.Kind != db.KindIntent {
		t.Fatalf("get intent %d: node=%v err=%v", id, n, err)
	}
	var p map[string]any
	if err := json.Unmarshal(n.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestAddIntentSchemaAcceptsRole(t *testing.T) {
	var tool actool.CoreTool
	for _, tl := range NewToolSet(nil, "planner").PlannerTools() {
		if tl.Name() == "add_intent" {
			tool = tl
			break
		}
	}
	if tool == nil {
		t.Fatal("planner tools missing add_intent")
	}
	props, _ := tool.InputSchema()["properties"].(map[string]any)
	role, _ := props["role"].(map[string]any)
	if role == nil {
		t.Fatal("add_intent schema missing role")
	}
	enum, _ := role["enum"].([]string)
	want := map[string]bool{roleRecon: true, roleExploit: true, roleValidate: true}
	if len(enum) != len(want) {
		t.Fatalf("role enum = %#v", enum)
	}
	for _, v := range enum {
		if !want[v] {
			t.Fatalf("unexpected role enum %q", v)
		}
	}
	if req, ok := tool.InputSchema()["required"].([]string); ok {
		for _, name := range req {
			if name == "role" {
				t.Fatal("role must stay optional")
			}
		}
	}
	desc := tool.Description()
	if !strings.Contains(desc, "recon") || !strings.Contains(desc, "留空") {
		t.Fatalf("add_intent description does not mention optional role: %s", desc)
	}
	intents, _ := props["intents"].(map[string]any)
	if !strings.Contains(fmtString(intents["description"]), "role") {
		t.Fatalf("batch intents schema does not mention role: %#v", intents["description"])
	}
}

func fmtString(v any) string {
	s, _ := v.(string)
	return s
}

func TestPlannerPromptSequencesRoles(t *testing.T) {
	for _, needle := range []string{
		"recon", "exploit", "validate", "role", "TodoWrite", "事实 id",
		"角色分派：开", "角色分派：关",
	} {
		if !strings.Contains(plannerDefaultTmpl, needle) {
			t.Errorf("planner prompt missing %q", needle)
		}
	}
	on := roleDispatchDirective(true)
	if !strings.Contains(on, "角色分派：开") || !strings.Contains(on, "recon") || !strings.Contains(on, "TodoWrite") {
		t.Fatalf("on directive = %s", on)
	}
	off := roleDispatchDirective(false)
	if !strings.Contains(off, "角色分派：关") || !strings.Contains(off, "不要填 role") {
		t.Fatalf("off directive = %s", off)
	}
	if p := NewPlanner(nil, "m", t.TempDir(), nil, 0, 0); p.roleDispatchEnabled() {
		t.Fatal("planner role dispatch defaults on")
	}
}

func TestSubagentDefsReuseRoleSpecs(t *testing.T) {
	defs := subagentDefs(9)
	specs := roleSpecs()
	if len(defs) != 3 || len(defs) != len(specs) {
		t.Fatalf("defs %d specs %d", len(defs), len(specs))
	}
	for name, spec := range specs {
		def, ok := defs[name]
		if !ok {
			t.Fatalf("missing subagent %s", name)
		}
		if len(def.Prompt) != 2 || def.Prompt[0] != spec.Lead {
			t.Fatalf("%s lead drifted: %q", name, def.Prompt)
		}
		if !strings.Contains(def.Prompt[1], "intent_id=9") {
			t.Fatalf("%s lost the nested intent pin", name)
		}
		if strings.Join(def.Tools, ",") != strings.Join(spec.Tools, ",") {
			t.Fatalf("%s tools drifted\n got %v\nwant %v", name, def.Tools, spec.Tools)
		}
	}
}

func TestWorkerRolePromptAndTools(t *testing.T) {
	base := workerBaseTools(NewToolSet(nil, "w"))
	generic := workerSystemForRole("", "127.0.0.1:1", "", "/data", "/run")
	if !strings.Contains(generic, "唯一职责") {
		t.Fatalf("empty role lost the generic worker prompt: %s", generic)
	}
	if strings.Contains(generic, "侦察子代理") {
		t.Fatal("generic prompt picked up the recon lead")
	}
	genericNames := toolNames(base)
	for _, name := range []string{"report_finding", "insert_assets", "Write", "search_all_worker_traces", "Bash"} {
		if !containsName(genericNames, name) {
			t.Fatalf("generic tool set missing %s", name)
		}
	}

	cases := []struct {
		role    string
		lead    string
		has     []string
		missing []string
	}{
		{roleRecon, "侦察子代理", []string{"Bash", "record_fact", "insert_assets", "list_assets", "search_all_worker_traces"}, []string{"report_finding", "Write", "Edit"}},
		{roleExploit, "利用确认子代理", []string{"Bash", "Write", "Edit", "record_fact", "report_finding", "insert_assets"}, []string{"search_all_worker_traces", "get_worker_trace"}},
		{roleValidate, "复测子代理", []string{"Bash", "record_fact", "report_finding", "list_findings"}, []string{"insert_assets", "Write", "list_assets"}},
	}
	for _, tc := range cases {
		prompt := workerSystemForRole(tc.role, "", "/ca.pem", "/data", "/run")
		if !strings.Contains(prompt, tc.lead) || !strings.Contains(prompt, "角色="+tc.role) {
			t.Fatalf("%s prompt = %s", tc.role, prompt)
		}
		if strings.Contains(prompt, "唯一职责") {
			t.Fatalf("%s prompt fell back to the generic worker", tc.role)
		}
		if !strings.Contains(prompt, "traffic_search") || !strings.Contains(prompt, "中间产物输出规约") {
			t.Fatalf("%s prompt dropped the code-owned tail", tc.role)
		}
		names := toolNames(filterToolsAllow(base, roleToolAllow(tc.role)))
		for _, name := range tc.has {
			if !containsName(names, name) {
				t.Errorf("%s missing %s in %v", tc.role, name, names)
			}
		}
		for _, name := range tc.missing {
			if containsName(names, name) {
				t.Errorf("%s should not include %s", tc.role, name)
			}
		}
	}

	if got := activeIntentRole(false, &db.Node{Payload: json.RawMessage(`{"role":"recon"}`)}); got != "" {
		t.Fatalf("flag off returned %q", got)
	}
	if got := activeIntentRole(true, &db.Node{Payload: json.RawMessage(`{"summary":"x"}`)}); got != "" {
		t.Fatalf("role-less returned %q", got)
	}
	if got := activeIntentRole(true, &db.Node{Payload: json.RawMessage(`{"role":"nope"}`)}); got != "" {
		t.Fatalf("unknown role returned %q", got)
	}
	if got := activeIntentRole(true, &db.Node{Payload: json.RawMessage(`{"role":"Validate"}`)}); got != roleValidate {
		t.Fatalf("normalize = %q", got)
	}
}

// roleCaptureProvider records the first completion's system prompt and tool
// names, then ends the turn.
type roleCaptureProvider struct {
	system string
	tools  []string
}

func (p *roleCaptureProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	if p.system == "" {
		p.system = strings.Join(req.System, "\n")
		for _, tl := range req.Tools {
			p.tools = append(p.tools, tl.Name)
		}
	}
	return streamEvents("role-done", "", "", "")
}

func (p *roleCaptureProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeFromStream{stream: p.Stream}.Complete(ctx, req)
}

func TestWorkerExecuteSelectsRole(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("role-execute", "select role", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(task.ID, true, true) })
	store := d.Exploration(task.ExplorationID)

	run := func(t *testing.T, enabled bool, payload string) (string, []string) {
		t.Helper()
		var body map[string]any
		if err := json.Unmarshal([]byte(payload), &body); err != nil {
			t.Fatal(err)
		}
		id, err := store.AddIntent(body, 1, nil, "planner")
		if err != nil {
			t.Fatal(err)
		}
		intent, err := store.GetNode(id)
		if err != nil || intent == nil {
			t.Fatal(err)
		}
		prov := &roleCaptureProvider{}
		w := NewWorker(prov, "m", t.TempDir(), nil, 0, 1)
		if enabled {
			w.ConfigureRoleDispatch(func() bool { return true })
		}
		if _, _, err := w.Execute(context.Background(), "work#1", task.ID, nil, store, intent, nil, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return prov.system, prov.tools
	}

	system, tools := run(t, true, `{"summary":"map login","role":"recon"}`)
	if !strings.Contains(system, "侦察子代理") || strings.Contains(system, "唯一职责") {
		t.Fatalf("recon execute prompt = %s", system)
	}
	if !containsName(tools, "insert_assets") || !containsName(tools, "record_fact") || containsName(tools, "report_finding") {
		t.Fatalf("recon execute tools = %v", tools)
	}

	system, tools = run(t, true, `{"summary":"confirm","role":"exploit"}`)
	if !strings.Contains(system, "利用确认子代理") || !containsName(tools, "report_finding") || !containsName(tools, "Write") || containsName(tools, "search_all_worker_traces") {
		t.Fatalf("exploit execute prompt/tools\n%s\n%v", system, tools)
	}

	system, tools = run(t, true, `{"summary":"retest","role":"validate"}`)
	if !strings.Contains(system, "复测子代理") || !containsName(tools, "report_finding") || containsName(tools, "insert_assets") || containsName(tools, "Write") {
		t.Fatalf("validate execute prompt/tools\n%s\n%v", system, tools)
	}

	system, tools = run(t, true, `{"summary":"do the whole thing"}`)
	if !strings.Contains(system, "唯一职责") || !containsName(tools, "report_finding") || !containsName(tools, "insert_assets") || !containsName(tools, "search_all_worker_traces") {
		t.Fatalf("role-less execute changed\n%s\n%v", system, tools)
	}

	system, tools = run(t, false, `{"summary":"map login","role":"recon"}`)
	if !strings.Contains(system, "唯一职责") || strings.Contains(system, "侦察子代理") || !containsName(tools, "report_finding") || !containsName(tools, "insert_assets") {
		t.Fatalf("flag off should stay generic\n%s\n%v", system, tools)
	}
}
