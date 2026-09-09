package server

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServeBindsIPv4LoopbackAndShutsDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, 0, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"service": "clodex"})
		}), func(address net.Addr) { ready <- address })
	}()
	address := <-ready
	tcp, ok := address.(*net.TCPAddr)
	if !ok || tcp.IP.String() != "127.0.0.1" || tcp.Port == 0 {
		t.Fatalf("listener address = %#v", address)
	}
	response, err := http.Get("http://" + address.String())
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve() error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not shut down")
	}
	connection, err := net.DialTimeout("tcp4", address.String(), 50*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		t.Fatal("listener still accepts after shutdown")
	}
}

func TestNewHTTPServerLeavesReadTimeoutUnlimitedForSSE(t *testing.T) {
	server := newHTTPServer(context.Background(), http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if server.ReadTimeout != 0 {
		t.Fatalf("ReadTimeout = %s, want 0 so long SSE responses are not killed", server.ReadTimeout)
	}
	if server.ReadHeaderTimeout != 10*time.Second {
		t.Fatalf("ReadHeaderTimeout = %s, want 10s", server.ReadHeaderTimeout)
	}
	if server.IdleTimeout != 2*time.Minute {
		t.Fatalf("IdleTimeout = %s", server.IdleTimeout)
	}
	if server.MaxHeaderBytes != 1<<20 {
		t.Fatalf("MaxHeaderBytes = %d", server.MaxHeaderBytes)
	}
}

func TestServeValidatesPortHandlerAndContext(t *testing.T) {
	valid := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})
	for _, test := range []struct {
		name    string
		ctx     context.Context
		port    int
		handler http.Handler
	}{
		{name: "nil context", port: 1, handler: valid},
		{name: "negative port", ctx: context.Background(), port: -1, handler: valid},
		{name: "large port", ctx: context.Background(), port: 65536, handler: valid},
		{name: "nil handler", ctx: context.Background(), port: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := Serve(test.ctx, test.port, test.handler, nil); err == nil {
				t.Fatal("Serve() error = nil")
			}
		})
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Serve(ctx, 0, valid, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Serve(canceled) error = %v", err)
	}
}
