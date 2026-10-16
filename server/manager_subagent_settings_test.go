package server

import (
	"testing"
)

// clearWorkerSubagentSettings drops the three keys so a shared settings table
// left dirty by another test still exercises the compiled-in defaults.
func clearWorkerSubagentSettings(t *testing.T, m *Manager) {
	t.Helper()
	for _, key := range []string{settingWorkerSubagentsOn, settingWorkerSubagentMaxDepth, settingWorkerSubagentParallel} {
		if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, key); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWorkerSubagentSettingsRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	t.Cleanup(func() {
		_ = m.SetWorkerSubagents(false, defaultWorkerSubagentMaxDepth, defaultWorkerSubagentMaxParallel)
	})
	clearWorkerSubagentSettings(t, m)

	if m.WorkerSubagentsEnabled() {
		t.Fatal("default worker_subagents_enabled must be false")
	}
	if got := m.WorkerSubagentMaxDepth(); got != defaultWorkerSubagentMaxDepth {
		t.Fatalf("default depth %d, want %d", got, defaultWorkerSubagentMaxDepth)
	}
	if got := m.WorkerSubagentMaxParallel(); got != defaultWorkerSubagentMaxParallel {
		t.Fatalf("default parallel %d, want %d", got, defaultWorkerSubagentMaxParallel)
	}

	if err := m.SetWorkerSubagents(true, 5, 2); err != nil {
		t.Fatal(err)
	}
	if !m.WorkerSubagentsEnabled() || m.WorkerSubagentMaxDepth() != 5 || m.WorkerSubagentMaxParallel() != 2 {
		t.Fatalf("after set: on=%v depth=%d parallel=%d", m.WorkerSubagentsEnabled(), m.WorkerSubagentMaxDepth(), m.WorkerSubagentMaxParallel())
	}

	// Non-positive caps fall back to the defaults; the toggle still persists.
	if err := m.SetWorkerSubagents(false, 0, -1); err != nil {
		t.Fatal(err)
	}
	if m.WorkerSubagentsEnabled() {
		t.Fatal("expected disabled")
	}
	if m.WorkerSubagentMaxDepth() != defaultWorkerSubagentMaxDepth || m.WorkerSubagentMaxParallel() != defaultWorkerSubagentMaxParallel {
		t.Fatalf("clamped caps depth=%d parallel=%d", m.WorkerSubagentMaxDepth(), m.WorkerSubagentMaxParallel())
	}
}

func TestWorkerSubagentConfigFollowsFlag(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	t.Cleanup(func() {
		_ = m.SetWorkerSubagents(false, defaultWorkerSubagentMaxDepth, defaultWorkerSubagentMaxParallel)
	})
	clearWorkerSubagentSettings(t, m)
	s := &Server{m: m}
	if s.workerSubagentConfig() != nil {
		t.Fatal("flag off must not populate a worker subagent config")
	}
	if err := m.SetWorkerSubagents(true, 3, 4); err != nil {
		t.Fatal(err)
	}
	cfg := s.workerSubagentConfig()
	if cfg == nil || cfg.MaxDepth != 3 || cfg.MaxParallel != 4 {
		t.Fatalf("flag on config = %+v", cfg)
	}
}
