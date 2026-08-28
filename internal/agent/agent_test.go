package agent

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestNextReconnectDelay(t *testing.T) {
	base := time.Second
	tests := []struct {
		current time.Duration
		want    time.Duration
	}{
		{time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{32 * time.Second, time.Minute},
		{time.Minute, time.Minute},
	}
	for _, tt := range tests {
		if got := nextReconnectDelay(tt.current, base); got != tt.want {
			t.Fatalf("nextReconnectDelay(%s) = %s, want %s", tt.current, got, tt.want)
		}
	}
}

func TestValidateConfigRejectsInvalidLimits(t *testing.T) {
	cfg := Config{
		Server:     "127.0.0.1:4400",
		AgentID:    "test",
		SecretFile: "/tmp/test.key",
		Forward:    map[string]string{"service": "127.0.0.1:3000"},
	}
	ApplyDefaults(&cfg)
	cfg.MaxConnections = -1
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "max_connections") {
		t.Fatalf("expected max_connections error, got %v", err)
	}
}

func TestValidateConfigRejectsNegativeTimeout(t *testing.T) {
	cfg := Config{
		Server:     "127.0.0.1:4400",
		AgentID:    "test",
		SecretFile: "/tmp/test.key",
		Forward:    map[string]string{"service": "127.0.0.1:3000"},
	}
	ApplyDefaults(&cfg)
	cfg.ConnectTimeout = -time.Second
	if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), "connect_timeout") {
		t.Fatalf("expected connect_timeout error, got %v", err)
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path := t.TempDir() + "/agent.yaml"
	data := []byte("server: 127.0.0.1:4400\nagent_id: test\nsecret_file: /tmp/test.key\nforward:\n  web: 127.0.0.1:3000\nmax_conections: 10\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "max_conections") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}

func TestLoadConfigRejectsAdditionalDocument(t *testing.T) {
	path := t.TempDir() + "/agent.yaml"
	data := []byte("server: 127.0.0.1:4400\n---\nagent_id: test\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Fatalf("expected multiple-document error, got %v", err)
	}
}

func TestNextReconnectDelayPreservesLargeBase(t *testing.T) {
	base := 2 * time.Minute
	if got := nextReconnectDelay(base, base); got != base {
		t.Fatalf("nextReconnectDelay(%s) = %s, want %s", base, got, base)
	}
}

func TestValidateConfigRejectsNegativeReconnectDelay(t *testing.T) {
	cfg := Config{
		Server:         "127.0.0.1:4400",
		AgentID:        "test",
		SecretFile:     "/tmp/test.key",
		Forward:        map[string]string{"service": "127.0.0.1:3000"},
		ReconnectDelay: -time.Second,
	}
	if err := ValidateConfig(cfg); err == nil {
		t.Fatal("expected negative reconnect delay to be rejected")
	}
}
