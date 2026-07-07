package metrics

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type Metrics struct {
	ActiveTCPConnections atomic.Int64
	ActiveQUICStreams    atomic.Int64
	QUICConnections      atomic.Int64
	TCPConnectionsTotal  atomic.Uint64
	StreamsTotal         atomic.Uint64
	StreamErrorsTotal    atomic.Uint64
	TargetDialErrors     atomic.Uint64
	AuthFailuresTotal    atomic.Uint64
	BytesClientToTarget  atomic.Uint64
	BytesTargetToClient  atomic.Uint64
}

func New() *Metrics { return &Metrics{} }

func (m *Metrics) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintf(w, "mule_active_tcp_connections %d\n", m.ActiveTCPConnections.Load())
		fmt.Fprintf(w, "mule_active_quic_streams %d\n", m.ActiveQUICStreams.Load())
		fmt.Fprintf(w, "mule_quic_connections %d\n", m.QUICConnections.Load())
		fmt.Fprintf(w, "mule_tcp_connections_total %d\n", m.TCPConnectionsTotal.Load())
		fmt.Fprintf(w, "mule_streams_total %d\n", m.StreamsTotal.Load())
		fmt.Fprintf(w, "mule_stream_errors_total %d\n", m.StreamErrorsTotal.Load())
		fmt.Fprintf(w, "mule_target_dial_errors_total %d\n", m.TargetDialErrors.Load())
		fmt.Fprintf(w, "mule_auth_failures_total %d\n", m.AuthFailuresTotal.Load())
		fmt.Fprintf(w, "mule_bytes_client_to_target_total %d\n", m.BytesClientToTarget.Load())
		fmt.Fprintf(w, "mule_bytes_target_to_client_total %d\n", m.BytesTargetToClient.Load())
	})
}

func Serve(ctx context.Context, addr string, m *Metrics) error {
	if addr == "" {
		return nil
	}
	srv := &http.Server{
		Addr:              addr,
		Handler:           m.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
		err := <-errCh
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return err
	}
}
