package app

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/config"
	"github.com/cloud-ru-tech/guardrails-llm-filter/internal/health"
)

// serveGateway starts srv on a loopback port in place of the data-plane
// server and returns its base URL.
func serveGateway(t *testing.T, srv *http.Server) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

// stoppableApp is an App whose only running server is the given data plane;
// everything shutdown touches besides it is absent or never started.
func stoppableApp(gateway *http.Server, sh config.Shutdown) *App {
	return &App{
		cfg:           &config.Config{Shutdown: sh},
		gatewayServer: gateway,
		metricsServer: &http.Server{ReadHeaderTimeout: time.Second},
	}
}

func markReady(t *testing.T) {
	t.Helper()
	health.SetReadiness(true)
	t.Cleanup(func() { health.SetReadiness(false) })
}

// freshGet issues a GET on a new connection, so the result reflects the
// listener's state rather than a pooled connection's.
func freshGet(url string) (*http.Response, error) {
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}, Timeout: 5 * time.Second}
	return client.Get(url)
}

func TestShutdownDrainKeepsServingWithReadinessOff(t *testing.T) {
	markReady(t)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /readyz", readinessHandler)
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok")) })
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	base := serveGateway(t, srv)

	pooled := &http.Client{Timeout: 5 * time.Second}
	resp, err := pooled.Get(base + "/")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	require.False(t, resp.Close, "before the stop signal connections are kept alive")

	const drain = 500 * time.Millisecond
	app := stoppableApp(srv, config.Shutdown{DrainPeriod: drain, Timeout: 2 * time.Second})
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- app.shutdown(func() {}) }()

	require.Eventually(t, func() bool { return !health.GetReadiness() }, time.Second, 5*time.Millisecond)

	ready, err := freshGet(base + "/readyz")
	require.NoError(t, err, "the listener must stay open while draining")
	_ = ready.Body.Close()
	assert.Equal(t, http.StatusServiceUnavailable, ready.StatusCode, "readiness is off while draining")

	served, err := freshGet(base + "/")
	require.NoError(t, err, "requests that still arrive while draining must be served")
	_ = served.Body.Close()
	assert.Equal(t, http.StatusOK, served.StatusCode)
	assert.True(t, served.Close, "while draining every response closes its connection")

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("shutdown did not finish")
	}
	assert.GreaterOrEqual(t, time.Since(started), drain, "the listener closes only after the drain period")

	_, err = freshGet(base + "/")
	require.Error(t, err, "the listener is closed after shutdown")
}

func TestShutdownFinishesInFlightRequest(t *testing.T) {
	markReady(t)
	entered := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			close(entered)
			time.Sleep(300 * time.Millisecond)
			_, _ = w.Write([]byte("complete"))
		}),
		ReadHeaderTimeout: time.Second,
	}
	base := serveGateway(t, srv)

	type result struct {
		body string
		err  error
	}
	got := make(chan result, 1)
	go func() {
		resp, err := freshGet(base + "/")
		if err != nil {
			got <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		b, err := io.ReadAll(resp.Body)
		got <- result{body: string(b), err: err}
	}()
	<-entered

	app := stoppableApp(srv, config.Shutdown{Timeout: 3 * time.Second})
	require.NoError(t, app.shutdown(func() {}))

	r := <-got
	require.NoError(t, r.err)
	assert.Equal(t, "complete", r.body, "an in-flight request within the budget completes")
}

func TestShutdownTimeoutBoundsInFlightRequest(t *testing.T) {
	markReady(t)
	entered := make(chan struct{})
	release := make(chan struct{})
	srv := &http.Server{
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
			close(entered)
			<-release
		}),
		ReadHeaderTimeout: time.Second,
	}
	base := serveGateway(t, srv)
	t.Cleanup(func() { close(release) })

	go func() {
		if resp, err := freshGet(base + "/"); err == nil {
			_ = resp.Body.Close()
		}
	}()
	<-entered

	const budget = 100 * time.Millisecond
	app := stoppableApp(srv, config.Shutdown{Timeout: budget})
	started := time.Now()
	err := app.shutdown(func() {})

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded), "shutdown gives up once the configured budget is spent: %v", err)
	assert.Less(t, time.Since(started), 2*time.Second, "the configured budget, not a built-in one, bounds shutdown")
}
