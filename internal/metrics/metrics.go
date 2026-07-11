package metrics

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

type Metrics struct {
	ActiveTCPConnections atomic.Int64
	ActiveQUICStreams    atomic.Int64
	QUICConnections      atomic.Int64
	TCPConnectionsTotal  atomic.Uint64
	StreamsTotal         atomic.Uint64
	StreamErrorsTotal    atomic.Uint64
	TargetDialErrors     atomic.Uint64
	AuthFailuresTotal    atomic.Uint64
	BytesClientToTarget  atomic.Uint64
	BytesTargetToClient  atomic.Uint64

	statusMu        sync.RWMutex
	role            string
	agentID         string
	server          string
	connectedAgents map[string]bool
}

type Status struct {
	Role                 string   `json:"role"`
	AgentID              string   `json:"agent_id,omitempty"`
	Server               string   `json:"server,omitempty"`
	Connected            *bool    `json:"connected,omitempty"`
	ConnectedAgents      []string `json:"connected_agents,omitempty"`
	ConfiguredAgentCount int      `json:"configured_agent_count,omitempty"`
	QUICConnections      int64    `json:"quic_connections"`
}

func New() *Metrics { return &Metrics{} }

func (m *Metrics) ConfigureAgent(agentID, server string) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.role = "agent"
	m.agentID = agentID
	m.server = server
	m.connectedAgents = map[string]bool{agentID: false}
}

func (m *Metrics) ConfigureServer(agentIDs []string) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	m.role = "server"
	m.connectedAgents = make(map[string]bool, len(agentIDs))
	for _, id := range agentIDs {
		m.connectedAgents[id] = false
	}
}

func (m *Metrics) SetAgentConnected(agentID string, connected bool) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if m.connectedAgents == nil {
		m.connectedAgents = make(map[string]bool)
	}
	m.connectedAgents[agentID] = connected
}

func (m *Metrics) Status() Status {
	m.statusMu.RLock()
	defer m.statusMu.RUnlock()
	status := Status{
		Role:            m.role,
		AgentID:         m.agentID,
		Server:          m.server,
		QUICConnections: m.QUICConnections.Load(),
	}
	if m.role == "agent" {
		connected := m.connectedAgents[m.agentID]
		status.Connected = &connected
	}
	if m.role == "server" {
		status.ConfiguredAgentCount = len(m.connectedAgents)
		for id, connected := range m.connectedAgents {
			if connected {
				status.ConnectedAgents = append(status.ConnectedAgents, id)
			}
		}
		sort.Strings(status.ConnectedAgents)
	}
	return status
}

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/status" {
			w.Header().Set("Content-Type", "application/json")
			if m.Status().Role == "" {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte("{\"status\":\"starting\"}\n"))
				return
			}
			_ = json.NewEncoder(w).Encode(m.Status())
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "mule_active_tcp_connections %d\n", m.ActiveTCPConnections.Load())
		fmt.Fprintf(w, "mule_active_quic_streams %d\n", m.ActiveQUICStreams.Load())
		fmt.Fprintf(w, "mule_quic_connections %d\n", m.QUICConnections.Load())
		fmt.Fprintf(w, "mule_tcp_connections_total %d\n", m.TCPConnectionsTotal.Load())
		fmt.Fprintf(w, "mule_streams_total %d\n", m.StreamsTotal.Load())
		fmt.Fprintf(w, "mule_stream_errors_total %d\n", m.StreamErrorsTotal.Load())
		fmt.Fprintf(w, "mule_target_dial_errors_total %d\n", m.TargetDialErrors.Load())
		fmt.Fprintf(w, "mule_auth_failures_total %d\n", m.AuthFailuresTotal.Load())
		fmt.Fprintf(w, "mule_bytes_client_to_target_total %d\n", m.BytesClientToTarget.Load())
		fmt.Fprintf(w, "mule_bytes_target_to_client_total %d\n", m.BytesTargetToClient.Load())
	})
}

func Serve(ctx context.Context, addr string, m *Metrics) error {
	if addr == "" {
		return nil
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		err := <-errCh
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
