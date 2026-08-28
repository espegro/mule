package main

import (
	"bytes"
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/logging"
)

func TestTopLevelUsageIsConfigFirst(t *testing.T) {
	var out bytes.Buffer
	writeUsage(&out)
	for _, want := range []string{"mule help [COMMAND]", "check-config", "mule agent --config", "mule server --config"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("usage missing %q:\n%s", want, out.String())
		}
	}
}

func TestCommandUsageUsesLongFlags(t *testing.T) {
	fs := flagSetForUsageTest(&bytes.Buffer{})
	var out bytes.Buffer
	fs.SetOutput(&out)
	fs.Usage()
	if !strings.Contains(out.String(), "--config") || strings.Contains(out.String(), "\n  -config") {
		t.Fatalf("expected long options in help:\n%s", out.String())
	}
}

func flagSetForUsageTest(out *bytes.Buffer) *flag.FlagSet {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.String("config", "", "YAML config file")
	setCommandUsage(fs, "Usage: test\n")
	return fs
}

func TestAgentRejectsConfigOwnedFlagsWithConfig(t *testing.T) {
	err := runAgent([]string{"--config", "/does/not/matter.yaml", "--server", "server.example:4400"})
	if err == nil || !strings.Contains(err.Error(), "--config cannot be combined") {
		t.Fatalf("expected mixed config flag error, got %v", err)
	}
}

func TestCheckConfig(t *testing.T) {
	dir := t.TempDir()
	secretPath := dir + "/agent.key"
	if err := auth.GenerateSecretFile(secretPath); err != nil {
		t.Fatal(err)
	}
	configPath := dir + "/agent.yaml"
	configData := "server: 127.0.0.1:4400\nagent_id: test\nsecret_file: " + secretPath + "\nforward:\n  web: 127.0.0.1:8080\n"
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCheckConfig([]string{"--role", "agent", "--config", configPath}); err != nil {
		t.Fatal(err)
	}
}

func TestCheckConfigCanSkipMissingSecret(t *testing.T) {
	dir := t.TempDir()
	configPath := dir + "/agent.yaml"
	configData := "server: 127.0.0.1:4400\nagent_id: test\nsecret_file: /missing.key\nforward:\n  web: 127.0.0.1:8080\n"
	if err := os.WriteFile(configPath, []byte(configData), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runCheckConfig([]string{"--role", "agent", "--config", configPath, "--skip-secrets"}); err != nil {
		t.Fatal(err)
	}
}

func TestWarnMetricsExposure(t *testing.T) {
	tests := []struct {
		addr     string
		wantWarn bool
	}{
		{"127.0.0.1:9100", false},
		{"[::1]:9100", false},
		{"localhost:9100", false},
		{":9100", true},
		{"0.0.0.0:9100", true},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			var out bytes.Buffer
			log := logging.NewForWriter("text", "debug", &out)
			warnMetricsExposure(log, tt.addr)
			gotWarn := strings.Contains(out.String(), "metrics_listener_exposed")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warning = %v, want %v; output=%q", gotWarn, tt.wantWarn, out.String())
			}
		})
	}
}
