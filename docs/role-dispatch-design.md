# Role-Based Planner Dispatch (Strix-style)

**Status:** Design proposal · flag-gated · backward compatible
**Scope:** `agent/planner.go`, `agent/worker.go`, `agent/tools*.go`, `agent/worker_subagent.go`
**Non-scope:** `server/engine.go` (no changes required)

## 1. Goal

Introduce explicit, Strix-like orchestration where the **planner (the root)**
dispatches a **recon** intent first, is **woken by its result**, then dispatches
**exploit**, then **validate** — an adaptive, sequential root→recon→return→root→
dispatch-next loop.

Crucially, this is achieved by **reusing the existing planner + worker-pool +
engine loop**. There is **no new orchestration engine**. The change is about
*role specialization and intent granularity*, not architecture.

## 2. Current State (and why we already have the root loop)

ARTEX today already contains the structural equivalent of the Strix root-dispatch
loop:

- **Planner** (`agent/planner.go` + `server/engine.go` `plannerLoop`) is the sole
  intent generator. It wakes on `NotifyDone` / `NotifyFinding` and
  re-dispatches adaptively based on results.
- **Worker pool** (default 3) claims intents and executes them via the engine
  (`claimNext` / `runIntent` / `workerLoop`).
- **Engine** drives the loop and the wake notifications.

> The planner's wake-and-re-dispatch behavior **is** the root-dispatch loop
> (root → dispatch → woken by result → dispatch-next). The architecture is
> already there.

**Option A** (`agent/worker_subagent.go`) is *orthogonal* to this: it provides
blocking, nested `Agent` tool calls **inside one claimed intent**. That is why
it currently "feels like one worker" — the nesting happens within a single
phase, it does not drive the root loop.

**The gap** is therefore **role specialization + intent granularity**, not the
orchestration architecture.

## 3. Strix ↔ ARTEX Mapping

| Strix concept                        | ARTEX mechanism                                                        |
|--------------------------------------|------------------------------------------------------------------------|
| Root agent                           | Planner (`agent/planner.go`, `plannerLoop` in `server/engine.go`)      |
| Root dispatches a task               | Planner emits an intent via `add_intent` tool                          |
| Specialized subagent (recon/exploit/validate) | Worker executing an intent with a role-specific prompt + tool allow-list |
| Subagent returns result to root      | `NotifyDone` / `NotifyFinding` wakes the planner                       |
| Root decides next task from result   | Planner re-dispatches next-phase intent on wake                        |
| Sequential recon → exploit → validate | `role`-tagged intents chained by the planner prompt                    |
| Nested helper agents within a task   | Option A nested subagents (`worker_subagent.go`) — intra-phase         |
| Shared memory / scratchpad           | Persistent `TodoStore`                                                 |

## 4. Implementation Path

### Step 1 — Add optional `role` to the intent payload
- Add an optional `role` field with values `recon | exploit | validate` to the
  intent payload struct.
- Extend the planner's `add_intent` tool schema (`agent/tools*.go`) to accept
  `role`.
- Update the planner prompt (`plannerDefaultTmpl` in `agent/planner.go`) so the
  planner explicitly dispatches a **recon** intent first, then dispatches the
  next phase based on returned results.
- `role` is **optional**: role-less intents behave exactly as today.

### Step 2 — Worker selects behavior by role
- In `Worker.execute` (`agent/worker.go`), select a **role-specific system
  prompt** and **tool allow-list** keyed on `intent.role`.
- Lift the three role definitions already present in `subagentDefs`
  (`agent/worker_subagent.go`) **up to the top-level worker**, so recon /
  exploit / validate behavior is reusable for a claimed intent (not only for
  nested subagents).

### Step 3 — Strengthen the serial-chain planner prompt
- Reinforce the planner prompt (`agent/planner.go`) to sequence
  **recon → exploit → validate** explicitly.
- Reuse the persistent `TodoStore` so phase results and chain state survive
  across wakes.

### Step 4 — Keep Option A as an intra-phase helper
- Option A (`agent/worker_subagent.go`) remains available for nested subagents
  **within a single phase** (e.g. parallel recon helpers inside the recon
  intent). It is **complementary**, not replaced.

### Step 5 — Engine untouched
- `server/engine.go` (`plannerLoop` / `workerLoop` / `claimNext` / `runIntent`)
  needs **no change**. The existing wake-and-dispatch loop already provides the
  root→recon→return→root→dispatch-next behavior.

## 5. File / Function Change List

| File                         | Function / Symbol               | Change                                                           |
|------------------------------|---------------------------------|------------------------------------------------------------------|
| `agent/planner.go`           | intent payload struct           | Add optional `role` field (`recon`/`exploit`/`validate`)        |
| `agent/planner.go`           | `plannerDefaultTmpl`            | Prompt: dispatch recon first, chain next-phase on results        |
| `agent/tools*.go`            | `add_intent` tool schema        | Accept optional `role` argument                                  |
| `agent/worker.go`            | `Worker.execute`               | Select role-specific system prompt + tool allow-list by `role`   |
| `agent/worker_subagent.go`   | `subagentDefs`                 | Lift the 3 role definitions to top-level worker (reuse)          |
| `agent/worker_subagent.go`   | Option A nested subagents       | Keep as intra-phase helper (no removal)                          |
| `server/engine.go`           | `plannerLoop`/`workerLoop`/`claimNext`/`runIntent` | **No change**                               |

## 6. Compatibility & Rollout

- **Flag-gated:** the role-based dispatch behavior is enabled behind a flag.
- **Backward compatible:** `role` is optional; intents without a role behave
  exactly as the system does today.
- **Incremental:** Steps 1–3 deliver the core sequential role dispatch; Step 4
  is additive; Step 5 confirms no engine work is needed.

## 7. Summary

ARTEX already has the Strix root-dispatch loop in its planner + worker-pool +
engine. To get explicit Strix-style **recon → exploit → validate** orchestration
we only need to add role specialization and finer intent granularity — a `role`
field, role-aware worker behavior, and a stronger serial-chain planner prompt —
all reusing the existing engine, flag-gated and backward compatible.
