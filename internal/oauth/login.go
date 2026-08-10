package oauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"runtime"
	"time"

	"github.com/Aotricx/Clodex/internal/redact"
)

const (
	DefaultCallbackPort  uint16 = 1455
	FallbackCallbackPort uint16 = 1457
)

type Opener func(string) error

type OAuthCallbackError struct {
	Code        string
	Description string
}

func (e *OAuthCallbackError) Error() string {
	code := redact.Text(e.Code)
	description := redact.Text(e.Description)
	if description == "" {
		return fmt.Sprintf("OAuth callback error: %s", code)
	}
	return fmt.Sprintf("OAuth callback error: %s: %s", code, description)
}

type loginCallback struct {
	code  string
	err   error
	reply chan loginReply
	done  chan struct{}
}

type loginReply struct {
	success bool
}

type commandRunner func(context.Context, string, ...string) error

func (c *Client) Login(ctx context.Context, opener Opener) (TokenSet, error) {
	pkce, err := GeneratePKCE(c.Random)
	if err != nil {
		return TokenSet{}, err
	}
	state, err := generateState(c.Random)
	if err != nil {
		return TokenSet{}, err
	}

	listener, actualPort, err := listenForCallback(ctx, callbackPortCandidates(c.CallbackPorts))
	if err != nil {
		return TokenSet{}, err
	}
	loginCtx, cancel := context.WithCancel(ctx)
	callbacks := make(chan loginCallback, 1)
	server := &http.Server{
		Handler:           callbackHandler(loginCtx, state, callbacks),
		ReadHeaderTimeout: 5 * time.Second,
		BaseContext: func(net.Listener) context.Context {
			return loginCtx
		},
	}
	serverDone := make(chan error, 1)
	go func() {
		serveErr := server.Serve(listener)
		if errors.Is(serveErr, http.ErrServerClosed) {
			serveErr = nil
		}
		serverDone <- serveErr
	}()
	serverStopped := false
	defer func() {
		cancel()
		_ = listener.Close()
		_ = server.Close()
		if !serverStopped {
			<-serverDone
		}
	}()

	redirectURI := fmt.Sprintf("http://localhost:%d/auth/callback", actualPort)
	authorizeURL, err := c.AuthorizeURL(redirectURI, pkce, state)
	if err != nil {
		return TokenSet{}, err
	}
	if opener == nil {
		opener = func(rawURL string) error {
			return openBrowser(loginCtx, runtime.GOOS, startBrowserCommand, rawURL)
		}
	}
	if err := opener(authorizeURL); err != nil {
		return TokenSet{}, fmt.Errorf("open browser: %w", err)
	}

	select {
	case <-ctx.Done():
		return TokenSet{}, ctx.Err()
	case serveErr := <-serverDone:
		serverStopped = true
		if serveErr == nil {
			return TokenSet{}, errors.New("OAuth callback server stopped before login completed")
		}
		return TokenSet{}, fmt.Errorf("serve OAuth callback: %w", serveErr)
	case callback := <-callbacks:
		if callback.err != nil {
			return TokenSet{}, callback.err
		}
		tokens, exchangeErr := c.ExchangeCode(loginCtx, callback.code, redirectURI, pkce)
		reply := loginReply{success: exchangeErr == nil}
		select {
		case callback.reply <- reply:
		case <-callback.done:
		case <-ctx.Done():
		}
		select {
		case <-callback.done:
		case <-ctx.Done():
		}
		if exchangeErr != nil {
			return TokenSet{}, fmt.Errorf("exchange OAuth authorization code: %w", exchangeErr)
		}
		return tokens, nil
	}
}

func callbackPortCandidates(configured []uint16) []uint16 {
	if len(configured) == 0 {
		return []uint16{DefaultCallbackPort, FallbackCallbackPort}
	}
	return append([]uint16(nil), configured...)
}

func listenForCallback(ctx context.Context, ports []uint16) (net.Listener, uint16, error) {
	var failures []error
	for _, port := range ports {
		address := fmt.Sprintf("127.0.0.1:%d", port)
		listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp4", address)
		if err != nil {
			failures = append(failures, fmt.Errorf("bind OAuth callback %s: %w", address, err))
			continue
		}
		actualPort := uint16(listener.Addr().(*net.TCPAddr).Port)
		return listener, actualPort, nil
	}
	if len(failures) == 0 {
		return nil, 0, errors.New("no OAuth callback ports configured")
	}
	return nil, 0, errors.Join(failures...)
}

func callbackHandler(ctx context.Context, expectedState string, callbacks chan<- loginCallback) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/auth/callback" {
			http.Error(w, "Not Found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		query := r.URL.Query()
		if query.Get("state") != expectedState {
			http.Error(w, "State mismatch", http.StatusBadRequest)
			return
		}
		if errorCode := query.Get("error"); errorCode != "" {
			callbackErr := &OAuthCallbackError{Code: errorCode, Description: query.Get("error_description")}
			writeLoginPage(w, http.StatusBadRequest, "Login failed", "Authorization was not completed.")
			select {
			case callbacks <- loginCallback{err: callbackErr}:
			case <-ctx.Done():
			}
			return
		}
		code := query.Get("code")
		if code == "" {
			err := errors.New("missing authorization code")
			writeLoginPage(w, http.StatusBadRequest, "Missing authorization code", "Sign-in could not be completed.")
			select {
			case callbacks <- loginCallback{err: err}:
			case <-ctx.Done():
			}
			return
		}

		callback := loginCallback{
			code:  code,
			reply: make(chan loginReply, 1),
			done:  make(chan struct{}),
		}
		select {
		case callbacks <- callback:
		case <-ctx.Done():
			writeLoginPage(w, http.StatusRequestTimeout, "Login cancelled", "The login request is no longer active.")
			return
		}
		defer close(callback.done)
		select {
		case reply := <-callback.reply:
			if reply.success {
				writeLoginPage(w, http.StatusOK, "Login successful", "You may close this window.")
				return
			}
			writeLoginPage(w, http.StatusBadGateway, "Login failed", "The authorization code could not be exchanged.")
		case <-ctx.Done():
			writeLoginPage(w, http.StatusRequestTimeout, "Login cancelled", "The login request is no longer active.")
		}
	})
}

func writeLoginPage(w http.ResponseWriter, status int, title, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "close")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(
		w,
		"<!doctype html><html><head><meta charset=\"utf-8\"><title>%s</title></head><body><h1>%s</h1><p>%s</p></body></html>",
		html.EscapeString(title), html.EscapeString(title), html.EscapeString(message),
	)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func generateState(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	bytes := make([]byte, 32)
	if _, err := io.ReadFull(random, bytes); err != nil {
		return "", fmt.Errorf("generate OAuth state: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}

func OpenBrowser(rawURL string) error {
	return openBrowser(context.Background(), runtime.GOOS, startBrowserCommand, rawURL)
}

func openBrowser(ctx context.Context, goos string, runner commandRunner, rawURL string) error {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
		return errors.New("browser URL must be an absolute http or https URL without credentials")
	}
	var name string
	var args []string
	switch goos {
	case "darwin":
		name, args = "open", []string{rawURL}
	case "linux":
		name, args = "xdg-open", []string{rawURL}
	case "windows":
		name, args = "rundll32", []string{"url.dll,FileProtocolHandler", rawURL}
	default:
		return fmt.Errorf("open browser: unsupported operating system %q", goos)
	}
	if err := runner(ctx, name, args...); err != nil {
		return fmt.Errorf("start browser command: %w", err)
	}
	return nil
}

func startBrowserCommand(ctx context.Context, name string, args ...string) error {
	return exec.CommandContext(ctx, name, args...).Run()
}
