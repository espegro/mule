package exit

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/transport"
)

type Server struct {
	cfg     config.Exit
	secret  []byte
	log     *logging.Logger
	metrics *metrics.Metrics
}

type routeAuthorizer struct {
	simple        map[string]string
	byClient      map[string]map[string]string
	authenticator *auth.MultiClientAuthenticator
}

func New(cfg config.Exit, secret []byte, log *logging.Logger, metrics *metrics.Metrics) *Server {
	return &Server{cfg: cfg, secret: secret, log: log, metrics: metrics}
}

func (s *Server) Run(ctx context.Context) error {
	tlsCfg, routes, err := s.prepareAuth()
	if err != nil {
		return err
	}
	qcfg := &quic.Config{
		HandshakeIdleTimeout: s.cfg.HandshakeTimeout,
		MaxIdleTimeout:       s.cfg.IdleTimeout,
		KeepAlivePeriod:      s.cfg.KeepAlive,
		MaxIncomingStreams:   int64(s.cfg.MaxStreams),
	}
	ln, err := quic.ListenAddr(s.cfg.ListenUDP, tlsCfg, qcfg)
	if err != nil {
		return err
	}
	defer ln.Close()
	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	s.log.Info("startup", "role", "exit", "listener_address", s.cfg.ListenUDP, "routes", routes.count(), "multi_client", routes.multiClient())
	streams := make(chan struct{}, s.cfg.MaxStreams)
	dials := make(chan struct{}, s.cfg.MaxPendingDials)
	for {
		conn, err := ln.Accept(ctx)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, quic.ErrServerClosed) {
				s.log.Info("shutdown", "role", "exit")
				return nil
			}
			return err
		}
		s.metrics.QUICConnections.Add(1)
		clientID, ok := routes.clientID(conn.ConnectionState().TLS)
		if !ok {
			conn.CloseWithError(1, "unauthorized")
			s.metrics.AuthFailuresTotal.Add(1)
			s.log.Warn("authentication_failed", "role", "exit", "peer_address", conn.RemoteAddr().String(), "reason", "unknown_client")
			continue
		}
		s.log.Info("quic_connected", "role", "exit", "peer_address", conn.RemoteAddr().String(), "client_id", clientID)
		go s.handleConn(ctx, conn, streams, dials, routes, clientID)
	}
}

func (s *Server) prepareAuth() (*tls.Config, routeAuthorizer, error) {
	if config.MultiClientMode(s.cfg) {
		clientRoutes, err := config.NormalizeClientRoutes(s.cfg)
		if err != nil {
			return nil, routeAuthorizer{}, err
		}
		clients := make([]auth.ClientIdentity, 0, len(s.cfg.Clients))
		for _, client := range s.cfg.Clients {
			secret, err := auth.LoadSecretFile(client.SecretFile)
			if err != nil {
				return nil, routeAuthorizer{}, err
			}
			clients = append(clients, auth.ClientIdentity{ID: client.ID, Secret: secret})
		}
		authenticator, err := auth.MultiClientTLSConfig(clients)
		if err != nil {
			return nil, routeAuthorizer{}, err
		}
		return authenticator.TLSConfig, routeAuthorizer{byClient: clientRoutes, authenticator: authenticator}, nil
	}
	routes, err := config.NormalizeExitRoutes(s.cfg)
	if err != nil {
		return nil, routeAuthorizer{}, err
	}
	tlsCfg, err := auth.TLSConfig(s.secret, auth.RoleExit)
	if err != nil {
		return nil, routeAuthorizer{}, err
	}
	return tlsCfg, routeAuthorizer{simple: routes}, nil
}

func (r routeAuthorizer) count() int {
	if r.simple != nil {
		return len(r.simple)
	}
	n := 0
	for _, routes := range r.byClient {
		n += len(routes)
	}
	return n
}

func (r routeAuthorizer) multiClient() bool {
	return r.byClient != nil
}

func (r routeAuthorizer) clientID(cs tls.ConnectionState) (string, bool) {
	if r.authenticator == nil {
		return "default", true
	}
	return auth.ClientIDForTLSState(r.authenticator.ClientByPublicKey, cs)
}

func (r routeAuthorizer) target(clientID, route string) (string, bool) {
	if r.simple != nil {
		target, ok := r.simple[route]
		return target, ok
	}
	target, ok := r.byClient[clientID][route]
	return target, ok
}

func (s *Server) handleConn(ctx context.Context, conn *quic.Conn, streams chan struct{}, dials chan struct{}, routes routeAuthorizer, clientID string) {
	defer func() {
		s.metrics.QUICConnections.Add(-1)
		s.log.Info("quic_disconnected", "role", "exit", "peer_address", conn.RemoteAddr().String(), "client_id", clientID)
	}()
	for {
		stream, err := conn.AcceptStream(ctx)
		if err != nil {
			return
		}
		select {
		case streams <- struct{}{}:
			s.metrics.ActiveQUICStreams.Add(1)
			s.metrics.StreamsTotal.Add(1)
			go func() {
				defer func() {
					<-streams
					s.metrics.ActiveQUICStreams.Add(-1)
				}()
				s.handleStream(ctx, stream, dials, routes, clientID)
			}()
		default:
			s.metrics.StreamErrorsTotal.Add(1)
			go rejectOverloaded(stream)
		}
	}
}

func rejectOverloaded(stream *quic.Stream) {
	_ = stream.SetDeadline(time.Now().Add(5 * time.Second))
	frame, err := protocol.ReadFrame(stream)
	if err == nil && frame.Type == protocol.TypeOpen {
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorOverloaded})
	}
	_ = stream.Close()
}

func (s *Server) handleStream(ctx context.Context, stream *quic.Stream, dials chan struct{}, routes routeAuthorizer, clientID string) {
	start := time.Now()
	_ = stream.SetDeadline(time.Now().Add(s.cfg.HandshakeTimeout))
	frame, err := protocol.ReadFrame(stream)
	if err != nil || frame.Type != protocol.TypeOpen {
		s.metrics.StreamErrorsTotal.Add(1)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		s.log.Warn("stream_rejected", "role", "exit", "reason", "bad_open")
		return
	}
	route := frame.Route
	if route == "" {
		route = config.DefaultRoute
	}
	logFields := []any{
		"role", "exit",
		"client_id", clientID,
		"route", route,
		"connection_id", frame.ConnectionID,
		"forward_id", frame.ForwardID,
		"forward_listener", frame.Listener,
		"source_addr", frame.SourceAddr,
	}
	target, ok := routes.target(clientID, route)
	if !ok {
		s.metrics.StreamErrorsTotal.Add(1)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorUnauthorized})
		_ = stream.Close()
		s.log.Warn("stream_rejected", append(logFields, "reason", "unknown_route")...)
		return
	}
	s.log.Info("stream_opened", append(logFields, "target_address", target)...)

	select {
	case dials <- struct{}{}:
		defer func() { <-dials }()
	default:
		s.metrics.TargetDialErrors.Add(1)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorOverloaded})
		_ = stream.Close()
		s.log.Warn("rate_or_limit_rejected", append(logFields, "reason", "max_pending_dials")...)
		return
	}

	dialer := net.Dialer{Timeout: s.cfg.DialTimeout}
	tcpConn, err := dialer.DialContext(ctx, "tcp", target)
	if err != nil {
		s.metrics.TargetDialErrors.Add(1)
		_ = protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeError, Code: protocol.ErrorDialFailed})
		_ = stream.Close()
		s.log.Warn("target_dial_failed", append(logFields, "target_address", target, "duration_ms", time.Since(start).Milliseconds(), "reason", "dial_failed")...)
		return
	}
	defer tcpConn.Close()

	if err := protocol.WriteFrame(stream, protocol.Frame{Type: protocol.TypeOK}); err != nil {
		_ = stream.Close()
		return
	}
	_ = stream.SetDeadline(time.Time{})
	s.log.Info("target_dial_succeeded", append(logFields, "target_address", target)...)
	count := transport.Pipe(tcpConn, stream)
	s.metrics.BytesClientToTarget.Add(count.BToA)
	s.metrics.BytesTargetToClient.Add(count.AToB)
	s.log.Info("connection_closed", append(logFields, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.BToA, "bytes_target_to_client", count.AToB)...)
}
