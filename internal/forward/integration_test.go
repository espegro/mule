package forward_test

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	muleexit "github.com/espegro/mule/internal/exit"
	"github.com/espegro/mule/internal/forward"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
)

func TestTunnelEcho(t *testing.T) {
	secret := testSecret(1)
	targetAddr, stopEcho := startEcho(t)
	defer stopEcho()

	exitAddr := freeUDPAddr(t)
	forwardAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startExit(t, ctx, exitAddr, targetAddr, secret)
	startForward(t, ctx, forwardAddr, exitAddr, secret)
	waitForTCP(t, forwardAddr)

	conn, err := net.DialTimeout("tcp", forwardAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, 5)
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Fatalf("got %q, want hello", got)
	}
}

func TestTunnelMultipleRoutes(t *testing.T) {
	secret := testSecret(3)
	webTarget, stopWeb := startPrefixServer(t, "web:")
	defer stopWeb()
	sshTarget, stopSSH := startPrefixServer(t, "ssh:")
	defer stopSSH()

	exitAddr := freeUDPAddr(t)
	webForwardAddr := freeTCPAddr(t)
	sshForwardAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startExitRoutes(t, ctx, exitAddr, map[string]string{
		"web": webTarget,
		"ssh": sshTarget,
	}, secret)
	startForwardRoutes(t, ctx, []config.RouteListen{
		{Route: "web", Address: webForwardAddr},
		{Route: "ssh", Address: sshForwardAddr},
	}, exitAddr, secret)
	waitForTCP(t, webForwardAddr)
	waitForTCP(t, sshForwardAddr)

	assertRoundTrip(t, webForwardAddr, "ping", "web:ping")
	assertRoundTrip(t, sshForwardAddr, "pong", "ssh:pong")
}

func TestWrongSecretDoesNotTunnel(t *testing.T) {
	targetAddr, stopEcho := startEcho(t)
	defer stopEcho()

	exitAddr := freeUDPAddr(t)
	forwardAddr := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startExit(t, ctx, exitAddr, targetAddr, testSecret(1))
	startForward(t, ctx, forwardAddr, exitAddr, testSecret(2))
	waitForTCP(t, forwardAddr)

	conn, err := net.DialTimeout("tcp", forwardAddr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("hello")); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1)
	_, err = conn.Read(buf)
	if err == nil {
		t.Fatal("expected closed connection")
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		t.Fatalf("connection timed out instead of closing after failed auth: %v", err)
	}
}

func startExit(t *testing.T, ctx context.Context, listen, target string, secret []byte) {
	t.Helper()
	cfg := config.Exit{
		ListenUDP:        listen,
		Target:           target,
		DialTimeout:      time.Second,
		HandshakeTimeout: time.Second,
		IdleTimeout:      time.Minute,
		MaxStreams:       10,
		MaxPendingDials:  5,
		KeepAlive:        20 * time.Second,
	}
	go func() {
		err := muleexit.New(cfg, secret, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("exit failed: %v", err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
}

func startExitRoutes(t *testing.T, ctx context.Context, listen string, routes map[string]string, secret []byte) {
	t.Helper()
	cfg := config.Exit{
		ListenUDP:        listen,
		Routes:           routes,
		DialTimeout:      time.Second,
		HandshakeTimeout: time.Second,
		IdleTimeout:      time.Minute,
		MaxStreams:       10,
		MaxPendingDials:  5,
		KeepAlive:        20 * time.Second,
	}
	go func() {
		err := muleexit.New(cfg, secret, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("exit failed: %v", err)
		}
	}()
	time.Sleep(100 * time.Millisecond)
}

func startForward(t *testing.T, ctx context.Context, listen, peer string, secret []byte) {
	t.Helper()
	cfg := config.Forward{
		ListenTCP:        listen,
		Peer:             peer,
		ConnectTimeout:   time.Second,
		HandshakeTimeout: time.Second,
		IdleTimeout:      time.Minute,
		MaxConnections:   10,
		KeepAlive:        20 * time.Second,
	}
	go func() {
		err := forward.New(cfg, secret, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("forward failed: %v", err)
		}
	}()
}

func startForwardRoutes(t *testing.T, ctx context.Context, listens []config.RouteListen, peer string, secret []byte) {
	t.Helper()
	cfg := config.Forward{
		Listens:          listens,
		Peer:             peer,
		ConnectTimeout:   time.Second,
		HandshakeTimeout: time.Second,
		IdleTimeout:      time.Minute,
		MaxConnections:   10,
		KeepAlive:        20 * time.Second,
	}
	go func() {
		err := forward.New(cfg, secret, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("forward failed: %v", err)
		}
	}()
}

func startEcho(t *testing.T) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
	}
}

func startPrefixServer(t *testing.T, prefix string) (string, func()) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				buf := make([]byte, 4096)
				n, err := conn.Read(buf)
				if err != nil {
					return
				}
				_, _ = conn.Write([]byte(prefix + string(buf[:n])))
			}()
		}
	}()
	return ln.Addr().String(), func() {
		_ = ln.Close()
	}
}

func assertRoundTrip(t *testing.T, addr, msg, want string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte(msg)); err != nil {
		t.Fatal(err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(conn, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func freeTCPAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func freeUDPAddr(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := conn.LocalAddr().String()
	_ = conn.Close()
	return addr
}

func waitForTCP(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", addr)
}

func testSecret(seed byte) []byte {
	secret := make([]byte, auth.MinSecretBytes)
	for i := range secret {
		secret[i] = seed + byte(i)
	}
	return secret
}
