package config

import "testing"

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
