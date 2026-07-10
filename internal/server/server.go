package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"gopkg.in/yaml.v3"
	"os"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/transport"
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
	if err := yaml.Unmarshal(data, &fc); err != nil {
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
	if err := validateConfig(s.cfg); err != nil {
		return err
	}
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

	errCh := make(chan error, 2+len(s.cfg.Agents))
	var listeners []net.Listener
	for _, agent := range s.cfg.Agents {
		for service, addr := range agent.Reverse {
			l, err := net.Listen("tcp", addr)
			if err != nil {
				return err
			}
			listeners = append(listeners, l)
			s.log.Info("reverse_listener_started", "role", "server", "agent_id", agent.ID, "service", service, "listener_address", addr)
			go s.acceptReverse(ctx, l, agent.ID, service, errCh)
		}
	}
	defer func() {
		for _, l := range listeners {
			_ = l.Close()
		}
	}()
	go func() {
		<-ctx.Done()
		ln.Close()
		for _, l := range listeners {
			_ = l.Close()
		}
	}()

	dials := make(chan struct{}, s.cfg.MaxPendingDials)
	probes := make(chan struct{}, s.cfg.MaxPendingDials)
	s.log.Info("startup", "role", "server", "listener_address", s.cfg.ListenUDP, "agents", len(s.cfg.Agents))
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil {
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
		if !s.register(agentID, conn) {
			select {
			case probes <- struct{}{}:
			default:
				conn.CloseWithError(2, "probe capacity exceeded")
				continue
			}
			s.log.Info("probe_connection_accepted", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
			go func() {
				defer func() { <-probes }()
				s.handleProbeConnection(ctx, agentID, conn, dials)
			}()
			continue
		}
		s.metrics.QUICConnections.Add(1)
		s.log.Info("agent_connected", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
		go s.handleAgent(ctx, agentID, conn, dials)
	}
}

func validateConfig(cfg Config) error {
	if cfg.ListenUDP == "" {
		return fmt.Errorf("listen_udp is required")
	}
	if err := config.ValidateUDPAddress(cfg.ListenUDP); err != nil {
		return err
	}
	if len(cfg.Agents) == 0 {
		return fmt.Errorf("at least one agent is required")
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
		s.log.Info("agent_disconnected", "role", "server", "agent_id", agentID, "peer_address", conn.RemoteAddr().String())
	}()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
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
				return
			}
			s.handleProbe(ctx, agentID, stream, frame, dials)
		}(stream)
	}
}

func (s *Server) handleAgentStream(ctx context.Context, agentID string, stream *quic.Stream, dials chan struct{}) {
	start := time.Now()
	_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	frame, err := protocol.ReadFrame(stream)
	if err != nil {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
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
		defer func() { <-dials }()
	default:
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorOverloaded})
		_ = stream.Close()
		return
	}
	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorDialFailed})
		_ = stream.Close()
		s.log.Warn("target_dial_failed", "role", "server", "agent_id", agentID, "service", frame.Service, "direction", "forward", "target_address", target)
		return
	}
	defer tcpConn.Close()
	if err := protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeOK}); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	count := transport.Pipe(tcpConn, stream)
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

func (s *Server) acceptReverse(ctx context.Context, ln net.Listener, agentID, service string, errCh chan<- error) {
	for {
		tcpConn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			errCh <- err
			return
		}
		go s.handleReverseTCP(ctx, tcpConn, agentID, service, ln.Addr().String())
	}
}

func (s *Server) handleReverseTCP(ctx context.Context, tcpConn net.Conn, agentID, service, listener string) {
	defer tcpConn.Close()
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
		s.log.Warn("reverse_rejected", "role", "server", "agent_id", agentID, "service", service, "reason", "open_stream_failed")
		return
	}
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
	s.log.Info("connection_closed", "role", "server", "agent_id", agentID, "service", service, "direction", "reverse", "connection_id", connectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.AToB, "bytes_target_to_client", count.BToA)
}

func newConnectionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
