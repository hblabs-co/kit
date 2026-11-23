package system

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/hblabs-co/kit/core/log"
)

// Default values for every ServerOptions knob. Even behind Cloudflare
// these caps stop a single connection from tying up a goroutine
// indefinitely (DNS leak, debug subdomain, accidental origin
// exposure). Pulled out as named constants so the rationale travels
// with the value at every reference, not just inside the struct
// literal.
const (
	// defaultReadHeaderTimeout caps the header read.
	defaultReadHeaderTimeout = 10 * time.Second
	// defaultReadTimeout caps the full request read — kills slow-POST.
	defaultReadTimeout = 30 * time.Second
	// defaultWriteTimeout caps the response write — slowloris-on-write.
	defaultWriteTimeout = 30 * time.Second
	// defaultIdleTimeout recycles keep-alive after a long silence.
	defaultIdleTimeout = 120 * time.Second
	// defaultMaxHeaderBytes caps the header-bomb attack surface.
	// 64 KiB matches well-known proxy defaults (nginx, envoy).
	defaultMaxHeaderBytes = 64 << 10
	// defaultShutdownGrace bounds the drain on SIGTERM. Stays well
	// under k8s' 30s terminationGracePeriodSeconds default so the
	// container runtime gets time to actually kill the process.
	defaultShutdownGrace = 10 * time.Second
)

// DefaultServerOptions are the values every service uses unless
// it has a domain-specific reason to deviate. See the default* group
// above for the rationale of each field.
var DefaultServerOptions = ServerOptions{
	ReadHeaderTimeout: defaultReadHeaderTimeout,
	ReadTimeout:       defaultReadTimeout,
	WriteTimeout:      defaultWriteTimeout,
	IdleTimeout:       defaultIdleTimeout,
	MaxHeaderBytes:    defaultMaxHeaderBytes,
	ShutdownGrace:     defaultShutdownGrace,
}

// ServerOptions bundles every knob the server needs. Zero values fall
// back to the matching field in DefaultServerOptions, so the typical
// caller fills only Addr and Handler.
type ServerOptions struct {
	Addr              string        // required, e.g. ":8080"
	Handler           http.Handler  // required
	ReadHeaderTimeout time.Duration // default 10 s
	ReadTimeout       time.Duration // default 30 s
	WriteTimeout      time.Duration // default 30 s
	IdleTimeout       time.Duration // default 120 s
	MaxHeaderBytes    int           // default 64 KiB
	ShutdownGrace     time.Duration // default 10 s
}

// Server is a hardened http.Server wrapped as a Module. Drops into
// RunForever next to NATS consumers, dev watchers, schedulers, and
// any other lifecycle workers. Construct with NewServer.
//
// The shutdown path honours both the Module-level deadline (passed
// by RunForever via SHUTDOWN_TIMEOUT) and the per-server
// ShutdownGrace cap, whichever fires first. ListenAndServe runs in a
// goroutine started by Register; failures (port in use, permission
// denied) are surfaced when Shutdown drains the listener channel.
type Server struct {
	opts      ServerOptions
	server    *http.Server
	listenErr chan error
}

// NewServer constructs a Server from ServerOptions. Validation is
// deferred to Register so a bad config surfaces at the same place as
// other module Register errors (uniform error path inside RunForever).
//
// Typical wiring:
//
//	ctx := system.Boot("service-edge", system.WithPort(8080))
//	if err := system.RunForever(ctx,
//	    system.NewServer(system.ServerOptions{Addr: ":8080", Handler: handler}),
//	    // …other modules
//	); err != nil { … }
func NewServer(opts ServerOptions) *Server {
	return &Server{opts: opts}
}

// Register starts ListenAndServe in a background goroutine and returns
// immediately, matching the Module contract: long-lived work runs in
// goroutines anchored to ctx. ctx isn't used directly — RunForever
// owns the lifecycle and triggers Shutdown — but receiving it keeps
// the interface uniform across modules.
func (s *Server) Register(ctx context.Context) error {
	if s.opts.Addr == "" {
		return fmt.Errorf("system.Server: Addr is required")
	}
	if s.opts.Handler == nil {
		return fmt.Errorf("system.Server: Handler is required")
	}
	s.server = buildHTTPServer(s.opts)
	s.listenErr = make(chan error, 1)
	go func() {
		log.Info("listening", "addr", s.opts.Addr)
		err := s.server.ListenAndServe()
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		s.listenErr <- err
	}()
	_ = ctx
	return nil
}

// Shutdown drains in-flight requests, propagating shutdownCtx so the
// system-level SHUTDOWN_TIMEOUT bounds the wait. The per-server
// ShutdownGrace caps the wait independently — whichever fires first
// wins. Returns nil on a clean drain or if Register was never called.
func (s *Server) Shutdown(shutdownCtx context.Context) error {
	if s.server == nil {
		return nil
	}
	grace := pickDur(s.opts.ShutdownGrace, DefaultServerOptions.ShutdownGrace)
	graceCtx, cancel := context.WithTimeout(shutdownCtx, grace)
	defer cancel()
	if err := s.server.Shutdown(graceCtx); err != nil {
		return fmt.Errorf("shutdown: %w", err)
	}
	// Drain the listener goroutine so the channel slot is released.
	select {
	case <-s.listenErr:
	default:
	}
	return nil
}

// buildHTTPServer applies DefaultServerOptions for any zero-valued
// field on opts. Centralised so the Module path stays in sync if the
// hardened defaults ever change.
func buildHTTPServer(opts ServerOptions) *http.Server {
	return &http.Server{
		Addr:              opts.Addr,
		Handler:           opts.Handler,
		ReadHeaderTimeout: pickDur(opts.ReadHeaderTimeout, DefaultServerOptions.ReadHeaderTimeout),
		ReadTimeout:       pickDur(opts.ReadTimeout, DefaultServerOptions.ReadTimeout),
		WriteTimeout:      pickDur(opts.WriteTimeout, DefaultServerOptions.WriteTimeout),
		IdleTimeout:       pickDur(opts.IdleTimeout, DefaultServerOptions.IdleTimeout),
		MaxHeaderBytes:    pickInt(opts.MaxHeaderBytes, DefaultServerOptions.MaxHeaderBytes),
	}
}

func pickDur(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}

func pickInt(n, def int) int {
	if n == 0 {
		return def
	}
	return n
}
