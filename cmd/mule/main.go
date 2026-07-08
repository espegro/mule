package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	muleexit "github.com/espegro/mule/internal/exit"
	"github.com/espegro/mule/internal/forward"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
)

var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return errors.New("missing command")
	}
	switch args[0] {
	case "forward":
		return runForward(args[1:])
	case "exit":
		return runExit(args[1:])
	case "keygen":
		return runKeygen(args[1:])
	case "check":
		return runCheck(args[1:])
	case "probe":
		return runProbe(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mule forward|exit|probe|keygen|check|version")
}

func runForward(args []string) error {
	fs := flag.NewFlagSet("forward", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfg := config.Forward{}
	var listens mappingFlags
	addCommonFlags(fs, &cfg.Common)
	fs.StringVar(&cfg.ListenTCP, "listen-tcp", "", "TCP listen address for the default route")
	fs.Var(&listens, "listen", "route=TCP listen address; may be repeated")
	fs.StringVar(&cfg.Peer, "peer", "", "UDP/QUIC peer address")
	fs.StringVar(&cfg.ForwardID, "forward-id", "", "non-sensitive forward instance id for exit logs")
	fs.BoolVar(&cfg.SendClientAddr, "send-client-addr", false, "include original TCP client address in OPEN metadata")
	fs.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 10*time.Second, "QUIC connect timeout")
	fs.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 10*time.Second, "stream/TLS handshake timeout")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 5*time.Minute, "QUIC idle timeout, 0 disables")
	fs.IntVar(&cfg.MaxConnections, "max-connections", 100, "maximum concurrent TCP connections")
	fs.DurationVar(&cfg.KeepAlive, "keepalive", 20*time.Second, "QUIC keepalive period")
	if err := fs.Parse(args); err != nil {
		return err
	}
	for _, raw := range listens {
		listen, err := config.ParseRouteListen(raw)
		if err != nil {
			return err
		}
		cfg.Listens = append(cfg.Listens, listen)
	}
	if cfg.Peer == "" || cfg.SecretFile == "" {
		return errors.New("--peer and --secret-file are required")
	}
	if err := validateCommon(cfg.Common); err != nil {
		return err
	}
	if cfg.ForwardID == "" {
		if hostname, err := os.Hostname(); err == nil && config.ValidateForwardID(hostname) == nil {
			cfg.ForwardID = hostname
		}
	}
	if err := config.ValidateForwardID(cfg.ForwardID); err != nil {
		return err
	}
	if _, err := config.NormalizeForwardListens(cfg); err != nil {
		return err
	}
	if err := config.ValidateUDPAddress(cfg.Peer); err != nil {
		return err
	}
	if cfg.MaxConnections < 1 {
		return errors.New("--max-connections must be at least 1")
	}
	secret, err := auth.LoadSecretFile(cfg.SecretFile)
	if err != nil {
		return err
	}
	log := logging.New(cfg.LogFormat, cfg.LogLevel)
	m := metrics.New()
	return runWithMetrics(cfg.Common, m, func(ctx context.Context) error {
		return forward.New(cfg, secret, log, m).Run(ctx)
	})
}

func runExit(args []string) error {
	fs := flag.NewFlagSet("exit", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	cfg := config.Exit{}
	var routes mappingFlags
	var clients mappingFlags
	var configPath string
	addCommonFlags(fs, &cfg.Common)
	fs.StringVar(&configPath, "config", "", "YAML config file")
	fs.StringVar(&cfg.ListenUDP, "listen-udp", "", "UDP/QUIC listen address")
	fs.StringVar(&cfg.Target, "target", "", "fixed TCP target address for the default route")
	fs.Var(&routes, "route", "route=target or client:route=target; may be repeated")
	fs.Var(&clients, "client", "client-id=secret-file for multi-client mode; may be repeated")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", 10*time.Second, "target dial timeout")
	fs.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 10*time.Second, "stream/TLS handshake timeout")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 5*time.Minute, "QUIC idle timeout, 0 disables")
	fs.IntVar(&cfg.MaxStreams, "max-streams", 100, "maximum concurrent streams")
	fs.IntVar(&cfg.MaxPendingDials, "max-pending-dials", 20, "maximum concurrent target dials")
	fs.DurationVar(&cfg.KeepAlive, "keepalive", 20*time.Second, "QUIC keepalive period")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if configPath != "" {
		loaded, err := config.LoadExitFile(configPath)
		if err != nil {
			return err
		}
		cfg = loaded
	}
	for _, raw := range clients {
		client, err := config.ParseClient(raw)
		if err != nil {
			return err
		}
		cfg.Clients = append(cfg.Clients, client)
	}
	if cfg.Routes == nil {
		cfg.Routes = make(map[string]string)
	}
	if cfg.ClientRoutes == nil {
		cfg.ClientRoutes = make(map[string]map[string]string)
	}
	for _, raw := range routes {
		if clientID, route, target, err := config.ParseClientRouteTarget(raw); err == nil {
			if cfg.ClientRoutes[clientID] == nil {
				cfg.ClientRoutes[clientID] = make(map[string]string)
			}
			if _, ok := cfg.ClientRoutes[clientID][route]; ok {
				return fmt.Errorf("duplicate route %q for client %q", route, clientID)
			}
			cfg.ClientRoutes[clientID][route] = target
		} else {
			route, target, err := config.ParseRouteTarget(raw)
			if err != nil {
				return err
			}
			if _, ok := cfg.Routes[route]; ok {
				return fmt.Errorf("duplicate route %q", route)
			}
			cfg.Routes[route] = target
		}
	}
	config.ApplyExitDefaults(&cfg)
	if cfg.ListenUDP == "" {
		return errors.New("--listen-udp is required")
	}
	if !config.MultiClientMode(cfg) && cfg.SecretFile == "" {
		return errors.New("--secret-file is required in simple mode")
	}
	if err := validateCommon(cfg.Common); err != nil {
		return err
	}
	if err := config.ValidateUDPAddress(cfg.ListenUDP); err != nil {
		return err
	}
	if config.MultiClientMode(cfg) {
		if _, err := config.NormalizeClientRoutes(cfg); err != nil {
			return err
		}
	} else {
		if _, err := config.NormalizeExitRoutes(cfg); err != nil {
			return err
		}
	}
	if cfg.MaxStreams < 1 || cfg.MaxPendingDials < 1 {
		return errors.New("--max-streams and --max-pending-dials must be at least 1")
	}
	var secret []byte
	if !config.MultiClientMode(cfg) {
		var err error
		secret, err = auth.LoadSecretFile(cfg.SecretFile)
		if err != nil {
			return err
		}
	}
	log := logging.New(cfg.LogFormat, cfg.LogLevel)
	m := metrics.New()
	return runWithMetrics(cfg.Common, m, func(ctx context.Context) error {
		return muleexit.New(cfg, secret, log, m).Run(ctx)
	})
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var secretFile, peer, route, forwardID string
	var connectTimeout, handshakeTimeout, idleTimeout, keepalive time.Duration
	fs.StringVar(&secretFile, "secret-file", "", "secret file path")
	fs.StringVar(&peer, "peer", "", "UDP/QUIC peer address")
	fs.StringVar(&route, "route", config.DefaultRoute, "route to probe")
	fs.StringVar(&forwardID, "forward-id", "", "client/forward id for SNI and logs")
	fs.DurationVar(&connectTimeout, "connect-timeout", 10*time.Second, "QUIC connect timeout")
	fs.DurationVar(&handshakeTimeout, "handshake-timeout", 10*time.Second, "stream handshake timeout")
	fs.DurationVar(&idleTimeout, "idle-timeout", 30*time.Second, "QUIC idle timeout")
	fs.DurationVar(&keepalive, "keepalive", 20*time.Second, "QUIC keepalive period")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if secretFile == "" || peer == "" {
		return errors.New("--secret-file and --peer are required")
	}
	if err := config.ValidateUDPAddress(peer); err != nil {
		return err
	}
	if err := config.ValidateRouteID(route); err != nil {
		return err
	}
	if forwardID == "" {
		if hostname, err := os.Hostname(); err == nil && config.ValidateForwardID(hostname) == nil {
			forwardID = hostname
		}
	}
	if err := config.ValidateForwardID(forwardID); err != nil {
		return err
	}
	secret, err := auth.LoadSecretFile(secretFile)
	if err != nil {
		return err
	}
	serverName := auth.ExitServerName
	if config.ValidateClientID(forwardID) == nil {
		serverName = auth.ExitServerNameForClient(forwardID)
	}
	tlsCfg, err := auth.TLSConfigWithServerName(secret, auth.RoleForward, serverName)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	conn, err := quic.DialAddr(ctx, peer, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: handshakeTimeout,
		MaxIdleTimeout:       idleTimeout,
		KeepAlivePeriod:      keepalive,
	})
	if err != nil {
		return fmt.Errorf("probe connect failed: %w", err)
	}
	defer conn.CloseWithError(0, "probe complete")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return fmt.Errorf("probe stream failed: %w", err)
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeOpen,
		Route:        route,
		ForwardID:    forwardID,
		ConnectionID: "probe",
	}); err != nil {
		return fmt.Errorf("probe open failed: %w", err)
	}
	frame, err := protocol.ReadFrame(stream)
	if err != nil {
		return fmt.Errorf("probe response failed: %w", err)
	}
	if frame.Type != protocol.TypeOK {
		return fmt.Errorf("probe rejected: error code %d", frame.Code)
	}
	fmt.Printf("probe ok: peer=%s route=%s forward_id=%s\n", peer, route, forwardID)
	return nil
}

type mappingFlags []string

func (m *mappingFlags) String() string {
	return fmt.Sprint([]string(*m))
}

func (m *mappingFlags) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func runKeygen(args []string) error {
	fs := flag.NewFlagSet("keygen", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	out := fs.String("out", "", "output secret file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *out == "" {
		return errors.New("--out is required")
	}
	return auth.GenerateSecretFile(*out)
}

func runCheck(args []string) error {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	secretFile := fs.String("secret-file", "", "secret file to validate")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *secretFile == "" {
		return errors.New("--secret-file is required")
	}
	if _, err := auth.LoadSecretFile(*secretFile); err != nil {
		return err
	}
	fmt.Println("secret file ok")
	return nil
}

func addCommonFlags(fs *flag.FlagSet, cfg *config.Common) {
	fs.StringVar(&cfg.SecretFile, "secret-file", "", "secret file path")
	fs.StringVar(&cfg.LogFormat, "log-format", "text", "text or json")
	fs.StringVar(&cfg.LogLevel, "log-level", "info", "debug, info, warn or error")
	fs.StringVar(&cfg.MetricsListen, "metrics-listen", "", "Prometheus metrics listen address")
	fs.DurationVar(&cfg.ShutdownTimeout, "shutdown-timeout", 30*time.Second, "graceful shutdown timeout")
}

func validateCommon(cfg config.Common) error {
	if err := config.ValidateLogFormat(cfg.LogFormat); err != nil {
		return err
	}
	if err := config.ValidateLogLevel(cfg.LogLevel); err != nil {
		return err
	}
	if cfg.MetricsListen != "" {
		if err := config.ValidateListenAddress(cfg.MetricsListen); err != nil {
			return err
		}
	}
	if cfg.ShutdownTimeout <= 0 {
		return errors.New("--shutdown-timeout must be positive")
	}
	return nil
}

func runWithMetrics(common config.Common, m *metrics.Metrics, fn func(context.Context) error) error {
	signalCtx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	ctx, cancel := context.WithCancel(signalCtx)
	defer cancel()
	errCh := make(chan error, 2)
	if common.MetricsListen != "" {
		go func() { errCh <- metrics.Serve(ctx, common.MetricsListen, m) }()
	}
	go func() { errCh <- fn(ctx) }()

	select {
	case err := <-errCh:
		cancel()
		return err
	case <-signalCtx.Done():
		cancel()
		timeout := time.NewTimer(common.ShutdownTimeout)
		defer timeout.Stop()
		select {
		case err := <-errCh:
			return err
		case <-timeout.C:
			return errors.New("shutdown timed out")
		}
	}
}
