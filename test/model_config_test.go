package test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"stream-gateway-lab/internal/control"
)

func TestModelConfigPersistsWithoutExposingKey(t *testing.T) {
	dir := t.TempDir()
	python, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := control.Config{DataDir: dir, AdminToken: "admin", BrainBinary: "unused", BrainURL: "ws://unused", AgentDir: dir, AgentPython: python, ModelConfig: filepath.Join(dir, "model.json")}
	s, err := control.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	server := httptest.NewServer(s)
	defer server.Close()
	poolRequest(t, server.URL, "/api/model-config", "", "GET", nil, 401, nil)
	poolRequest(t, server.URL, "/api/model-config", "", "PUT", map[string]string{}, 401, nil)
	var view map[string]any
	poolRequest(t, server.URL, "/api/model-config", "admin", "GET", nil, 200, &view)
	if view["api_key_set"] != false {
		t.Fatal("new configuration must have no saved key")
	}
	input := map[string]string{"base_url": "https://example.test/v1/", "model": "test-model", "api_key": ""}
	poolRequest(t, server.URL, "/api/model-config", "admin", "PUT", input, 400, nil)
	if _, err := os.Stat(cfg.ModelConfig); !os.IsNotExist(err) {
		t.Fatal("incomplete configuration must not be saved")
	}
	if err := os.WriteFile(cfg.ModelConfig, []byte(`{"max_model_calls":7,"reasoning":{"effort":"low"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"fixture-secret-one", "", "fixture-secret-two"} {
		input["api_key"] = key
		response := poolRequest(t, server.URL, "/api/model-config", "admin", "PUT", input, 200, &view)
		if bytes.Contains(response, []byte("fixture-secret")) || view["api_key_set"] != true {
			t.Fatal("save response must indicate a key without exposing it")
		}
		response = poolRequest(t, server.URL, "/api/model-config", "admin", "GET", nil, 200, &view)
		if bytes.Contains(response, []byte("fixture-secret")) || view["base_url"] != "https://example.test/v1" {
			t.Fatal("read response must not expose the saved key")
		}
		data, err := os.ReadFile(cfg.ModelConfig)
		if err != nil {
			t.Fatal(err)
		}
		var saved map[string]any
		if err := json.Unmarshal(data, &saved); err != nil {
			t.Fatal(err)
		}
		wantKey := key
		if key == "" {
			wantKey = "fixture-secret-one"
		}
		if saved["api_key"] != wantKey || saved["max_model_calls"] != float64(7) || saved["reasoning"] == nil {
			t.Fatal("save must preserve an omitted key and existing advanced options")
		}
	}
	before, _ := os.ReadFile(cfg.ModelConfig)
	for _, bad := range []string{"file:///tmp", "https://example.test/v1/responses", "https://user:pass@example.test/v1", "https://example.test/v1?key=x"} {
		input["base_url"] = bad
		poolRequest(t, server.URL, "/api/model-config", "admin", "PUT", input, 400, nil)
		after, _ := os.ReadFile(cfg.ModelConfig)
		if !bytes.Equal(before, after) {
			t.Fatal("invalid update changed the saved configuration")
		}
	}
	info, _ := os.Stat(cfg.ModelConfig)
	if info.Mode().Perm() != 0600 {
		t.Fatal("configuration must remain private")
	}
	var state map[string]any
	poolRequest(t, server.URL, "/api/state", "admin", "GET", nil, 200, &state)
	if state["model_ready"] != true {
		t.Fatal("configuration should take effect without a restart")
	}
	server.Close()
	s.Close()
	s, err = control.New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted := httptest.NewServer(s)
	defer restarted.Close()
	poolRequest(t, restarted.URL, "/api/model-config", "admin", "GET", nil, 200, &view)
	if view["api_key_set"] != true || view["model"] != "test-model" {
		t.Fatal("configuration did not survive restart")
	}
}
