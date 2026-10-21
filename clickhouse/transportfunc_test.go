package clickhouse

import (
	"context"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

type recordingProxy struct {
	listener net.Listener
	hits     atomic.Int64
}

func newRecordingProxy(t *testing.T) *recordingProxy {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	proxy := &recordingProxy{listener: listener}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			proxy.hits.Add(1)
			_ = conn.Close()
		}
	}()
	return proxy
}

func pingHTTPDSN(t *testing.T, options Options) {
	t.Helper()
	client, err := NewClickHouseQueryClientWithOptions(options)
	if err != nil {
		t.Fatalf("NewClickHouseQueryClientWithOptions() error = %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = client.Ping(ctx)
}

func TestTransportFunc_PinsHTTPTransportAwayFromEnvProxy(t *testing.T) {
	proxy := newRecordingProxy(t)
	t.Setenv("HTTP_PROXY", "http://"+proxy.listener.Addr().String())
	t.Setenv("http_proxy", "http://"+proxy.listener.Addr().String())
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	const dsn = "http://user:secret@ch.example.invalid:8123/default"

	t.Run("nil option keeps the driver default (env proxy)", func(t *testing.T) {
		before := proxy.hits.Load()
		pingHTTPDSN(t, Options{DSN: dsn})
		if proxy.hits.Load() == before {
			t.Fatal("proxy saw no connection, want the driver default to use the env proxy")
		}
	})

	t.Run("TransportFunc pins the transport", func(t *testing.T) {
		before := proxy.hits.Load()
		called := false
		pingHTTPDSN(t, Options{DSN: dsn, TransportFunc: func(transport *http.Transport) (http.RoundTripper, error) {
			called = true
			transport.Proxy = nil
			return transport, nil
		}})
		if !called {
			t.Fatal("TransportFunc was not called")
		}
		if got := proxy.hits.Load() - before; got != 0 {
			t.Fatalf("proxy saw %d connections, want 0", got)
		}
	})
}
