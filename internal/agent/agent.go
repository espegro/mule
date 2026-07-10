package agent

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"gopkg.in/yaml.v3"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/transport"
)

const (
	maxReconnectDelay       = time.Minute
	stableConnectionMinimum = time.Minute
)

type Config struct {
	Server           string
	AgentID          string
	SecretFile       string
	Forward          map[string]string
	Reverse          map[string]string
	ConnectTimeout   time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxConnections   int
	KeepAlive        time.Duration
	ReconnectDelay   time.Duration
	SendClientAddr   bool
}

type FileConfig struct {
	Server           string            `yaml:"server"`
	AgentID          string            `yaml:"agent_id"`
	SecretFile       string            `yaml:"secret_file"`
	Forward          map[string]string `yaml:"forward"`
	Reverse          map[string]string `yaml:"reverse"`
	ConnectTimeout   string            `yaml:"connect_timeout"`
	HandshakeTimeout string            `yaml:"handshake_timeout"`
	IdleTimeout      string            `yaml:"idle_timeout"`
	MaxConnections   int               `yaml:"max_connections"`
	KeepAlive        string            `yaml:"keepalive"`
	ReconnectDelay   string            `yaml:"reconnect_delay"`
	SendClientAddr   bool              `yaml:"send_client_addr"`
}

type Agent struct {
	cfg     Config
	secret  []byte
	log     *logging.Logger
	metrics *metrics.Metrics

	mu   sync.Mutex
	conn *quic.Conn
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
		Server:         fc.Server,
		AgentID:        fc.AgentID,
		SecretFile:     fc.SecretFile,
		Forward:        fc.Forward,
		Reverse:        fc.Reverse,
		MaxConnections: fc.MaxConnections,
		SendClientAddr: fc.SendClientAddr,
	}
	var errp error
	if fc.ConnectTimeout != "" {
		cfg.ConnectTimeout, errp = time.ParseDuration(fc.ConnectTimeout)
		if errp != nil {
			return Config{}, errp
		}
	}
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
	if fc.KeepAlive != "" {
		cfg.KeepAlive, errp = time.ParseDuration(fc.KeepAlive)
		if errp != nil {
			return Config{}, errp
		}
	}
	if fc.ReconnectDelay != "" {
		cfg.ReconnectDelay, errp = time.ParseDuration(fc.ReconnectDelay)
		if errp != nil {
			return Config{}, errp
		}
	}
	ApplyDefaults(&cfg)
	return cfg, nil
}

func ApplyDefaults(cfg *Config) {
	if cfg.ConnectTimeout == 0 {
		cfg.ConnectTimeout = 10 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.MaxConnections == 0 {
		cfg.MaxConnections = 100
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = 20 * time.Second
	}
	if cfg.ReconnectDelay == 0 {
		cfg.ReconnectDelay = time.Second
	}
}

func New(cfg Config, secret []byte, log *logging.Logger, metrics *metrics.Metrics) *Agent {
	return &Agent{cfg: cfg, secret: secret, log: log, metrics: metrics}
}

func (a *Agent) Run(ctx context.Context) error {
	ApplyDefaults(&a.cfg)
	if err := validateConfig(a.cfg); err != nil {
		return err
	}
	go a.maintainConnection(ctx)

	sem := make(chan struct{}, a.cfg.MaxConnections)
	var listeners []net.Listener
	for service, addr := range a.cfg.Forward {
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return err
		}
		listeners = append(listeners, ln)
		a.log.Info("forward_listener_started", "role", "agent", "agent_id", a.cfg.AgentID, "service", service, "listener_address", addr)
		go a.acceptForward(ctx, ln, service, sem)
	}
	defer func() {
		for _, ln := range listeners {
			_ = ln.Close()
		}
		a.closeConn()
	}()
	<-ctx.Done()
	for _, ln := range listeners {
		_ = ln.Close()
	}
	a.closeConn()
	return nil
}

func validateConfig(cfg Config) error {
	if cfg.Server == "" || cfg.AgentID == "" || cfg.SecretFile == "" {
		return fmt.Errorf("server, agent_id and secret_file are required")
	}
	if err := config.ValidateUDPAddress(cfg.Server); err != nil {
		return err
	}
	if err := config.ValidateClientID(cfg.AgentID); err != nil {
		return err
	}
	if len(cfg.Forward) == 0 && len(cfg.Reverse) == 0 {
		return fmt.Errorf("at least one forward or reverse service is required")
	}
	if cfg.ReconnectDelay <= 0 {
		return fmt.Errorf("reconnect_delay must be positive")
	}
	for service, addr := range cfg.Forward {
		if err := config.ValidateRouteID(service); err != nil {
			return err
		}
		if err := config.ValidateListenAddress(addr); err != nil {
			return err
		}
	}
	for service, addr := range cfg.Reverse {
		if err := config.ValidateRouteID(service); err != nil {
			return err
		}
		if err := config.ValidateTCPAddress(addr); err != nil {
			return err
		}
	}
	return nil
}

func (a *Agent) maintainConnection(ctx context.Context) {
	reconnectDelay := a.cfg.ReconnectDelay
	for ctx.Err() == nil {
		conn, err := a.dial(ctx)
		if err != nil {
			a.log.Warn("server_connect_failed", "role", "agent", "agent_id", a.cfg.AgentID, "reason", "connect_failed", "retry_delay_ms", reconnectDelay.Milliseconds())
			if !waitForReconnect(ctx, reconnectDelay) {
				return
			}
			reconnectDelay = nextReconnectDelay(reconnectDelay, a.cfg.ReconnectDelay)
			continue
		}
		connectedAt := time.Now()
		a.setConn(conn)
		a.metrics.QUICConnections.Add(1)
		a.log.Info("server_connected", "role", "agent", "agent_id", a.cfg.AgentID, "peer_address", a.cfg.Server)
		a.acceptReverse(ctx, conn)
		a.clearConn(conn)
		a.metrics.QUICConnections.Add(-1)
		a.log.Info("server_disconnected", "role", "agent", "agent_id", a.cfg.AgentID, "peer_address", a.cfg.Server)
		if time.Since(connectedAt) >= stableConnectionMinimum {
			reconnectDelay = a.cfg.ReconnectDelay
		}
		if !waitForReconnect(ctx, reconnectDelay) {
			return
		}
		reconnectDelay = nextReconnectDelay(reconnectDelay, a.cfg.ReconnectDelay)
	}
}

func waitForReconnect(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextReconnectDelay(current, base time.Duration) time.Duration {
	limit := maxReconnectDelay
	if base > limit {
		limit = base
	}
	if current >= limit || current > limit/2 {
		return limit
	}
	return current * 2
}

func (a *Agent) dial(ctx context.Context) (*quic.Conn, error) {
	tlsCfg, err := auth.TLSConfigWithServerName(a.secret, auth.RoleForward, auth.ExitServerNameForClient(a.cfg.AgentID))
	if err != nil {
		return nil, err
	}
	dialCtx, cancel := context.WithTimeout(ctx, a.cfg.ConnectTimeout)
	defer cancel()
	return quic.DialAddr(dialCtx, a.cfg.Server, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: a.cfg.HandshakeTimeout,
		MaxIdleTimeout:       a.cfg.IdleTimeout,
		KeepAlivePeriod:      a.cfg.KeepAlive,
	})
}

func (a *Agent) setConn(conn *quic.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
}

func (a *Agent) clearConn(conn *quic.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.conn == conn {
		a.conn = nil
	}
}

func (a *Agent) getConn(ctx context.Context) (*quic.Conn, error) {
	deadline := time.Now().Add(a.cfg.ConnectTimeout)
	for {
		a.mu.Lock()
		conn := a.conn
		a.mu.Unlock()
		if conn != nil {
			return conn, nil
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("agent is not connected")
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func (a *Agent) closeConn() {
	a.mu.Lock()
	conn := a.conn
	a.conn = nil
	a.mu.Unlock()
	if conn != nil {
		conn.CloseWithError(0, "shutdown")
	}
}

func (a *Agent) acceptForward(ctx context.Context, ln net.Listener, service string, sem chan struct{}) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case sem <- struct{}{}:
			go func() {
				defer func() { <-sem }()
				a.handleForwardTCP(ctx, conn, service, ln.Addr().String())
			}()
		default:
			_ = conn.Close()
		}
	}
}

func (a *Agent) handleForwardTCP(ctx context.Context, tcpConn net.Conn, service, listener string) {
	defer tcpConn.Close()
	start := time.Now()
	connectionID := newConnectionID()
	conn, err := a.getConn(ctx)
	if err != nil {
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, a.cfg.HandshakeTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(openCtx)
	if err != nil {
		return
	}
	_ = stream.SetDeadline(time.Now().Add(a.cfg.HandshakeTimeout))
	sourceAddr := ""
	if a.cfg.SendClientAddr {
		sourceAddr = tcpConn.RemoteAddr().String()
	}
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeOpen,
		Direction:    protocol.DirectionForward,
		Service:      service,
		PeerID:       a.cfg.AgentID,
		Listener:     listener,
		SourceAddr:   sourceAddr,
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
	a.log.Info("connection_closed", "role", "agent", "agent_id", a.cfg.AgentID, "service", service, "direction", "forward", "connection_id", connectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.AToB, "bytes_target_to_client", count.BToA)
}

func (a *Agent) acceptReverse(ctx context.Context, conn *quic.Conn) {
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		go a.handleReverseStream(ctx, stream)
	}
}

func (a *Agent) handleReverseStream(ctx context.Context, stream *quic.Stream) {
	start := time.Now()
	_ = stream.SetDeadline(time.Now().Add(a.cfg.HandshakeTimeout))
	frame, err := protocol.ReadFrame(stream)
	if err != nil || frame.Type != protocol.TypeOpen || frame.Direction != protocol.DirectionReverse {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	target, ok := a.cfg.Reverse[frame.Service]
	if !ok {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		return
	}
	dialer := net.Dialer{Timeout: a.cfg.ConnectTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorDialFailed})
		_ = stream.Close()
		return
	}
	defer tcpConn.Close()
	if err := protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeOK}); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	count := transport.Pipe(tcpConn, stream)
	a.log.Info("connection_closed", "role", "agent", "agent_id", a.cfg.AgentID, "service", frame.Service, "direction", "reverse", "connection_id", frame.ConnectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.BToA, "bytes_target_to_client", count.AToB)
}

func newConnectionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}
