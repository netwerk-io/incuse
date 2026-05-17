package observability

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Server is the operator-facing HTTP surface: /healthz, /readyz,
// /metrics. Owned by cmd/incuse, started inside the errgroup
// alongside scaleset.Run + orchestrator.Run.
type Server struct {
	configuredAddr string
	rec            *Recorder
	healthy        atomic.Bool
	ready          atomic.Bool
	srv            *http.Server

	addrMu sync.RWMutex
	addr   string
}

// NewServer returns a server bound to addr (Go listen syntax). addr
// is exposed via Addr() once Start has run, useful in tests that
// pick :0.
func NewServer(addr string, rec *Recorder) *Server {
	return &Server{configuredAddr: addr, rec: rec}
}

// MarkHealthy is called once scale-set bootstrap succeeds. /healthz
// returns 200 from this point.
func (s *Server) MarkHealthy() { s.healthy.Store(true) }

// MarkReady is called once the first listener poll cycle has
// returned. /readyz returns 200 from this point.
func (s *Server) MarkReady() { s.ready.Store(true) }

// IsHealthy / IsReady let callers inspect the gauges programmatically
// (tests, the listener wrapper).
func (s *Server) IsHealthy() bool { return s.healthy.Load() }
func (s *Server) IsReady() bool   { return s.ready.Load() }

// Run starts the HTTP server and blocks until ctx is cancelled. It
// then performs a graceful shutdown bounded at 5s before returning.
// Returns nil on clean ctx-cancel; surfaces ListenAndServe errors
// otherwise.
func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		if s.healthy.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("starting"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, _ *http.Request) {
		if s.ready.Load() {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready"))
	})
	mux.Handle("/metrics", promhttp.HandlerFor(s.rec.Registry(), promhttp.HandlerOpts{
		// Errors during scrape go to the default Go log, but the
		// listener should keep serving.
		ErrorHandling: promhttp.ContinueOnError,
	}))

	s.srv = &http.Server{
		Addr:              s.configuredAddr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	ln, err := net.Listen("tcp", s.configuredAddr)
	if err != nil {
		return err
	}
	s.setAddr(ln.Addr().String())

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			errCh <- nil
			return
		}
		errCh <- err
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutCtx)
		<-errCh
		return nil
	case err := <-errCh:
		return err
	}
}

// Addr returns the bound address. Empty until Run has called
// net.Listen.
func (s *Server) Addr() string {
	s.addrMu.RLock()
	defer s.addrMu.RUnlock()
	return s.addr
}

func (s *Server) setAddr(a string) {
	s.addrMu.Lock()
	defer s.addrMu.Unlock()
	s.addr = a
}
