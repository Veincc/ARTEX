package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRoleDispatchSettingsRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	t.Cleanup(func() { _ = m.SetRoleDispatch(false) })
	if _, err := m.pg.Exec(`DELETE FROM settings WHERE key=$1`, settingRoleDispatch); err != nil {
		t.Fatal(err)
	}
	if m.RoleDispatchEnabled() {
		t.Fatal("default role_dispatch_enabled must be false")
	}
	if err := m.SetRoleDispatch(true); err != nil {
		t.Fatal(err)
	}
	if !m.RoleDispatchEnabled() {
		t.Fatal("expected role dispatch on")
	}
	if err := m.SetRoleDispatch(false); err != nil {
		t.Fatal(err)
	}
	if m.RoleDispatchEnabled() {
		t.Fatal("expected role dispatch off")
	}

	s := &Server{m: m}
	if s.roleDispatchEnabled() {
		t.Fatal("server resolver should follow the flag")
	}
	if err := m.SetRoleDispatch(true); err != nil {
		t.Fatal(err)
	}
	if !s.roleDispatchEnabled() {
		t.Fatal("server resolver did not see the flag")
	}
}

func TestRoleDispatchSettingsHTTPRoundTrip(t *testing.T) {
	m, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Skipf("postgres unavailable (%v)", err)
	}
	defer m.Close()
	t.Cleanup(func() { _ = m.SetRoleDispatch(false) })
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

	code, out := do("PUT", "/api/settings", map[string]any{"role_dispatch_enabled": false})
	if code != 200 {
		t.Fatalf("reset PUT: %d %v", code, out)
	}
	code, out = do("GET", "/api/settings", nil)
	if code != 200 || out["role_dispatch_enabled"] != false {
		t.Fatalf("GET role_dispatch_enabled = %v (%d)", out["role_dispatch_enabled"], code)
	}
	code, out = do("PUT", "/api/settings", map[string]any{"role_dispatch_enabled": true})
	if code != 200 || out["role_dispatch_enabled"] != true {
		t.Fatalf("PUT echo = %v (%d)", out["role_dispatch_enabled"], code)
	}
	// A partial update of an unrelated field must leave the toggle on.
	code, out = do("PUT", "/api/settings", map[string]any{"workers": m.Workers()})
	if code != 200 || out["role_dispatch_enabled"] != true {
		t.Fatalf("partial PUT cleared role dispatch: %v (%d)", out["role_dispatch_enabled"], code)
	}
}
