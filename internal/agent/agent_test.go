package agent

import (
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
	if err := validateConfig(cfg); err == nil {
		t.Fatal("expected negative reconnect delay to be rejected")
	}
}
