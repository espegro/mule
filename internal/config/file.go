package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

type ExitFile struct {
	ListenUDP        string                    `yaml:"listen_udp"`
	SecretFile       string                    `yaml:"secret_file"`
	Target           string                    `yaml:"target"`
	Routes           map[string]string         `yaml:"routes"`
	Clients          map[string]ExitFileClient `yaml:"clients"`
	DialTimeout      string                    `yaml:"dial_timeout"`
	HandshakeTimeout string                    `yaml:"handshake_timeout"`
	IdleTimeout      string                    `yaml:"idle_timeout"`
	MaxStreams       int                       `yaml:"max_streams"`
	MaxPendingDials  int                       `yaml:"max_pending_dials"`
	KeepAlive        string                    `yaml:"keepalive"`
	LogFormat        string                    `yaml:"log_format"`
	LogLevel         string                    `yaml:"log_level"`
	MetricsListen    string                    `yaml:"metrics_listen"`
	ShutdownTimeout  string                    `yaml:"shutdown_timeout"`
}

type ExitFileClient struct {
	SecretFile string            `yaml:"secret_file"`
	Routes     map[string]string `yaml:"routes"`
}

func LoadExitFile(path string) (Exit, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Exit{}, fmt.Errorf("read config file: %w", err)
	}
	var fc ExitFile
	if err := yaml.Unmarshal(data, &fc); err != nil {
		return Exit{}, fmt.Errorf("parse config file: %w", err)
	}
	cfg := Exit{
		Common: Common{
			SecretFile:    fc.SecretFile,
			LogFormat:     fc.LogFormat,
			LogLevel:      fc.LogLevel,
			MetricsListen: fc.MetricsListen,
		},
		ListenUDP:       fc.ListenUDP,
		Target:          fc.Target,
		Routes:          fc.Routes,
		MaxStreams:      fc.MaxStreams,
		MaxPendingDials: fc.MaxPendingDials,
	}
	if fc.DialTimeout != "" {
		if cfg.DialTimeout, err = time.ParseDuration(fc.DialTimeout); err != nil {
			return Exit{}, fmt.Errorf("invalid dial_timeout: %w", err)
		}
	}
	if fc.HandshakeTimeout != "" {
		if cfg.HandshakeTimeout, err = time.ParseDuration(fc.HandshakeTimeout); err != nil {
			return Exit{}, fmt.Errorf("invalid handshake_timeout: %w", err)
		}
	}
	if fc.IdleTimeout != "" {
		if cfg.IdleTimeout, err = time.ParseDuration(fc.IdleTimeout); err != nil {
			return Exit{}, fmt.Errorf("invalid idle_timeout: %w", err)
		}
	}
	if fc.KeepAlive != "" {
		if cfg.KeepAlive, err = time.ParseDuration(fc.KeepAlive); err != nil {
			return Exit{}, fmt.Errorf("invalid keepalive: %w", err)
		}
	}
	if fc.ShutdownTimeout != "" {
		if cfg.ShutdownTimeout, err = time.ParseDuration(fc.ShutdownTimeout); err != nil {
			return Exit{}, fmt.Errorf("invalid shutdown_timeout: %w", err)
		}
	}
	if len(fc.Clients) > 0 {
		cfg.ClientRoutes = make(map[string]map[string]string, len(fc.Clients))
		for id, client := range fc.Clients {
			if err := ValidateClientID(id); err != nil {
				return Exit{}, err
			}
			cfg.Clients = append(cfg.Clients, Client{ID: id, SecretFile: client.SecretFile})
			cfg.ClientRoutes[id] = client.Routes
		}
	}
	return cfg, nil
}

func ApplyExitDefaults(cfg *Exit) {
	if cfg.LogFormat == "" {
		cfg.LogFormat = "text"
	}
	if cfg.LogLevel == "" {
		cfg.LogLevel = "info"
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 30 * time.Second
	}
	if cfg.DialTimeout == 0 {
		cfg.DialTimeout = 10 * time.Second
	}
	if cfg.HandshakeTimeout == 0 {
		cfg.HandshakeTimeout = 10 * time.Second
	}
	if cfg.IdleTimeout == 0 {
		cfg.IdleTimeout = 5 * time.Minute
	}
	if cfg.MaxStreams == 0 {
		cfg.MaxStreams = 100
	}
	if cfg.MaxPendingDials == 0 {
		cfg.MaxPendingDials = 20
	}
	if cfg.KeepAlive == 0 {
		cfg.KeepAlive = 20 * time.Second
	}
}
