package forward

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/transport"
)

type Client struct {
	cfg     config.Forward
	secret  []byte
	log     *logging.Logger
	metrics *metrics.Metrics

	mu   sync.Mutex
	conn *quic.Conn
}

func New(cfg config.Forward, secret []byte, log *logging.Logger, metrics *metrics.Metrics) *Client {
	return &Client{cfg: cfg, secret: secret, log: log, metrics: metrics}
}

func (c *Client) Run(ctx context.Context) error {
	listens, err := config.NormalizeForwardListens(c.cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, c.cfg.MaxConnections)
	errCh := make(chan error, len(listens))
	var lns []net.Listener
	for _, listen := range listens {
		ln, err := net.Listen("tcp", listen.Address)
		if err != nil {
			for _, opened := range lns {
				_ = opened.Close()
			}
			return err
		}
		lns = append(lns, ln)
		c.log.Info("startup", "role", "forward", "route", listen.Route, "listener_address", listen.Address, "peer_address", c.cfg.Peer)
		go c.acceptLoop(ctx, ln, listen.Route, sem, errCh)
	}
	defer func() {
		for _, ln := range lns {
			_ = ln.Close()
		}
		c.closeQUIC()
	}()
	go func() {
		<-ctx.Done()
		for _, ln := range lns {
			_ = ln.Close()
		}
		c.closeQUIC()
	}()

	select {
	case err := <-errCh:
		cancel()
		if err == nil {
			c.log.Info("shutdown", "role", "forward")
			return nil
		}
		return err
	case <-ctx.Done():
		c.log.Info("shutdown", "role", "forward")
		return nil
	}
}

func (c *Client) acceptLoop(ctx context.Context, ln net.Listener, route string, sem chan struct{}, errCh chan<- error) {
	listenerAddress := ln.Addr().String()
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				errCh <- nil
				return
			}
			errCh <- fmt.Errorf("accept %s: %w", ln.Addr().String(), err)
			return
		}
		select {
		case sem <- struct{}{}:
			c.metrics.ActiveTCPConnections.Add(1)
			c.metrics.TCPConnectionsTotal.Add(1)
			c.log.Info("tcp_accepted", "role", "forward", "route", route, "listener_address", listenerAddress, "peer_address", conn.RemoteAddr().String())
			go func() {
				defer func() {
					<-sem
					c.metrics.ActiveTCPConnections.Add(-1)
				}()
				c.handleTCP(ctx, conn, route, listenerAddress)
			}()
		default:
			c.metrics.StreamErrorsTotal.Add(1)
			c.log.Warn("rate_or_limit_rejected", "role", "forward", "route", route, "reason", "max_connections")
			_ = conn.Close()
		}
	}
}

func (c *Client) handleTCP(ctx context.Context, tcpConn net.Conn, route string, listenerAddress string) {
	defer tcpConn.Close()
	start := time.Now()
	connectionID := newConnectionID()
	conn, err := c.getQUIC(ctx)
	if err != nil {
		c.log.Warn("quic_disconnected", "role", "forward", "route", route, "connection_id", connectionID, "reason", "connect_failed")
		return
	}
	openCtx, cancel := context.WithTimeout(ctx, c.cfg.ConnectTimeout)
	defer cancel()
	stream, err := conn.OpenStreamSync(openCtx)
	if err != nil {
		c.metrics.StreamErrorsTotal.Add(1)
		c.resetQUIC(conn)
		return
	}
	c.metrics.ActiveQUICStreams.Add(1)
	c.metrics.StreamsTotal.Add(1)
	defer c.metrics.ActiveQUICStreams.Add(-1)

	_ = stream.SetDeadline(time.Now().Add(c.cfg.HandshakeTimeout))
	sourceAddr := ""
	if c.cfg.SendClientAddr {
		sourceAddr = tcpConn.RemoteAddr().String()
	}
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeOpen,
		Route:        route,
		ForwardID:    c.cfg.ForwardID,
		Listener:     listenerAddress,
		SourceAddr:   sourceAddr,
		ConnectionID: connectionID,
	}); err != nil {
		c.metrics.StreamErrorsTotal.Add(1)
		_ = stream.Close()
		return
	}
	resp, err := protocol.ReadFrame(stream)
	if err != nil {
		c.metrics.StreamErrorsTotal.Add(1)
		_ = stream.Close()
		return
	}
	if resp.Type != protocol.TypeOK {
		c.metrics.StreamErrorsTotal.Add(1)
		_ = stream.Close()
		c.log.Warn("stream_rejected", "role", "forward", "route", route, "connection_id", connectionID, "reason", "exit_error")
		return
	}
	_ = stream.SetDeadline(time.Time{})
	c.log.Info("stream_opened", "role", "forward", "route", route, "connection_id", connectionID, "listener_address", listenerAddress)
	count := transport.Pipe(tcpConn, stream)
	c.metrics.BytesClientToTarget.Add(count.AToB)
	c.metrics.BytesTargetToClient.Add(count.BToA)
	c.log.Info("connection_closed", "role", "forward", "route", route, "connection_id", connectionID, "duration_ms", time.Since(start).Milliseconds(), "bytes_client_to_target", count.AToB, "bytes_target_to_client", count.BToA)
}

func newConnectionID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

func (c *Client) getQUIC(ctx context.Context) (*quic.Conn, error) {
	c.mu.Lock()
	if c.conn != nil {
		conn := c.conn
		c.mu.Unlock()
		return conn, nil
	}
	c.mu.Unlock()

	serverName := auth.ExitServerName
	if config.ValidateClientID(c.cfg.ForwardID) == nil {
		serverName = auth.ExitServerNameForClient(c.cfg.ForwardID)
	}
	tlsCfg, err := auth.TLSConfigWithServerName(c.secret, auth.RoleForward, serverName)
	if err != nil {
		return nil, err
	}
	qcfg := &quic.Config{
		HandshakeIdleTimeout: c.cfg.HandshakeTimeout,
		MaxIdleTimeout:       c.cfg.IdleTimeout,
		KeepAlivePeriod:      c.cfg.KeepAlive,
	}
	dialCtx, cancel := context.WithTimeout(ctx, c.cfg.ConnectTimeout)
	defer cancel()
	conn, err := quic.DialAddr(dialCtx, c.cfg.Peer, tlsCfg, qcfg)
	if err != nil {
		c.metrics.AuthFailuresTotal.Add(1)
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		conn.CloseWithError(0, "replaced")
		return c.conn, nil
	}
	c.conn = conn
	c.metrics.QUICConnections.Add(1)
	c.log.Info("quic_connected", "role", "forward", "peer_address", c.cfg.Peer)
	go func() {
		<-conn.Context().Done()
		c.resetQUIC(conn)
	}()
	return conn, nil
}

func (c *Client) resetQUIC(conn *quic.Conn) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == conn {
		c.conn = nil
		c.metrics.QUICConnections.Add(-1)
		c.log.Info("quic_disconnected", "role", "forward", "peer_address", c.cfg.Peer)
	}
}

func (c *Client) closeQUIC() {
	c.mu.Lock()
	conn := c.conn
	c.conn = nil
	c.mu.Unlock()
	if conn != nil {
		conn.CloseWithError(0, "shutdown")
		c.metrics.QUICConnections.Add(-1)
	}
}
