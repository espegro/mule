package server

import (
	"os"
	"strings"
	"testing"
)

func validTestConfig() Config {
	cfg := Config{
		ListenUDP: "127.0.0.1:4400",
		Agents: []AgentConfig{{
			ID:         "test",
			SecretFile: "/tmp/test.key",
			Forward:    map[string]string{"web": "127.0.0.1:3000"},
		}},
	}
	ApplyDefaults(&cfg)
	return cfg
}

func TestValidateConfigRejectsInvalidLimits(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{"negative max streams", func(cfg *Config) { cfg.MaxStreams = -1 }, "max_streams"},
		{"negative pending dials", func(cfg *Config) { cfg.MaxPendingDials = -1 }, "max_pending_dials"},
		{"negative dial timeout", func(cfg *Config) { cfg.DialTimeout = -1 }, "dial_timeout"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validTestConfig()
			tt.mutate(&cfg)
			if err := ValidateConfig(cfg); err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("expected %s error, got %v", tt.want, err)
			}
		})
	}
}

func TestLoadConfigRejectsUnknownField(t *testing.T) {
	path := t.TempDir() + "/server.yaml"
	data := []byte("listen_udp: :4400\nmax_pending_dials: 10\nagnts:\n  test:\n    secret_file: /tmp/test.key\n")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil || !strings.Contains(err.Error(), "agnts") {
		t.Fatalf("expected unknown-field error, got %v", err)
	}
}
