package agent

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"testing"

	"github.com/Autumn-27/artex/db"
	"github.com/Autumn-27/norma/llm"
	actool "github.com/Autumn-27/norma/tool"
)

// factProvider's first turn writes record_fact with the pinned intent id and
// records whether the system prompt named that intent. The second turn stops.
type factProvider struct {
	intentID int64
	system   string
	prompt   string
}

func (p *factProvider) Stream(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	hasResult := false
	var blob string
	for _, m := range req.Messages {
		blob += messageBlob(m)
		for _, c := range m.Content {
			if c.Type == llm.BlockToolResult {
				hasResult = true
			}
		}
	}
	if !hasResult {
		p.system = strings.Join(req.System, "\n")
		p.prompt = blob
		input := fmt.Sprintf(`{"summary":"banner observed","intent_id":%d,"evidence":"curl /","confidence":"observed"}`, p.intentID)
		return streamEvents("", "fact1", "record_fact", input)
	}
	return streamEvents("fact-written", "", "", "")
}

func (p *factProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	return completeFromStream{stream: p.Stream}.Complete(ctx, req)
}

func TestSubagentRecordFactUsesWorkerToolSet(t *testing.T) {
	d := testDB(t)
	defer d.Close()
	task, err := d.CreateTask("subagent-attr", "attr", nil, 0, 0)
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	t.Cleanup(func() { _, _ = d.DeleteTaskCascade(task.ID, true, true) })

	store := d.Exploration(task.ExplorationID)
	intentID, err := store.AddNode(db.KindIntent, map[string]any{"summary": "probe one host"}, 1, "running", "planner", nil)
	if err != nil {
		t.Fatalf("add intent: %v", err)
	}

	prov := &factProvider{intentID: intentID}
	w := NewWorker(prov, "m", t.TempDir(), nil, 0, 0)
	cfg := &SubagentConfig{MaxDepth: 3, MaxParallel: 4}
	tsx := NewToolSet(store, "work#1")
	tsx.SetTaskID(task.ID)
	tsx.SetOwnerNode(intentID)
	ctx, tools, reg := w.attachSubagents(context.Background(), workerBaseTools(tsx), intentID, nil, cfg)
	fillSubagentRegistry(reg, tools)
	agent, ok := reg.Get("Agent")
	if !ok {
		t.Fatal("no Agent tool")
	}
	// The child registry's record_fact must be the worker ToolSet's tool, not a copy.
	fact, ok := reg.Get("record_fact")
	if !ok {
		t.Fatal("parent registry missing record_fact")
	}
	workerFact := tsx.recordFact()
	if fact.Name() != workerFact.Name() {
		t.Fatalf("record_fact name %s", fact.Name())
	}

	res, err := agent.Call(ctx, []byte(`{"agent_type":"recon","prompt":"note the banner"}`), &actool.ToolContext{})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.IsError {
		t.Fatalf("subagent error: %s", res.Flatten())
	}
	if !strings.Contains(prov.system, fmt.Sprintf("%d", intentID)) {
		t.Fatalf("system prompt did not pin intent %d: %s", intentID, prov.system)
	}
	if !strings.Contains(prov.prompt, fmt.Sprintf("intent_id=%d", intentID)) {
		t.Fatalf("seed prompt did not pin intent %d: %s", intentID, prov.prompt)
	}
	if got := tsx.Writes().Facts; got != 1 {
		t.Fatalf("worker ToolSet facts = %d, want 1 (subagent wrote through a different set)", got)
	}
	ids, err := store.FactsYielded(intentID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("intent %d facts = %v, want the subagent record_fact linked to this intent", intentID, ids)
	}
}
