package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/espegro/mule/internal/logging"
)

func TestWarnMetricsExposure(t *testing.T) {
	tests := []struct {
		addr     string
		wantWarn bool
	}{
		{"127.0.0.1:9100", false},
		{"[::1]:9100", false},
		{"localhost:9100", false},
		{":9100", true},
		{"0.0.0.0:9100", true},
	}
	for _, tt := range tests {
		t.Run(tt.addr, func(t *testing.T) {
			var out bytes.Buffer
			log := logging.NewForWriter("text", "debug", &out)
			warnMetricsExposure(log, tt.addr)
			gotWarn := strings.Contains(out.String(), "metrics_listener_exposed")
			if gotWarn != tt.wantWarn {
				t.Fatalf("warning = %v, want %v; output=%q", gotWarn, tt.wantWarn, out.String())
			}
		})
	}
}
