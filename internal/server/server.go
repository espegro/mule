package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/transport"
	"github.com/quic-go/quic-go"
)

type AgentConfig struct {
	ID         string
	SecretFile string
	Forward    map[string]string
	Reverse    map[string]string
}

type Config struct {
	ListenUDP        string
	Agents           []AgentConfig
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	DialTimeout      time.Duration
	MaxStreams       int
	MaxPendingDials  int
	KeepAlive        time.Duration
}

type FileConfig struct {
	ListenUDP        string                     `yaml:"listen_udp"`
	Agents           map[string]FileAgentConfig `yaml:"agents"`
	HandshakeTimeout string                     `yaml:"handshake_timeout"`
	IdleTimeout      string                     `yaml:"idle_timeout"`
	DialTimeout      string                     `yaml:"dial_timeout"`
	MaxStreams       int                        `yaml:"max_streams"`
	MaxPendingDials  int                        `yaml:"max_pending_dials"`
	KeepAlive        string                     `yaml:"keepalive"`
}

type FileAgentConfig struct {
	SecretFile string            `yaml:"secret_file"`
	Forward    map[string]string `yaml:"forward"`
	Reverse    map[string]string `yaml:"reverse"`
}

type Server struct {
	cfg     Config
	log     *logging.Logger
	metrics *metrics.Metrics

	mu    sync.Mutex
	conns map[string]*quic.Conn
}

func LoadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	var fc FileConfig
	if err := config.DecodeYAMLStrict(data, &fc); err != nil {
		return Config{}, err
	}
	cfg := Config{
		ListenUDP:       fc.ListenUDP,
		MaxStreams:      fc.MaxStreams,
		MaxPendingDials: fc.MaxPendingDials,
	}
	for id, agent := range fc.Agents {
		cfg.Agents = append(cfg.Agents, AgentConfig{ID: id, SecretFile: agent.SecretFile, Forward: agent.Forward, Reverse: agent.Reverse})
	}
	var errp error
	if fc.HandshakeTimeout != "" {
		cfg.HandshakeTimeout, errp = time.ParseDuration(fc.HandshakeTimeout)
		if errp != nil {
			return Config{}, errp
		}
	}
	if fc.IdleTimeout != "" {
		cfg.IdleTimeout, errp = time.ParseDuration(fc.IdleTimeout)
		if errp != nil {
			return Config{}, errp
		}
	}
	if fc.DialTimeout != "" {
		cfg.DialTimeout, errp = time.ParseDuration(fc.DialTimeout)
		if errp != nil {
			return Config{}, errp
		}
	}
	if fc.KeepAlive != "" {
		cfg.KeepAlive, errp = time.ParseDuration(fc.KeepAlive)
		if errp != nil {
			return Config{}, errp
		}
	}
	ApplyDefaults(&cfg)
	return cfg, nil
}

func ApplyDefaults(cfg *Config) {
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 100
	}
	if cfg.MaxPendingDials == 0 {
		cfg.MaxPendingDials = 20
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = 20 * time.Second
	}
}

func New(cfg Config, log *logging.Logger, metrics *metrics.Metrics) *Server {
	return &Server{cfg: cfg, log: log, metrics: metrics, conns: make(map[string]*quic.Conn)}
}

func (s *Server) Run(ctx context.Context) error {
	ApplyDefaults(&s.cfg)
	if err := ValidateConfig(s.cfg); err != nil {
		return err
	}
	agentIDs := make([]string, 0, len(s.cfg.Agents))
	for _, agent := range s.cfg.Agents {
		agentIDs = append(agentIDs, agent.ID)
	}
	s.metrics.ConfigureServer(agentIDs)
	clients := make([]auth.ClientIdentity, 0, len(s.cfg.Agents))
	for _, agent := range s.cfg.Agents {
		secret, err := auth.LoadSecretFile(agent.SecretFile)
		if err != nil {
			return err
		}
		clients = append(clients, auth.ClientIdentity{ID: agent.ID, Secret: secret})
	}
	authn, err := auth.MultiClientTLSConfig(clients)
	if err != nil {
		return err
	}
	ln, err := quic.ListenAddr(s.cfg.ListenUDP, authn.TLSConfig, &quic.Config{
		HandshakeIdleTimeout: s.cfg.HandshakeTimeout,
		MaxIdleTimeout:       s.cfg.IdleTimeout,
		KeepAlivePeriod:      s.cfg.KeepAlive,
		MaxIncomingStreams:   int64(s.cfg.MaxStreams),
	})
	if err != nil {
		return err
	}
	defer ln.Close()
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 1)
	reverseConnections := make(chan struct{}, s.cfg.MaxStreams)
	var listeners []net.Listener
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	for _, agent := range s.cfg.Agents {
		for service, addr := range agent.Reverse {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			listeners = append(listeners, l)
			s.log.Info("reverse_listener_started", "role", "server", "agent_id", agent.ID, "service", service, "listener_address", addr)
			go s.acceptReverse(runCtx, l, agent.ID, service, reverseConnections, errCh)
		}
	}
	go func() {
		<-runCtx.Done()
		ln.Close()
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	dials := make(chan struct{}, s.cfg.MaxPendingDials)
	probes := make(chan struct{}, s.cfg.MaxPendingDials)
	s.log.Info("startup", "role", "server", "listener_address", s.cfg.ListenUDP, "agents", len(s.cfg.Agents))
	type acceptResult struct {
		conn *quic.Conn
		err  error
	}
	accepted := make(chan acceptResult)
	go func() {
		for {
			conn, err := ln.Accept(runCtx)
			select {
			case accepted <- acceptResult{conn: conn, err: err}:
			case <-runCtx.Done():
				if conn != nil {
					_ = conn.CloseWithError(0, "shutdown")
				}
				return
			}
			if err != nil {
				return
			}
		}
	}()
	for {
		var conn *quic.Conn
		var err error
		select {
		case result := <-accepted:
			conn, err = result.conn, result.err
		case err = <-errCh:
			return fmt.Errorf("reverse listener failed: %w", err)
		case <-runCtx.Done():
			s.log.Info("shutdown", "role", "server")
			return nil
		}
		if err != nil {
			if runCtx.Err() != nil {
				s.log.Info("shutdown", "role", "server")
				return nil
			}
			return err
		}
		agentID, ok := auth.ClientIDForTLSState(authn.ClientByPublicKey, conn.ConnectionState().TLS)
		if !ok {
			conn.CloseWithError(1, "unauthorized")
			s.metrics.AuthFailuresTotal.Add(1)
			continue
		}
		s.log.Debug("quic_peer_authenticated", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
		if conn.ConnectionState().TLS.ServerName == auth.ProbeServerNameForClient(agentID) {
			select {
			case probes <- struct{}{}:
			default:
				conn.CloseWithError(2, "probe capacity exceeded")
				continue
			}
			s.log.Info("probe_connection_accepted", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
			go func() {
				defer func() { <-probes }()
				s.handleProbeConnection(runCtx, agentID, conn, dials)
			}()
			continue
		}
		if !s.register(agentID, conn) {
			select {
			case probes <- struct{}{}:
			default:
				_ = conn.CloseWithError(2, "agent already connected")
				continue
			}
			go func() {
				defer func() { <-probes }()
				s.handleLegacyProbeConnection(runCtx, agentID, conn, dials)
			}()
			continue
		}
		s.metrics.QUICConnections.Add(1)
		s.metrics.SetAgentConnected(agentID, true)
		s.log.Info("agent_connected", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
		go s.handleAgent(runCtx, agentID, conn, dials)
	}
}

func (s *Server) handleLegacyProbeConnection(ctx context.Context, agentID string, conn *quic.Conn, dials chan struct{}) {
	defer conn.CloseWithError(0, "legacy probe complete")
	probeCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()
	stream, err := conn.AcceptStream(probeCtx)
	if err != nil {
		return
	}
	_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	frame, err := protocol.ReadFrame(stream)
	if err != nil || frame.Type != protocol.TypeProbe {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	s.log.Info("legacy_probe_connection_accepted", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
	s.handleProbe(ctx, agentID, stream, frame, dials)
}

func ValidateConfig(cfg Config) error {
	if cfg.ListenUDP == "" {
		return fmt.Errorf("listen_udp is required")
	}
	if err := config.ValidateUDPAddress(cfg.ListenUDP); err != nil {
		return err
	}
	if len(cfg.Agents) == 0 {
		return fmt.Errorf("at least one agent is required")
	}
	if cfg.HandshakeTimeout <= 0 || cfg.IdleTimeout <= 0 || cfg.DialTimeout <= 0 || cfg.KeepAlive <= 0 {
		return fmt.Errorf("handshake_timeout, idle_timeout, dial_timeout and keepalive must be positive")
	}
	if cfg.MaxStreams <= 0 || cfg.MaxPendingDials <= 0 {
		return fmt.Errorf("max_streams and max_pending_dials must be positive")
	}
	seen := make(map[string]struct{}, len(cfg.Agents))
	for _, agent := range cfg.Agents {
		if err := config.ValidateClientID(agent.ID); err != nil {
			return err
		}
		if _, ok := seen[agent.ID]; ok {
			return fmt.Errorf("duplicate agent %q", agent.ID)
		}
		seen[agent.ID] = struct{}{}
		for service, addr := range agent.Forward {
			if err := config.ValidateRouteID(service); err != nil {
				return err
			}
			if err := config.ValidateTCPAddress(addr); err != nil {
				return err
			}
		}
		for service, addr := range agent.Reverse {
			if err := config.ValidateRouteID(service); err != nil {
				return err
			}
			if err := config.ValidateListenAddress(addr); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) register(agentID string, conn *quic.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing := s.conns[agentID]; existing != nil {
		return false
	}
	s.conns[agentID] = conn
	return true
}

func (s *Server) unregister(agentID string, conn *quic.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conns[agentID] == conn {
		delete(s.conns, agentID)
	}
}

func (s *Server) conn(agentID string) *quic.Conn {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.conns[agentID]
}

func (s *Server) handleAgent(ctx context.Context, agentID string, conn *quic.Conn, dials chan struct{}) {
	defer func() {
		s.unregister(agentID, conn)
		s.metrics.QUICConnections.Add(-1)
		s.metrics.SetAgentConnected(agentID, false)
		s.log.Info("agent_disconnected", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
	}()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		s.log.Debug("stream_accepted", "role", "server", "agent_id", agentID)
		go s.handleAgentStream(ctx, agentID, stream, dials)
	}
}

func (s *Server) handleProbeConnection(ctx context.Context, agentID string, conn *quic.Conn, dials chan struct{}) {
	defer conn.CloseWithError(0, "probe complete")
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go func(stream *quic.Stream) {
			_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
			frame, err := protocol.ReadFrame(stream)
			if err != nil || frame.Type != protocol.TypeProbe {
				_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
				_ = stream.Close()
				_ = conn.CloseWithError(3, "probe connection received non-probe stream")
				return
			}
			s.handleProbe(ctx, agentID, stream, frame, dials)
		}(stream)
	}
}

func (s *Server) handleAgentStream(ctx context.Context, agentID string, stream *quic.Stream, dials chan struct{}) {
	s.metrics.StreamsTotal.Add(1)
	s.metrics.ActiveQUICStreams.Add(1)
	defer s.metrics.ActiveQUICStreams.Add(-1)
	start := time.Now()
	_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	frame, err := protocol.ReadFrame(stream)
	if err != nil {
		s.metrics.StreamErrorsTotal.Add(1)
		s.log.Debug("control_frame_error", "role", "server", "agent_id", agentID, "error", err)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	s.log.Debug("control_frame_received", "role", "server", "agent_id", agentID, "type", frame.Type, "direction", frame.Direction, "service", frame.Service, "connection_id", frame.ConnectionID)
	if frame.Type == protocol.TypeProbe {
		s.handleProbe(ctx, agentID, stream, frame, dials)
		return
	}
	if frame.Type != protocol.TypeOpen || frame.Direction != protocol.DirectionForward {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	target, ok := s.forwardTarget(agentID, frame.Service)
	if !ok {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		s.log.Warn("stream_rejected", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "reason", "unauthorized")
		return
	}
	select {
	case dials <- struct{}{}:
	default:
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorOverloaded})
		_ = stream.Close()
		return
	}
	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	s.log.Debug("target_dial_started", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "target_address", target, "connection_id", frame.ConnectionID)
	tcpConn, err := dialer.DialContext(ctx, "tcp", target)
	<-dials
	if err != nil {
		s.metrics.TargetDialErrors.Add(1)
		s.metrics.StreamErrorsTotal.Add(1)
		s.log.Debug("target_dial_error", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "target_address", target, "connection_id", frame.ConnectionID, "error", err)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorDialFailed})
		_ = stream.Close()
		s.log.Warn("target_dial_failed", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "target_address", target)
		return
	}
	defer tcpConn.Close()
	s.metrics.TCPConnectionsTotal.Add(1)
	s.metrics.ActiveTCPConnections.Add(1)
	defer s.metrics.ActiveTCPConnections.Add(-1)
	s.log.Debug("target_dial_succeeded", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "target_address", target, "connection_id", frame.ConnectionID)
	if err := protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeOK}); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	count := transport.Pipe(tcpConn, stream)
	s.metrics.BytesClientToTarget.Add(count.BToA)
	s.metrics.BytesTargetToClient.Add(count.AToB)
	s.log.Info("connection_closed", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "connection_id", frame.ConnectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.BToA, "bytes_target_to_client", count.AToB)
}

func (s *Server) handleProbe(ctx context.Context, agentID string, stream *quic.Stream, frame protocol.Frame, dials chan struct{}) {
	switch frame.Direction {
	case protocol.DirectionForward:
		target, ok := s.forwardTarget(agentID, frame.Service)
		if !ok {
			_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
			_ = stream.Close()
			return
		}
		select {
		case dials <- struct{}{}:
			defer func() { <-dials }()
		default:
			_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorOverloaded})
			_ = stream.Close()
			return
		}
		dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
		conn, err := dialer.DialContext(ctx, "tcp", target)
		if err != nil {
			_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorDialFailed})
			_ = stream.Close()
			return
		}
		_ = conn.Close()
	case protocol.DirectionReverse:
		if !s.reverseAllowed(agentID, frame.Service) {
			_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
			_ = stream.Close()
			return
		}
	default:
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeOK})
	_ = stream.Close()
}

func (s *Server) forwardTarget(agentID, service string) (string, bool) {
	for _, agent := range s.cfg.Agents {
		if agent.ID == agentID {
			target, ok := agent.Forward[service]
			return target, ok
		}
	}
	return "", false
}

func (s *Server) reverseAllowed(agentID, service string) bool {
	for _, agent := range s.cfg.Agents {
		if agent.ID == agentID {
			_, ok := agent.Reverse[service]
			return ok
		}
	}
	return false
}

func (s *Server) acceptReverse(ctx context.Context, ln net.Listener, agentID, service string, sem chan struct{}, errCh chan<- error) {
	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			select {
			case errCh <- err:
			case <-ctx.Done():
			}
			return
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				s.handleReverseTCP(ctx, tcpConn, agentID, service, ln.Addr().String())
			}()
		default:
			_ = tcpConn.Close()
			s.log.Warn("reverse_rejected", "role", "server", "agent_id", agentID, "service", service, "reason", "connection_limit")
		}
	}
}

func (s *Server) handleReverseTCP(ctx context.Context, tcpConn net.Conn, agentID, service, listener string) {
	defer tcpConn.Close()
	s.metrics.TCPConnectionsTotal.Add(1)
	s.metrics.ActiveTCPConnections.Add(1)
	defer s.metrics.ActiveTCPConnections.Add(-1)
	start := time.Now()
	connectionID := newConnectionID()
	conn := s.conn(agentID)
	if conn == nil {
		s.log.Warn("reverse_rejected", "role", "server", "agent_id", agentID, "service", service, "reason", "agent_offline")
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, s.cfg.HandshakeTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(openCtx)
	if err != nil {
		s.metrics.StreamErrorsTotal.Add(1)
		s.log.Debug("stream_open_error", "role", "server", "agent_id", agentID, "service", service, "direction", "reverse", "connection_id", connectionID, "error", err)
		s.log.Warn("reverse_rejected", "role", "server", "agent_id", agentID, "service", service, "reason", "open_stream_failed")
		return
	}
	s.metrics.StreamsTotal.Add(1)
	s.metrics.ActiveQUICStreams.Add(1)
	defer s.metrics.ActiveQUICStreams.Add(-1)
	s.log.Debug("stream_opened", "role", "server", "agent_id", agentID, "service", service, "direction", "reverse", "connection_id", connectionID)
	_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeOpen,
		Direction:    protocol.DirectionReverse,
		Service:      service,
		PeerID:       agentID,
		Listener:     listener,
		SourceAddr:   tcpConn.RemoteAddr().String(),
		ConnectionID: connectionID,
	}); err != nil {
		_ = stream.Close()
		return
	}
	resp, err := protocol.ReadFrame(stream)
	if err != nil || resp.Type != protocol.TypeOK {
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	count := transport.Pipe(tcpConn, stream)
	s.metrics.BytesClientToTarget.Add(count.AToB)
	s.metrics.BytesTargetToClient.Add(count.BToA)
	s.log.Info("connection_closed", "role", "server", "agent_id", agentID, "service", service, "direction", "reverse", "connection_id", connectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.AToB, "bytes_target_to_client", count.BToA)
}

func newConnectionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
