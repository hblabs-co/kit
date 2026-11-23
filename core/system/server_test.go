package system

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hblabs-co/kit/core/constants"
)

// TestServer_DefaultAddr — an empty Addr binds ListenAddr() (HB_PORT
// here), so apps no longer build ":<port>" themselves.
func TestServer_DefaultAddr(t *testing.T) {
	addr := pickFreeAddr(t)
	t.Setenv(constants.EnvPort, addr[strings.LastIndex(addr, ":")+1:])
	s := NewServer(ServerOptions{Handler: http.NotFoundHandler()})
	if err := s.Register(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Shutdown(context.Background()) }()
	if s.opts.Addr != ListenAddr() || !strings.HasSuffix(addr, s.opts.Addr) {
		t.Fatalf("Addr = %q, want %q", s.opts.Addr, ListenAddr())
	}
}

// TestServer_RegisterRequiresHandler — same rationale: a nil handler
// would either panic on first request or 404 everything depending on
// stdlib behaviour, both worse than a boot-time error.
func TestServer_RegisterRequiresHandler(t *testing.T) {
	s := NewServer(ServerOptions{Addr: ":0"})
	err := s.Register(context.Background())
	if err == nil || !strings.Contains(err.Error(), "Handler") {
		t.Errorf("Register err = %v, want it to mention \"Handler\"", err)
	}
}

// TestServer_LifecycleHappyPath — Register binds and serves; a real
// HTTP request reaches the handler; Shutdown drains cleanly without
// returning an error. We pre-pick a free port via net.Listen+Close so
// the assertion can hit a known URL — the http.Server doesn't expose
// its listener once started.
func TestServer_LifecycleHappyPath(t *testing.T) {
	addr := pickFreeAddr(t)

	hit := make(chan struct{}, 1)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case hit <- struct{}{}:
		default:
		}
		w.WriteHeader(http.StatusNoContent)
	})

	s := NewServer(ServerOptions{Addr: addr, Handler: handler})
	if err := s.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}

	// ListenAndServe runs in a goroutine; poll briefly for liveness
	// so we don't race on whether the socket is up yet.
	if !waitForListen(addr, 500*time.Millisecond) {
		_ = s.Shutdown(context.Background())
		t.Fatalf("server never accepted on %s", addr)
	}

	resp, err := http.Get("http://" + addr + "/")
	if err != nil {
		_ = s.Shutdown(context.Background())
		t.Fatalf("GET: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusNoContent)
	}
	select {
	case <-hit:
	default:
		t.Error("handler was not invoked")
	}

	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// TestServer_ShutdownWithoutRegister — Shutdown is documented to
// return nil when Register was never called. Without this guard the
// module path inside RunForever would NPE on a Server that errored
// out before Register completed.
func TestServer_ShutdownWithoutRegister(t *testing.T) {
	s := NewServer(ServerOptions{Addr: ":0", Handler: http.NotFoundHandler()})
	if err := s.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown without Register = %v, want nil", err)
	}
}

// TestServer_ShutdownPropagatesError — when http.Server.Shutdown
// errors (here forced by holding a request in-flight while the
// shutdown ctx is already cancelled), the wrapped error must surface
// to RunForever so the operator sees what actually went wrong
// instead of a silent "drain succeeded".
//
// The held-request setup is necessary because http.Server.Shutdown
// only consults ctx in its idle-conn polling loop — with no active
// connections it returns nil immediately, never seeing the cancel.
func TestServer_ShutdownPropagatesError(t *testing.T) {
	addr := pickFreeAddr(t)
	block := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
	})
	s := NewServer(ServerOptions{Addr: addr, Handler: handler})
	if err := s.Register(context.Background()); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !waitForListen(addr, 500*time.Millisecond) {
		t.Fatalf("server never listened on %s", addr)
	}

	// Fire a request and wait for the handler to actually be inside
	// `<-block` — only then is the conn tracked as non-idle.
	reqDone := make(chan struct{})
	go func() {
		defer close(reqDone)
		c := &http.Client{Timeout: 5 * time.Second}
		resp, err := c.Get("http://" + addr + "/")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	time.Sleep(80 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := s.Shutdown(ctx)

	// Release the held request so the goroutine can exit before
	// the test returns.
	close(block)
	<-reqDone

	if err == nil {
		t.Fatal("expected non-nil error from cancelled-ctx Shutdown with in-flight request")
	}
	if !strings.Contains(err.Error(), "shutdown:") {
		t.Errorf("err = %v, want it wrapped with \"shutdown:\"", err)
	}

	// Drain the listener goroutine so its channel slot is freed.
	_ = s.server.Close()
	select {
	case <-s.listenErr:
	case <-time.After(500 * time.Millisecond):
	}
}

// TestBuildHTTPServer_AppliesDefaults — zero-valued fields must be
// backfilled from DefaultServerOptions. The hardened defaults are the
// whole point of this package; a regression here means services
// silently lose their slowloris caps.
func TestBuildHTTPServer_AppliesDefaults(t *testing.T) {
	srv := buildHTTPServer(ServerOptions{
		Addr:    ":80",
		Handler: http.NotFoundHandler(),
	})
	if srv.ReadHeaderTimeout != DefaultServerOptions.ReadHeaderTimeout {
		t.Errorf("ReadHeaderTimeout = %v, want %v",
			srv.ReadHeaderTimeout, DefaultServerOptions.ReadHeaderTimeout)
	}
	if srv.ReadTimeout != DefaultServerOptions.ReadTimeout {
		t.Errorf("ReadTimeout = %v, want %v",
			srv.ReadTimeout, DefaultServerOptions.ReadTimeout)
	}
	if srv.WriteTimeout != DefaultServerOptions.WriteTimeout {
		t.Errorf("WriteTimeout = %v, want %v",
			srv.WriteTimeout, DefaultServerOptions.WriteTimeout)
	}
	if srv.IdleTimeout != DefaultServerOptions.IdleTimeout {
		t.Errorf("IdleTimeout = %v, want %v",
			srv.IdleTimeout, DefaultServerOptions.IdleTimeout)
	}
	if srv.MaxHeaderBytes != DefaultServerOptions.MaxHeaderBytes {
		t.Errorf("MaxHeaderBytes = %d, want %d",
			srv.MaxHeaderBytes, DefaultServerOptions.MaxHeaderBytes)
	}
}

// TestBuildHTTPServer_KeepsExplicitValues — non-zero fields must
// override the defaults. Without this, a service that explicitly
// configures a tighter ReadTimeout would be silently relaxed.
func TestBuildHTTPServer_KeepsExplicitValues(t *testing.T) {
	srv := buildHTTPServer(ServerOptions{
		Addr:           ":80",
		Handler:        http.NotFoundHandler(),
		ReadTimeout:    5 * time.Second,
		MaxHeaderBytes: 1024,
	})
	if srv.ReadTimeout != 5*time.Second {
		t.Errorf("ReadTimeout = %v, want 5s", srv.ReadTimeout)
	}
	if srv.MaxHeaderBytes != 1024 {
		t.Errorf("MaxHeaderBytes = %d, want 1024", srv.MaxHeaderBytes)
	}
	// Other fields untouched should still pick up defaults.
	if srv.WriteTimeout != DefaultServerOptions.WriteTimeout {
		t.Errorf("WriteTimeout = %v, want default %v",
			srv.WriteTimeout, DefaultServerOptions.WriteTimeout)
	}
}

// TestPickDur — zero falls through to the default; explicit values
// pass straight. Trivial helper but the boundary is load-bearing for
// every server option.
func TestPickDur(t *testing.T) {
	if got := pickDur(0, 5*time.Second); got != 5*time.Second {
		t.Errorf("pickDur(0, 5s) = %v, want 5s", got)
	}
	if got := pickDur(2*time.Second, 5*time.Second); got != 2*time.Second {
		t.Errorf("pickDur(2s, 5s) = %v, want 2s (explicit wins)", got)
	}
}

// TestPickInt — same shape as pickDur, but for MaxHeaderBytes.
func TestPickInt(t *testing.T) {
	if got := pickInt(0, 1024); got != 1024 {
		t.Errorf("pickInt(0, 1024) = %d, want 1024", got)
	}
	if got := pickInt(42, 1024); got != 42 {
		t.Errorf("pickInt(42, 1024) = %d, want 42 (explicit wins)", got)
	}
}

// pickFreeAddr binds a TCP listener to ":0" to grab a kernel-assigned
// port, then closes it and returns the addr string. There's an
// inherent TOCTOU window between Close and the test's Server binding
// it, but in single-host CI runs the kernel won't reuse the port
// instantly — good enough for a localhost integration test.
func pickFreeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pickFreeAddr: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
}

// waitForListen polls Dial until the addr accepts or the deadline
// fires. Used to bridge the "ListenAndServe runs in a goroutine"
// race so the test request doesn't go out before the socket is up.
func waitForListen(addr string, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			conn.Close()
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}
