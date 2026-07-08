package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/quic-go/quic-go"

	"github.com/espegro/mule/internal/agent"
	"github.com/espegro/mule/internal/auth"
	"github.com/espegro/mule/internal/config"
	"github.com/espegro/mule/internal/logging"
	"github.com/espegro/mule/internal/metrics"
	"github.com/espegro/mule/internal/protocol"
	"github.com/espegro/mule/internal/server"
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
	case "server":
		return runServer(args[1:])
	case "agent":
		return runAgent(args[1:])
	case "probe":
		return runProbe(args[1:])
	case "keygen":
		return runKeygen(args[1:])
	case "check":
		return runCheck(args[1:])
	case "version":
		fmt.Println(version)
		return nil
	default:
		usage()
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: mule server|agent|probe|keygen|check|version")
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg server.Config
	var common config.Common
	var configPath string
	var agents, forwards, reverses mappingFlags
	addCommonFlags(fs, &common)
	fs.StringVar(&configPath, "config", "", "YAML config file")
	fs.StringVar(&cfg.ListenUDP, "listen-udp", "", "UDP/QUIC listen address")
	fs.Var(&agents, "agent", "agent-id=secret-file; may be repeated")
	fs.Var(&forwards, "forward", "[agent:]service=target address; may be repeated")
	fs.Var(&reverses, "reverse", "[agent:]service=listen address; may be repeated")
	fs.DurationVar(&cfg.DialTimeout, "dial-timeout", 10*time.Second, "target dial timeout")
	fs.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 10*time.Second, "QUIC/stream handshake timeout")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 5*time.Minute, "QUIC idle timeout")
	fs.IntVar(&cfg.MaxStreams, "max-streams", 100, "maximum concurrent streams")
	fs.IntVar(&cfg.MaxPendingDials, "max-pending-dials", 20, "maximum concurrent target dials")
	fs.DurationVar(&cfg.KeepAlive, "keepalive", 20*time.Second, "QUIC keepalive period")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var err error
	if configPath != "" {
		cfg, err = server.LoadConfig(configPath)
		if err != nil {
			return err
		}
	} else {
		for _, raw := range agents {
			agentCfg, err := parseAgent(raw)
			if err != nil {
				return err
			}
			cfg.Agents = append(cfg.Agents, agentCfg)
		}
		for _, raw := range forwards {
			if err := addServerMapping(cfg.Agents, raw, true); err != nil {
				return err
			}
		}
		for _, raw := range reverses {
			if err := addServerMapping(cfg.Agents, raw, false); err != nil {
				return err
			}
		}
		server.ApplyDefaults(&cfg)
	}
	if err := validateCommon(common); err != nil {
		return err
	}
	log := logging.New(common.LogFormat, common.LogLevel)
	m := metrics.New()
	return runWithMetrics(common, m, func(ctx context.Context) error {
		return server.New(cfg, log, m).Run(ctx)
	})
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var cfg agent.Config
	var common config.Common
	var configPath string
	var forwards, reverses mappingFlags
	addCommonFlags(fs, &common)
	fs.StringVar(&configPath, "config", "", "YAML config file")
	fs.StringVar(&cfg.Server, "server", "", "server UDP/QUIC address")
	fs.StringVar(&cfg.AgentID, "agent-id", "", "agent id")
	fs.StringVar(&cfg.SecretFile, "secret-file", "", "secret file path")
	fs.Var(&forwards, "forward", "service=listen address; may be repeated")
	fs.Var(&reverses, "reverse", "service=target address; may be repeated")
	fs.BoolVar(&cfg.SendClientAddr, "send-client-addr", false, "include original TCP client address in OPEN metadata")
	fs.DurationVar(&cfg.ConnectTimeout, "connect-timeout", 10*time.Second, "QUIC connect timeout")
	fs.DurationVar(&cfg.HandshakeTimeout, "handshake-timeout", 10*time.Second, "QUIC/stream handshake timeout")
	fs.DurationVar(&cfg.IdleTimeout, "idle-timeout", 5*time.Minute, "QUIC idle timeout")
	fs.IntVar(&cfg.MaxConnections, "max-connections", 100, "maximum concurrent local TCP connections")
	fs.DurationVar(&cfg.KeepAlive, "keepalive", 20*time.Second, "QUIC keepalive period")
	fs.DurationVar(&cfg.ReconnectDelay, "reconnect-delay", time.Second, "delay between reconnect attempts")
	if err := fs.Parse(args); err != nil {
		return err
	}
	var err error
	if configPath != "" {
		cfg, err = agent.LoadConfig(configPath)
		if err != nil {
			return err
		}
	} else {
		cfg.Forward = make(map[string]string)
		cfg.Reverse = make(map[string]string)
		for _, raw := range forwards {
			service, addr, err := parseServiceMapping(raw)
			if err != nil {
				return err
			}
			cfg.Forward[service] = addr
		}
		for _, raw := range reverses {
			service, addr, err := parseServiceMapping(raw)
			if err != nil {
				return err
			}
			cfg.Reverse[service] = addr
		}
		agent.ApplyDefaults(&cfg)
	}
	if err := validateCommon(common); err != nil {
		return err
	}
	secret, err := auth.LoadSecretFile(cfg.SecretFile)
	if err != nil {
		return err
	}
	log := logging.New(common.LogFormat, common.LogLevel)
	m := metrics.New()
	return runWithMetrics(common, m, func(ctx context.Context) error {
		return agent.New(cfg, secret, log, m).Run(ctx)
	})
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	var serverAddr, agentID, secretFile, service, directionText string
	var connectTimeout, handshakeTimeout, idleTimeout, keepalive time.Duration
	fs.StringVar(&serverAddr, "server", "", "server UDP/QUIC address")
	fs.StringVar(&agentID, "agent-id", "", "agent id")
	fs.StringVar(&secretFile, "secret-file", "", "secret file path")
	fs.StringVar(&service, "service", "", "service to probe")
	fs.StringVar(&directionText, "direction", "forward", "forward or reverse")
	fs.DurationVar(&connectTimeout, "connect-timeout", 10*time.Second, "QUIC connect timeout")
	fs.DurationVar(&handshakeTimeout, "handshake-timeout", 10*time.Second, "probe handshake timeout")
	fs.DurationVar(&idleTimeout, "idle-timeout", 30*time.Second, "QUIC idle timeout")
	fs.DurationVar(&keepalive, "keepalive", 20*time.Second, "QUIC keepalive period")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if serverAddr == "" || agentID == "" || secretFile == "" || service == "" {
		return errors.New("--server, --agent-id, --secret-file and --service are required")
	}
	if err := config.ValidateUDPAddress(serverAddr); err != nil {
		return err
	}
	if err := config.ValidateClientID(agentID); err != nil {
		return err
	}
	if err := config.ValidateRouteID(service); err != nil {
		return err
	}
	direction, err := parseDirection(directionText)
	if err != nil {
		return err
	}
	secret, err := auth.LoadSecretFile(secretFile)
	if err != nil {
		return err
	}
	tlsCfg, err := auth.TLSConfigWithServerName(secret, auth.RoleForward, auth.ExitServerNameForClient(agentID))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	conn, err := quic.DialAddr(ctx, serverAddr, tlsCfg, &quic.Config{
		HandshakeIdleTimeout: handshakeTimeout,
		MaxIdleTimeout:       idleTimeout,
		KeepAlivePeriod:      keepalive,
	})
	if err != nil {
		return err
	}
	defer conn.CloseWithError(0, "probe complete")
	stream, err := conn.OpenStreamSync(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()
	_ = stream.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := protocol.WriteFrame(stream, protocol.Frame{
		Type:         protocol.TypeProbe,
		Direction:    direction,
		Service:      service,
		PeerID:       agentID,
		ConnectionID: "probe",
	}); err != nil {
		return err
	}
	resp, err := protocol.ReadFrame(stream)
	if err != nil {
		return err
	}
	if resp.Type != protocol.TypeOK {
		return fmt.Errorf("probe rejected: error code %d", resp.Code)
	}
	fmt.Printf("probe ok: server=%s agent_id=%s direction=%s service=%s\n", serverAddr, agentID, directionText, service)
	return nil
}

type mappingFlags []string

func (m *mappingFlags) String() string { return fmt.Sprint([]string(*m)) }
func (m *mappingFlags) Set(v string) error {
	*m = append(*m, v)
	return nil
}

func parseAgent(raw string) (server.AgentConfig, error) {
	id, path, ok := strings.Cut(raw, "=")
	if !ok || id == "" || path == "" {
		return server.AgentConfig{}, fmt.Errorf("agent %q must be id=secret-file", raw)
	}
	if err := config.ValidateClientID(id); err != nil {
		return server.AgentConfig{}, err
	}
	return server.AgentConfig{ID: id, SecretFile: path, Forward: make(map[string]string), Reverse: make(map[string]string)}, nil
}

func addServerMapping(agents []server.AgentConfig, raw string, forward bool) error {
	left, addr, ok := strings.Cut(raw, "=")
	if !ok || left == "" || addr == "" {
		return fmt.Errorf("mapping %q must be [agent:]service=address", raw)
	}
	agentID := ""
	service := left
	if strings.Contains(left, ":") {
		agentID, service, ok = strings.Cut(left, ":")
		if !ok || agentID == "" || service == "" {
			return fmt.Errorf("mapping %q must be [agent:]service=address", raw)
		}
	} else {
		if len(agents) != 1 {
			return fmt.Errorf("mapping %q must include agent:service when multiple agents are configured", raw)
		}
		agentID = agents[0].ID
	}
	if err := config.ValidateRouteID(service); err != nil {
		return err
	}
	for i := range agents {
		if agents[i].ID == agentID {
			if forward {
				if err := config.ValidateTCPAddress(addr); err != nil {
					return err
				}
				if agents[i].Forward == nil {
					agents[i].Forward = make(map[string]string)
				}
				agents[i].Forward[service] = addr
			} else {
				if err := config.ValidateListenAddress(addr); err != nil {
					return err
				}
				if agents[i].Reverse == nil {
					agents[i].Reverse = make(map[string]string)
				}
				agents[i].Reverse[service] = addr
			}
			return nil
		}
	}
	return fmt.Errorf("unknown agent %q", agentID)
}

func parseServiceMapping(raw string) (string, string, error) {
	service, addr, ok := strings.Cut(raw, "=")
	if !ok || service == "" || addr == "" {
		return "", "", fmt.Errorf("mapping %q must be service=address", raw)
	}
	if err := config.ValidateRouteID(service); err != nil {
		return "", "", err
	}
	return service, addr, nil
}

func parseDirection(raw string) (protocol.Direction, error) {
	switch raw {
	case "forward":
		return protocol.DirectionForward, nil
	case "reverse":
		return protocol.DirectionReverse, nil
	default:
		return 0, fmt.Errorf("direction must be forward or reverse")
	}
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
