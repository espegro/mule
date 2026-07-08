package agent_test

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"os"
	"testing"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/espegro/mule/internal/agent"
	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/server"
)

func TestForwardAndReverse(t *testing.T) {
	secret := testSecret(1)
	forwardTarget, stopForward := startPrefixServer(t, "forward:")
	defer stopForward()
	reverseTarget, stopReverse := startPrefixServer(t, "reverse:")
	defer stopReverse()

	serverAddr := freeUDPAddr(t)
	agentListen := freeTCPAddr(t)
	serverReverseListen := freeTCPAddr(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startServer(t, ctx, server.Config{
		ListenUDP: serverAddr,
		Agents: []server.AgentConfig{{
			ID:         "dgx",
			SecretFile: writeSecretFile(t, secret),
			Forward:    map[string]string{"ollama": forwardTarget},
			Reverse:    map[string]string{"ssh": serverReverseListen},
		}},
	})
	startAgent(t, ctx, agent.Config{
		Server:     serverAddr,
		AgentID:    "dgx",
		SecretFile: writeSecretFile(t, secret),
		Forward:    map[string]string{"ollama": agentListen},
		Reverse:    map[string]string{"ssh": reverseTarget},
	})
	waitForTCP(t, agentListen)
	waitForTCP(t, serverReverseListen)

	assertProbe(t, serverAddr, "dgx", writeSecretFile(t, secret), protocol.DirectionForward, "ollama")
	assertProbe(t, serverAddr, "dgx", writeSecretFile(t, secret), protocol.DirectionReverse, "ssh")
	assertRoundTrip(t, agentListen, "ping", "forward:ping")
	assertRoundTrip(t, serverReverseListen, "pong", "reverse:pong")
}

func TestReverseACLRejectsMissingService(t *testing.T) {
	secret := testSecret(2)
	serverAddr := freeUDPAddr(t)
	badReverseListen := freeTCPAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startServer(t, ctx, server.Config{
		ListenUDP: serverAddr,
		Agents: []server.AgentConfig{{
			ID:         "dgx",
			SecretFile: writeSecretFile(t, secret),
			Reverse:    map[string]string{"ssh": badReverseListen},
		}},
	})
	startAgent(t, ctx, agent.Config{
		Server:     serverAddr,
		AgentID:    "dgx",
		SecretFile: writeSecretFile(t, secret),
		Reverse:    map[string]string{"other": "127.0.0.1:1"},
	})
	waitForTCP(t, badReverseListen)
	assertClosedWithoutResponse(t, badReverseListen)
}

func startServer(t *testing.T, ctx context.Context, cfg server.Config) {
	t.Helper()
	server.ApplyDefaults(&cfg)
	cfg.HandshakeTimeout = time.Second
	cfg.DialTimeout = time.Second
	go func() {
		err := server.New(cfg, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("server failed: %v", err)
		}
	}()
	time.Sleep(150 * time.Millisecond)
}

func startAgent(t *testing.T, ctx context.Context, cfg agent.Config) {
	t.Helper()
	agent.ApplyDefaults(&cfg)
	cfg.ConnectTimeout = time.Second
	cfg.HandshakeTimeout = time.Second
	cfg.ReconnectDelay = 100 * time.Millisecond
	go func() {
		secret, err := auth.LoadSecretFile(cfg.SecretFile)
		if err != nil {
			t.Errorf("load agent secret: %v", err)
			return
		}
		err = agent.New(cfg, secret, logging.New("text", "error"), metrics.New()).Run(ctx)
		if err != nil && ctx.Err() == nil {
			t.Errorf("agent failed: %v", err)
		}
	}()
	time.Sleep(250 * time.Millisecond)
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
	return ln.Addr().String(), func() { _ = ln.Close() }
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

func assertProbe(t *testing.T, serverAddr, agentID, secretFile string, direction protocol.Direction, service string) {
	t.Helper()
	secret, err := auth.LoadSecretFile(secretFile)
	if err != nil {
		t.Fatal(err)
	}
	tlsCfg, err := auth.TLSConfigWithServerName(secret, auth.RoleForward, auth.ExitServerNameForClient(agentID))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, err := quic.DialAddr(ctx, serverAddr, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: time.Second,
		MaxIdleTimeout:       5 * time.Second,
		KeepAlivePeriod:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseWithError(0, "test done")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeProbe,
		Direction:    direction,
		Service:      service,
		PeerID:       agentID,
		ConnectionID: "probe",
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := protocol.ReadFrame(stream)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Type != protocol.TypeOK {
		t.Fatalf("probe failed: %+v", resp)
	}
}

func assertClosedWithoutResponse(t *testing.T, addr string) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("test")); err != nil {
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
		t.Fatalf("connection timed out instead of closing: %v", err)
	}
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

func testSecret(seed byte) []byte {
	secret := make([]byte, auth.MinSecretBytes)
	for i := range secret {
		secret[i] = seed + byte(i)
	}
	return secret
}

func writeSecretFile(t *testing.T, secret []byte) string {
	t.Helper()
	path := t.TempDir() + "/secret.key"
	if err := os.WriteFile(path, []byte(base64.StdEncoding.EncodeToString(secret)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
