package auth

import (
	"crypto/tls"
	"testing"
)

func TestMultiClientTLSConfigServesAgentAndProbeNames(t *testing.T) {
	secret := make([]byte, MinSecretBytes)
	for i := range secret {
		secret[i] = byte(i + 1)
	}
	authn, err := MultiClientTLSConfig([]ClientIdentity{{ID: "agent-1", Secret: secret}})
	if err != nil {
		t.Fatal(err)
	}
	for _, serverName := range []string{ExitServerNameForClient("agent-1"), ProbeServerNameForClient("agent-1")} {
		cert, err := authn.TLSConfig.GetCertificate(&tls.ClientHelloInfo{ServerName: serverName})
		if err != nil {
			t.Fatalf("certificate for %q: %v", serverName, err)
		}
		if cert == nil {
			t.Fatalf("certificate for %q is nil", serverName)
		}
	}
	if ExitServerNameForClient("agent-1") == ProbeServerNameForClient("agent-1") {
		t.Fatal("agent and probe SNI names must differ")
	}
}
