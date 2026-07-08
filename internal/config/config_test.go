package config

import (
	"os"
	"testing"
)

func TestValidateAddress(t *testing.T) {
	valid := []string{
		"127.0.0.1:3000",
		"localhost:3000",
		"[2001:db8::1]:4400",
	}
	for _, addr := range valid {
		if err := ValidateTCPAddress(addr); err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
	}
	if err := ValidateUDPAddress(":4400"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateListenAddress(":3000"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateTCPAddress(":3000"); err == nil {
		t.Fatal("peer/target address without host should be rejected")
	}
}

func TestValidateAddressRejectsInvalidPort(t *testing.T) {
	invalid := []string{
		"127.0.0.1:0",
		"127.0.0.1:65536",
		"127.0.0.1:notaport",
		"2001:db8::1:4400",
	}
	for _, addr := range invalid {
		if err := ValidateTCPAddress(addr); err == nil {
			t.Fatalf("%s: expected error", addr)
		}
	}
}

func TestParseRouteMappings(t *testing.T) {
	listen, err := ParseRouteListen("web=127.0.0.1:3000")
	if err != nil {
		t.Fatal(err)
	}
	if listen.Route != "web" || listen.Address != "127.0.0.1:3000" {
		t.Fatalf("unexpected listen mapping: %+v", listen)
	}
	route, target, err := ParseRouteTarget("ssh=[2001:db8::1]:22")
	if err != nil {
		t.Fatal(err)
	}
	if route != "ssh" || target != "[2001:db8::1]:22" {
		t.Fatalf("unexpected route mapping: %s %s", route, target)
	}
}

func TestRouteMappingsRejectDuplicates(t *testing.T) {
	_, err := NormalizeForwardListens(Forward{
		ListenTCP: "127.0.0.1:3000",
		Listens:   []RouteListen{{Route: DefaultRoute, Address: "127.0.0.1:3001"}},
	})
	if err == nil {
		t.Fatal("expected duplicate forward route error")
	}

	_, err = NormalizeExitRoutes(Exit{
		Target: "127.0.0.1:443",
		Routes: map[string]string{DefaultRoute: "127.0.0.1:8443"},
	})
	if err == nil {
		t.Fatal("expected duplicate exit route error")
	}
}

func TestValidateForwardID(t *testing.T) {
	for _, id := range []string{"", "host-b", "edge.1", "site:a"} {
		if err := ValidateForwardID(id); err != nil {
			t.Fatalf("%q: %v", id, err)
		}
	}
	for _, id := range []string{"bad id", "bad/id"} {
		if err := ValidateForwardID(id); err == nil {
			t.Fatalf("%q: expected error", id)
		}
	}
}

func TestLoadExitFileMultiClient(t *testing.T) {
	path := t.TempDir() + "/exit.yaml"
	data := []byte(`
listen_udp: ":4400"
clients:
  host-b:
    secret_file: /etc/mule/host-b.key
    routes:
      ollama: 127.0.0.1:11434
  host-c:
    secret_file: /etc/mule/host-c.key
    routes:
      ssh: 127.0.0.1:22
dial_timeout: 3s
`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadExitFile(path)
	if err != nil {
		t.Fatal(err)
	}
	ApplyExitDefaults(&cfg)
	if cfg.ListenUDP != ":4400" || cfg.DialTimeout.String() != "3s" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
	if _, err := NormalizeClientRoutes(cfg); err != nil {
		t.Fatal(err)
	}
}
