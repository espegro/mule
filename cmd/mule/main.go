package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
		writeUsage(os.Stderr)
		return errors.New("missing command")
	}
	if args[0] == "-h" || args[0] == "--help" {
		writeUsage(os.Stdout)
		return nil
	}
	switch args[0] {
	case "help":
		return runHelp(args[1:])
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
	case "check-config":
		return runCheckConfig(args[1:])
	case "version":
		if len(args) != 1 {
			return errors.New("usage: mule version")
		}
		fmt.Println(version)
		return nil
	default:
		writeUsage(os.Stderr)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func writeUsage(w io.Writer) {
	fmt.Fprint(w, `Mule — encrypted TCP service tunnels over QUIC/UDP.

Usage:
  mule COMMAND [options]
  mule help [COMMAND]

Commands:
  agent         Connect to a server and expose configured services
  server        Accept agents and enforce per-agent service policy
  probe         Test authentication and service reachability
  check-config  Validate an agent or server YAML configuration
  keygen        Generate a shared secret file
  check         Validate a shared secret file
  version       Print the version

Start with a YAML file for persistent setups:
  mule agent --config /etc/mule/agent.yaml
  mule server --config /etc/mule/server.yaml

Run "mule help COMMAND" for command-specific options and examples.
`)
}

func runHelp(args []string) error {
	if len(args) == 0 {
		writeUsage(os.Stdout)
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: mule help [COMMAND]")
	}
	switch args[0] {
	case "agent":
		return runAgent([]string{"--help"})
	case "server":
		return runServer([]string{"--help"})
	case "probe":
		return runProbe([]string{"--help"})
	case "keygen":
		return runKeygen([]string{"--help"})
	case "check":
		return runCheck([]string{"--help"})
	case "check-config":
		return runCheckConfig([]string{"--help"})
	case "version":
		fmt.Println("Usage: mule version\n\nPrint the Mule version.")
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func setCommandUsage(fs *flag.FlagSet, text string) {
	fs.Usage = func() {
		output := fs.Output()
		fmt.Fprint(output, text)
		fmt.Fprintln(output, "\nOptions:")
		var defaults strings.Builder
		fs.SetOutput(&defaults)
		fs.PrintDefaults()
		fs.SetOutput(output)
		formatted := strings.ReplaceAll(defaults.String(), "  -", "  --")
		fmt.Fprint(output, formatted)
	}
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return err
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("unexpected positional arguments: %s", strings.Join(fs.Args(), " "))
	}
	return nil
}

func runServer(args []string) error {
	fs := flag.NewFlagSet("server", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	setCommandUsage(fs, `Usage:
  mule server --config FILE [runtime options]              (recommended)
  mule server --listen-udp ADDR --agent ID=SECRET [options]

Accept authenticated agents and route only explicitly configured services.
Each agent ID needs its own secret. Only one live connection per agent ID is used.

Example:
  mule server --config /etc/mule/server.yaml
`)
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
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := rejectFlagsWithConfig(fs, configPath, "agent", "dial-timeout", "forward", "handshake-timeout", "idle-timeout", "keepalive", "listen-udp", "max-pending-dials", "max-streams", "reverse"); err != nil {
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
	warnMetricsExposure(log, common.MetricsListen)
	m := metrics.New()
	return runWithMetrics(common, m, func(ctx context.Context) error {
		return server.New(cfg, log, m).Run(ctx)
	})
}

func runAgent(args []string) error {
	fs := flag.NewFlagSet("agent", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	setCommandUsage(fs, `Usage:
  mule agent --config FILE [runtime options]                (recommended)
  mule agent --server HOST:PORT --agent-id ID --secret-file FILE \
    (--forward SERVICE=LISTEN | --reverse SERVICE=TARGET) [options]

Connect outbound to a Mule server. Forward listeners live on the agent;
reverse targets are dialed by the agent. The agent ID and secret must match
exactly one entry in the server configuration.

Example:
  mule agent --config /etc/mule/agent.yaml
`)
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
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if err := rejectFlagsWithConfig(fs, configPath, "agent-id", "connect-timeout", "forward", "handshake-timeout", "idle-timeout", "keepalive", "max-connections", "reconnect-delay", "reverse", "secret-file", "send-client-addr", "server"); err != nil {
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
	warnMetricsExposure(log, common.MetricsListen)
	m := metrics.New()
	return runWithMetrics(common, m, func(ctx context.Context) error {
		return agent.New(cfg, secret, log, m).Run(ctx)
	})
}

func runProbe(args []string) error {
	fs := flag.NewFlagSet("probe", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	setCommandUsage(fs, `Usage:
  mule probe --server HOST:PORT --agent-id ID --secret-file FILE \
    --direction forward|reverse --service SERVICE [options]

Authenticate as an agent and verify that a configured service is authorized.
A forward probe also verifies that the server can dial its configured target.

Example:
  mule probe --server server.example.org:4400 --agent-id dgx \
    --secret-file /etc/mule/dgx.key --direction forward --service ollama
`)
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
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
	ctx, cancel := context.WithTimeout(context.Background(), connectTimeout)
	defer cancel()
	dialProbe := func(serverName string) (*quic.Conn, error) {
		tlsCfg, err := auth.TLSConfigWithServerName(secret, auth.RoleForward, serverName)
		if err != nil {
			return nil, err
		}
		return quic.DialAddr(ctx, serverAddr, tlsCfg, &quic.Config{
			HandshakeIdleTimeout: handshakeTimeout,
			MaxIdleTimeout:       idleTimeout,
			KeepAlivePeriod:      keepalive,
		})
	}
	conn, err := dialProbe(auth.ProbeServerNameForClient(agentID))
	if err != nil {
		probeErr := err
		conn, err = dialProbe(auth.ExitServerNameForClient(agentID))
		if err != nil {
			return fmt.Errorf("probe connection failed with dedicated and compatibility identities: %v; %w", probeErr, err)
		}
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
	setCommandUsage(fs, `Usage:
  mule keygen --out FILE

Generate a new shared secret. The output file must not already exist.
Use a different secret for every agent ID, and copy that agent's secret to
the matching server configuration.
`)
	out := fs.String("out", "", "output secret file")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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
	setCommandUsage(fs, `Usage:
  mule check --secret-file FILE

Validate the encoding, length, file type, and permissions of a shared secret.
`)
	secretFile := fs.String("secret-file", "", "secret file to validate")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
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

func runCheckConfig(args []string) error {
	fs := flag.NewFlagSet("check-config", flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	setCommandUsage(fs, `Usage:
  mule check-config --role agent|server --config FILE

Parse a YAML configuration, reject unknown fields and invalid values, and
verify every referenced secret file without opening network listeners.

Examples:
  mule check-config --role agent --config /etc/mule/agent.yaml
  mule check-config --role server --config /etc/mule/server.yaml
`)
	role := fs.String("role", "", "configuration role: agent or server")
	configPath := fs.String("config", "", "YAML config file")
	skipSecrets := fs.Bool("skip-secrets", false, "validate structure without reading referenced secrets")
	if err := parseFlags(fs, args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if *role == "" || *configPath == "" {
		return errors.New("--role and --config are required")
	}
	switch *role {
	case "agent":
		cfg, err := agent.LoadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load agent config: %w", err)
		}
		if err := agent.ValidateConfig(cfg); err != nil {
			return fmt.Errorf("validate agent config: %w", err)
		}
		if !*skipSecrets {
			if _, err := auth.LoadSecretFile(cfg.SecretFile); err != nil {
				return fmt.Errorf("validate agent secret: %w", err)
			}
		}
	case "server":
		cfg, err := server.LoadConfig(*configPath)
		if err != nil {
			return fmt.Errorf("load server config: %w", err)
		}
		if err := server.ValidateConfig(cfg); err != nil {
			return fmt.Errorf("validate server config: %w", err)
		}
		if !*skipSecrets {
			clients := make([]auth.ClientIdentity, 0, len(cfg.Agents))
			for _, configuredAgent := range cfg.Agents {
				secret, err := auth.LoadSecretFile(configuredAgent.SecretFile)
				if err != nil {
					return fmt.Errorf("validate secret for agent %q: %w", configuredAgent.ID, err)
				}
				clients = append(clients, auth.ClientIdentity{ID: configuredAgent.ID, Secret: secret})
			}
			if _, err := auth.MultiClientTLSConfig(clients); err != nil {
				return fmt.Errorf("validate server identities: %w", err)
			}
		}
	default:
		return fmt.Errorf("--role must be agent or server")
	}
	fmt.Printf("%s config ok: %s\n", *role, *configPath)
	return nil
}

func rejectFlagsWithConfig(fs *flag.FlagSet, configPath string, names ...string) error {
	if configPath == "" {
		return nil
	}
	disallowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		disallowed[name] = struct{}{}
	}
	var mixed []string
	fs.Visit(func(f *flag.Flag) {
		if _, ok := disallowed[f.Name]; ok {
			mixed = append(mixed, "--"+f.Name)
		}
	})
	if len(mixed) != 0 {
		return fmt.Errorf("--config cannot be combined with config-owned flags: %s", strings.Join(mixed, ", "))
	}
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

func warnMetricsExposure(log *logging.Logger, addr string) {
	if addr == "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		log.Warn("metrics_listener_exposed", "listener_address", addr, "reason", "status_and_metrics_have_no_authentication")
	}
}
