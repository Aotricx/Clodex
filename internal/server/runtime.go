package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

const shutdownTimeout = 10 * time.Second

// Serve binds only IPv4 loopback, reports the resolved listener, and blocks
// until failure or graceful context cancellation. Port zero is test-only but
// remains loopback-safe; CLI validation rejects it for shipping configuration.
func Serve(ctx context.Context, port int, handler http.Handler, ready func(net.Addr)) error {
	if ctx == nil {
		return errors.New("serve Clodex: nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if port < 0 || port > 65535 {
		return fmt.Errorf("serve Clodex: port must be between 0 and 65535, got %d", port)
	}
	if handler == nil {
		return errors.New("serve Clodex: nil HTTP handler")
	}

	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("serve Clodex: bind loopback: %w", err)
	}
	defer listener.Close()
	if ready != nil {
		ready(listener.Addr())
	}

	httpServer := newHTTPServer(ctx, handler)
	stopShutdown := make(chan struct{})
	shutdownResult := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			shutdownContext, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
			defer cancel()
			shutdownResult <- httpServer.Shutdown(shutdownContext)
		case <-stopShutdown:
			shutdownResult <- nil
		}
	}()

	serveErr := httpServer.Serve(listener)
	close(stopShutdown)
	shutdownErr := <-shutdownResult
	if shutdownErr != nil {
		return fmt.Errorf("serve Clodex: graceful shutdown: %w", shutdownErr)
	}
	if errors.Is(serveErr, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	if serveErr != nil {
		return fmt.Errorf("serve Clodex: %w", serveErr)
	}
	return nil
}

func newHTTPServer(ctx context.Context, handler http.Handler) *http.Server {
	return &http.Server{
		Handler:           handler,
		ReadTimeout:       0, // unlimited: long SSE responses must not be killed
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    1 << 20,
		BaseContext: func(net.Listener) context.Context {
			return ctx
		},
	}
}
