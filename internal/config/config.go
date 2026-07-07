package config

import (
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const DefaultRoute = "default"

var routeIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)
var forwardIDPattern = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,128}$`)

type Common struct {
	SecretFile      string
	LogFormat       string
	LogLevel        string
	MetricsListen   string
	ShutdownTimeout time.Duration
}

type Forward struct {
	Common
	ListenTCP        string
	Listens          []RouteListen
	Peer             string
	ForwardID        string
	SendClientAddr   bool
	ConnectTimeout   time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxConnections   int
	KeepAlive        time.Duration
}

type Exit struct {
	Common
	ListenUDP        string
	Target           string
	Routes           map[string]string
	DialTimeout      time.Duration
	HandshakeTimeout time.Duration
	IdleTimeout      time.Duration
	MaxStreams       int
	MaxPendingDials  int
	KeepAlive        time.Duration
}

type RouteListen struct {
	Route   string
	Address string
}

func NormalizeForwardListens(cfg Forward) ([]RouteListen, error) {
	var out []RouteListen
	if cfg.ListenTCP != "" {
		out = append(out, RouteListen{Route: DefaultRoute, Address: cfg.ListenTCP})
	}
	out = append(out, cfg.Listens...)
	if len(out) == 0 {
		return nil, fmt.Errorf("at least one listen address is required")
	}
	seen := make(map[string]struct{}, len(out))
	for _, listen := range out {
		if err := ValidateRouteID(listen.Route); err != nil {
			return nil, err
		}
		if err := ValidateListenAddress(listen.Address); err != nil {
			return nil, err
		}
		if _, ok := seen[listen.Route]; ok {
			return nil, fmt.Errorf("duplicate listen route %q", listen.Route)
		}
		seen[listen.Route] = struct{}{}
	}
	return out, nil
}

func NormalizeExitRoutes(cfg Exit) (map[string]string, error) {
	routes := make(map[string]string, len(cfg.Routes)+1)
	if cfg.Target != "" {
		routes[DefaultRoute] = cfg.Target
	}
	for route, target := range cfg.Routes {
		if _, exists := routes[route]; exists {
			return nil, fmt.Errorf("duplicate route %q", route)
		}
		routes[route] = target
	}
	if len(routes) == 0 {
		return nil, fmt.Errorf("at least one target route is required")
	}
	for route, target := range routes {
		if err := ValidateRouteID(route); err != nil {
			return nil, err
		}
		if err := ValidateTCPAddress(target); err != nil {
			return nil, err
		}
	}
	return routes, nil
}

func ParseRouteListen(v string) (RouteListen, error) {
	route, address, err := splitMapping(v)
	if err != nil {
		return RouteListen{}, err
	}
	if err := ValidateRouteID(route); err != nil {
		return RouteListen{}, err
	}
	if err := ValidateListenAddress(address); err != nil {
		return RouteListen{}, err
	}
	return RouteListen{Route: route, Address: address}, nil
}

func ParseRouteTarget(v string) (string, string, error) {
	route, target, err := splitMapping(v)
	if err != nil {
		return "", "", err
	}
	if err := ValidateRouteID(route); err != nil {
		return "", "", err
	}
	if err := ValidateTCPAddress(target); err != nil {
		return "", "", err
	}
	return route, target, nil
}

func ValidateRouteID(route string) error {
	if !routeIDPattern.MatchString(route) {
		return fmt.Errorf("invalid route id %q: use 1-64 chars from A-Z, a-z, 0-9, _, . or -", route)
	}
	return nil
}

func ValidateForwardID(id string) error {
	if id == "" {
		return nil
	}
	if !forwardIDPattern.MatchString(id) {
		return fmt.Errorf("invalid forward id %q: use 1-128 chars from A-Z, a-z, 0-9, _, ., : or -", id)
	}
	return nil
}

func splitMapping(v string) (string, string, error) {
	route, address, ok := strings.Cut(v, "=")
	if !ok || route == "" || address == "" {
		return "", "", fmt.Errorf("mapping %q must be route=address", v)
	}
	return route, address, nil
}

func ValidateTCPAddress(addr string) error {
	return validateHostPort(addr, false)
}

func ValidateListenAddress(addr string) error {
	return validateHostPort(addr, true)
}

func ValidateUDPAddress(addr string) error {
	return validateHostPort(addr, true)
}

func validateHostPort(addr string, allowEmptyHost bool) error {
	host, portText, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q: %w", addr, err)
	}
	if host == "" && !allowEmptyHost {
		return fmt.Errorf("invalid address %q: host is required", addr)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("invalid address %q: port must be 1-65535", addr)
	}
	return nil
}

func ValidateLogFormat(v string) error {
	switch v {
	case "text", "json":
		return nil
	default:
		return fmt.Errorf("invalid log format %q", v)
	}
}

func ValidateLogLevel(v string) error {
	switch v {
	case "debug", "info", "warn", "error":
		return nil
	default:
		return fmt.Errorf("invalid log level %q", v)
	}
}
