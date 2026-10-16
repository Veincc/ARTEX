package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestWorkerSubagentSettingsHTTPRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	td := t.TempDir()
	s := New(context.Background(), m, td, td, td)
	h := s.Handler()
	tok, err := signJWT(s.jwtKey)
	if err != nil {
		t.Fatal(err)
	}
	do := func(method, path string, body any) (int, map[string]any) {
		t.Helper()
		var r *http.Request
		if body != nil {
			b, _ := json.Marshal(body)
			r = httptest.NewRequest(method, path, bytes.NewReader(b))
			r.Header.Set("Content-Type", "application/json")
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// Reset first so the round trip does not depend on whatever a previous test
	// left in the shared settings table. The compiled-in defaults (off / 3 / 4)
	// are covered by TestWorkerSubagentSettingsRoundTrip.
	code, out := do("PUT", "/api/settings", map[string]any{
		"worker_subagents_enabled":     false,
		"worker_subagent_max_depth":    defaultWorkerSubagentMaxDepth,
		"worker_subagent_max_parallel": defaultWorkerSubagentMaxParallel,
	})
	if code != 200 {
		t.Fatalf("reset PUT: %d %v", code, out)
	}
	code, out = do("GET", "/api/settings", nil)
	if code != 200 {
		t.Fatalf("GET settings: %d %v", code, out)
	}
	if out["worker_subagents_enabled"] != false {
		t.Fatalf("reset enabled = %v", out["worker_subagents_enabled"])
	}
	if out["worker_subagent_max_depth"] != float64(defaultWorkerSubagentMaxDepth) {
		t.Fatalf("reset depth = %v", out["worker_subagent_max_depth"])
	}
	if out["worker_subagent_max_parallel"] != float64(defaultWorkerSubagentMaxParallel) {
		t.Fatalf("reset parallel = %v", out["worker_subagent_max_parallel"])
	}

	code, out = do("PUT", "/api/settings", map[string]any{
		"worker_subagents_enabled":     true,
		"worker_subagent_max_depth":    6,
		"worker_subagent_max_parallel": 2,
	})
	if code != 200 {
		t.Fatalf("PUT settings: %d %v", code, out)
	}
	if out["worker_subagents_enabled"] != true || out["worker_subagent_max_depth"] != float64(6) || out["worker_subagent_max_parallel"] != float64(2) {
		t.Fatalf("PUT echo = %v", out)
	}

	// Partial update keeps the caps when only the toggle is sent.
	code, out = do("PUT", "/api/settings", map[string]any{"worker_subagents_enabled": false})
	if code != 200 {
		t.Fatalf("PUT toggle: %d %v", code, out)
	}
	if out["worker_subagents_enabled"] != false || out["worker_subagent_max_depth"] != float64(6) || out["worker_subagent_max_parallel"] != float64(2) {
		t.Fatalf("partial PUT = enabled %v depth %v parallel %v", out["worker_subagents_enabled"], out["worker_subagent_max_depth"], out["worker_subagent_max_parallel"])
	}
	t.Cleanup(func() {
		_ = m.SetWorkerSubagents(false, defaultWorkerSubagentMaxDepth, defaultWorkerSubagentMaxParallel)
	})
}
