package metrics

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
)

func TestStatusIsUnavailableBeforeConfiguration(t *testing.T) {
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	New().Handler().ServeHTTP(rec, req)
	if rec.Code != 503 || rec.Body.String() != "{\"status\":\"starting\"}\n" {
		t.Fatalf("unexpected response: code=%d body=%q", rec.Code, rec.Body.String())
	}
}

func TestAgentStatus(t *testing.T) {
	m := New()
	m.ConfigureAgent("dgx", "server.example.org:4400")
	m.SetAgentConnected("dgx", true)
	m.QUICConnections.Add(1)

	status := requestStatus(t, m)
	if status.Role != "agent" || status.AgentID != "dgx" || status.Server != "server.example.org:4400" {
		t.Fatalf("unexpected agent status: %+v", status)
	}
	if status.Connected == nil || !*status.Connected || status.QUICConnections != 1 {
		t.Fatalf("agent should be connected: %+v", status)
	}
}

func TestServerStatusSortsConnectedAgents(t *testing.T) {
	m := New()
	m.ConfigureServer([]string{"zeta", "alpha", "offline"})
	m.SetAgentConnected("zeta", true)
	m.SetAgentConnected("alpha", true)

	status := requestStatus(t, m)
	if status.Role != "server" || status.ConfiguredAgentCount != 3 {
		t.Fatalf("unexpected server status: %+v", status)
	}
	if want := []string{"alpha", "zeta"}; !reflect.DeepEqual(status.ConnectedAgents, want) {
		t.Fatalf("connected agents = %v, want %v", status.ConnectedAgents, want)
	}
}

func requestStatus(t *testing.T, m *Metrics) Status {
	t.Helper()
	req := httptest.NewRequest("GET", "/status", nil)
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status code = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q", got)
	}
	var status Status
	if err := json.NewDecoder(rec.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	return status
}
