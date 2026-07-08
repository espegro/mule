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
var clientIDPattern = regexp.MustCompile(`^[A-Za-z0-9-]{1,63}$`)

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
	Clients          []Client
	ClientRoutes     map[string]map[string]string
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

type Client struct {
	ID         string
	SecretFile string
}

func MultiClientMode(cfg Exit) bool {
	return len(cfg.Clients) > 0 || len(cfg.ClientRoutes) > 0
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
	if MultiClientMode(cfg) {
		return nil, fmt.Errorf("simple routes are not available in multi-client mode")
	}
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

func NormalizeClientRoutes(cfg Exit) (map[string]map[string]string, error) {
	if len(cfg.Clients) == 0 {
		return nil, fmt.Errorf("at least one client is required in multi-client mode")
	}
	if cfg.SecretFile != "" || cfg.Target != "" || len(cfg.Routes) > 0 {
		return nil, fmt.Errorf("do not mix --secret-file/--target/simple --route with multi-client mode")
	}
	seenClients := make(map[string]struct{}, len(cfg.Clients))
	for _, client := range cfg.Clients {
		if err := ValidateClientID(client.ID); err != nil {
			return nil, err
		}
		if client.SecretFile == "" {
			return nil, fmt.Errorf("client %q has empty secret file", client.ID)
		}
		if _, ok := seenClients[client.ID]; ok {
			return nil, fmt.Errorf("duplicate client %q", client.ID)
		}
		seenClients[client.ID] = struct{}{}
	}
	out := make(map[string]map[string]string, len(cfg.ClientRoutes))
	for clientID, routes := range cfg.ClientRoutes {
		if _, ok := seenClients[clientID]; !ok {
			return nil, fmt.Errorf("route configured for unknown client %q", clientID)
		}
		if len(routes) == 0 {
			return nil, fmt.Errorf("client %q has no routes", clientID)
		}
		out[clientID] = make(map[string]string, len(routes))
		for route, target := range routes {
			if err := ValidateRouteID(route); err != nil {
				return nil, err
			}
			if err := ValidateTCPAddress(target); err != nil {
				return nil, err
			}
			out[clientID][route] = target
		}
	}
	for _, client := range cfg.Clients {
		if len(out[client.ID]) == 0 {
			return nil, fmt.Errorf("client %q has no routes", client.ID)
		}
	}
	return out, nil
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

func ParseClient(v string) (Client, error) {
	id, path, err := splitMapping(v)
	if err != nil {
		return Client{}, err
	}
	if err := ValidateClientID(id); err != nil {
		return Client{}, err
	}
	return Client{ID: id, SecretFile: path}, nil
}

func ParseClientRouteTarget(v string) (string, string, string, error) {
	left, target, err := splitMapping(v)
	if err != nil {
		return "", "", "", err
	}
	clientID, route, ok := strings.Cut(left, ":")
	if !ok || clientID == "" || route == "" {
		return "", "", "", fmt.Errorf("multi-client route %q must be client:route=target", v)
	}
	if err := ValidateClientID(clientID); err != nil {
		return "", "", "", err
	}
	if err := ValidateRouteID(route); err != nil {
		return "", "", "", err
	}
	if err := ValidateTCPAddress(target); err != nil {
		return "", "", "", err
	}
	return clientID, route, target, nil
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

func ValidateClientID(id string) error {
	if !clientIDPattern.MatchString(id) {
		return fmt.Errorf("invalid client id %q: use 1-63 chars from A-Z, a-z, 0-9 or -", id)
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
