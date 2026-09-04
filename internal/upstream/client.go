// Package upstream calls the one supported backend: ChatGPT Codex Responses.
package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/codexwire"
	"github.com/Aotricx/Clodex/internal/redact"
)

const (
	DefaultEndpoint       = "https://chatgpt.com/backend-api/codex/responses"
	CodexProtocolVersion  = "0.144.6"
	defaultOriginator     = "codex_cli_rs"
	defaultProductVersion = "0.1.0"
	maxDumpBodyBytes      = 32 << 20
)

// Session contains stable per-Claude-conversation identifiers. ThreadID is
// also the pinned x-client-request-id and should be reused across tool turns.
type Session struct {
	SessionID string
	ThreadID  string
}

// AuthenticationError marks credential load/refresh failures so orchestration
// can fail fast with Anthropic's authentication_error instead of retrying them
// as transient network faults.
type AuthenticationError struct {
	Err error
}

func (e *AuthenticationError) Error() string {
	if e == nil || e.Err == nil {
		return "ChatGPT OAuth authentication failed"
	}
	return redact.Text(e.Err.Error())
}

func (e *AuthenticationError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// Client is the concrete authenticated ChatGPT Codex HTTP/SSE transport.
// Endpoint exists only for loopback capture/tests; production uses DefaultEndpoint.
type Client struct {
	HTTPClient *http.Client
	Auth       *auth.Coordinator
	Endpoint   string
	DumpDir    string
	DebugWire  bool
	// OnDumpError receives diagnostic persistence failures without replacing
	// the real upstream response. It may be called concurrently.
	OnDumpError func(error)

	dumpSequence atomic.Uint64
	defaultOnce  sync.Once
	defaultHTTP  *http.Client
}

// Stream sends one stateless full-history Responses request. Auth handles one
// forced refresh and retry on HTTP 401 before the response is returned.
func (c *Client) Stream(ctx context.Context, session Session, request codexwire.Request) (*http.Response, error) {
	if ctx == nil {
		return nil, errors.New("call Codex: nil context")
	}
	if c == nil {
		return nil, errors.New("call Codex: nil client")
	}
	if c.Auth == nil {
		return nil, errors.New("call Codex: auth coordinator is nil")
	}
	endpoint, err := c.endpointURL()
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(session.SessionID) == "" || strings.TrimSpace(session.ThreadID) == "" {
		return nil, errors.New("call Codex: session and thread IDs are required")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("encode Codex request: %w", err)
	}

	credentials, err := c.Auth.Ensure(ctx)
	if err != nil {
		return nil, &AuthenticationError{Err: err}
	}
	attempt := func(attemptContext context.Context, credentials auth.Credentials) (*http.Response, *http.Request, error) {
		httpRequest, requestErr := http.NewRequestWithContext(attemptContext, http.MethodPost, endpoint.String(), bytes.NewReader(body))
		if requestErr != nil {
			return nil, nil, fmt.Errorf("create Codex request: %w", requestErr)
		}
		setPinnedHeaders(httpRequest, credentials, session, request)
		response, requestErr := c.httpClient().Do(httpRequest)
		return response, httpRequest, requestErr
	}

	response, sentRequest, err := attempt(ctx, credentials)
	if err != nil {
		c.dumpTransportError(sentRequest, body, err)
		return nil, err
	}
	if response == nil {
		return nil, errors.New("call Codex: HTTP client returned nil response")
	}
	if response.StatusCode == http.StatusUnauthorized {
		if err := c.captureNonSuccess(sentRequest, body, response); err != nil {
			c.reportDumpError(err)
		}
		_ = response.Body.Close()
		credentials, err = c.Auth.Recover401(ctx, credentials.AccessToken())
		if err != nil {
			return nil, &AuthenticationError{Err: err}
		}
		response, sentRequest, err = attempt(ctx, credentials)
		if err != nil {
			c.dumpTransportError(sentRequest, body, err)
			return nil, err
		}
		if response == nil {
			return nil, errors.New("call Codex: HTTP client returned nil response after authentication refresh")
		}
	}

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		if err := c.captureNonSuccess(sentRequest, body, response); err != nil {
			c.reportDumpError(err)
		}
		return response, nil
	}
	if !isEventStreamResponse(response) {
		contentType := ""
		if response.Header != nil {
			contentType = response.Header.Get("Content-Type")
		}
		response.StatusCode = http.StatusUnsupportedMediaType
		if err := c.captureNonSuccess(sentRequest, body, response); err != nil {
			c.reportDumpError(err)
		}
		if response.Body != nil {
			_ = response.Body.Close()
		}
		explanation := fmt.Sprintf(`{"error":{"type":"api_error","message":"unexpected Codex content type %q, want text/event-stream"}}`, contentType)
		response.Body = io.NopCloser(strings.NewReader(explanation))
		response.ContentLength = int64(len(explanation))
		return response, nil
	}
	if c.DebugWire && response.Body != nil {
		response.Body = &capturingBody{
			ReadCloser: response.Body,
			limit:      maxDumpBodyBytes,
			finish: func(captured []byte, truncated bool) {
				c.reportDumpError(c.writeDump(sentRequest, body, response, captured, truncated))
			},
		}
	}
	return response, nil
}

// DumpRetry persists one redacted wire pair for a retry decided after a
// successful HTTP response, such as an empty semantic completion. Diagnostic
// failures are reported through OnDumpError and never replace the real turn.
func (c *Client) DumpRetry(ctx context.Context, session Session, request codexwire.Request, reason string, attempt int, event json.RawMessage) {
	if c == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	endpoint, err := c.endpointURL()
	if err != nil {
		c.reportDumpError(err)
		return
	}
	body, err := json.Marshal(request)
	if err != nil {
		c.reportDumpError(fmt.Errorf("encode semantic retry request dump: %w", err))
		return
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), nil)
	if err != nil {
		c.reportDumpError(fmt.Errorf("create semantic retry request dump: %w", err))
		return
	}
	httpRequest.Header.Set("originator", defaultOriginator)
	httpRequest.Header.Set("version", CodexProtocolVersion)
	httpRequest.Header.Set("session-id", session.SessionID)
	httpRequest.Header.Set("thread-id", session.ThreadID)
	httpRequest.Header.Set("x-client-request-id", session.ThreadID)
	httpRequest.Header.Set("Content-Type", "application/json")
	response := &http.Response{Header: make(http.Header)}
	response.Header.Set("Content-Type", "application/json")
	response.Header.Set("X-Clodex-Retry-Reason", redact.Text(reason))
	response.Header.Set("X-Clodex-Retry-Attempt", fmt.Sprintf("%d", attempt))
	c.reportDumpError(c.writeDump(httpRequest, body, response, event, false))
}

func (c *Client) endpointURL() (*url.URL, error) {
	value := c.Endpoint
	if value == "" {
		value = DefaultEndpoint
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("parse Codex endpoint: %w", err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("parse Codex endpoint: credentials, query, and fragment are forbidden")
	}
	production := parsed.Scheme == "https" && strings.EqualFold(parsed.Hostname(), "chatgpt.com") &&
		parsed.Path == "/backend-api/codex/responses"
	if !production && !isLoopbackHost(parsed.Hostname()) {
		return nil, errors.New("parse Codex endpoint: HTTPS chatgpt.com endpoint required; only loopback test endpoints may use HTTP")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("parse Codex endpoint: HTTP or HTTPS scheme required")
	}
	if parsed.Host == "" {
		return nil, errors.New("parse Codex endpoint: host is required")
	}
	return parsed, nil
}

func setPinnedHeaders(request *http.Request, credentials auth.Credentials, session Session, wire codexwire.Request) {
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken())
	if credentials.AccountID() != "" {
		request.Header.Set("ChatGPT-Account-ID", credentials.AccountID())
	}
	request.Header.Set("originator", defaultOriginator)
	request.Header.Set("version", CodexProtocolVersion)
	request.Header.Set("session-id", session.SessionID)
	request.Header.Set("thread-id", session.ThreadID)
	request.Header.Set("x-client-request-id", session.ThreadID)
	request.Header.Set("User-Agent", fmt.Sprintf("%s/%s (%s %s) clodex/%s", defaultOriginator, CodexProtocolVersion, runtime.GOOS, runtime.GOARCH, defaultProductVersion))
	request.Header.Set("Accept", "text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	if wire.Reasoning != nil && wire.Reasoning.Context == codexwire.ReasoningContextAllTurns {
		request.Header.Set("x-openai-internal-codex-responses-lite", "true")
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	c.defaultOnce.Do(func() {
		c.defaultHTTP = &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			MaxIdleConnsPerHost:   32,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: time.Second,
		}}
	})
	return c.defaultHTTP
}

func (c *Client) captureNonSuccess(request *http.Request, requestBody []byte, response *http.Response) error {
	if response.Body == nil {
		return c.writeDump(request, requestBody, response, nil, false)
	}
	prefix, err := io.ReadAll(io.LimitReader(response.Body, maxDumpBodyBytes+1))
	response.Body = struct {
		io.Reader
		io.Closer
	}{Reader: io.MultiReader(bytes.NewReader(prefix), response.Body), Closer: response.Body}
	if err != nil {
		return fmt.Errorf("read Codex failure body for redacted dump: %w", err)
	}
	truncated := len(prefix) > maxDumpBodyBytes
	captured := prefix
	if truncated {
		captured = prefix[:maxDumpBodyBytes]
	}
	return c.writeDump(request, requestBody, response, captured, truncated)
}

func (c *Client) dumpTransportError(request *http.Request, requestBody []byte, err error) {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	c.reportDumpError(c.writeDump(request, requestBody, nil, []byte(err.Error()), false))
}

func (c *Client) reportDumpError(err error) {
	if err != nil && c.OnDumpError != nil {
		c.OnDumpError(err)
	}
}

func (c *Client) writeDump(request *http.Request, requestBody []byte, response *http.Response, responseBody []byte, truncated bool) error {
	directory, err := c.dumpDirectory()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create redacted wire dump directory: %w", err)
	}

	requestHeaders := http.Header(nil)
	requestURL := ""
	if request != nil {
		requestHeaders = redact.Headers(request.Header)
		requestURL = redact.URL(request.URL).String()
	}
	responseHeaders := http.Header(nil)
	status := 0
	contentType := ""
	if response != nil {
		responseHeaders = redact.Headers(response.Header)
		status = response.StatusCode
		contentType = response.Header.Get("Content-Type")
	}

	record := wireDump{
		Request: wireDumpSide{
			URL:     requestURL,
			Headers: requestHeaders,
			Body:    redactBody(requestBody, "application/json"),
		},
		Response: wireDumpSide{
			Status:        status,
			Headers:       responseHeaders,
			Body:          redactBody(responseBody, contentType),
			BodyTruncated: truncated,
		},
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
		return fmt.Errorf("encode redacted wire dump: %w", err)
	}
	sequence := c.dumpSequence.Add(1)
	name := fmt.Sprintf("clodex-%s-%06d.wire.json", time.Now().UTC().Format("20060102T150405.000000000Z"), sequence)
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create redacted wire dump: %w", err)
	}
	writeErr := error(nil)
	if _, err := file.Write(encoded.Bytes()); err != nil {
		writeErr = fmt.Errorf("write redacted wire dump: %w", err)
	}
	if err := file.Close(); writeErr == nil && err != nil {
		writeErr = fmt.Errorf("close redacted wire dump: %w", err)
	}
	return writeErr
}

func (c *Client) dumpDirectory() (string, error) {
	if c.DumpDir != "" {
		return c.DumpDir, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("resolve redacted wire dump directory: %w", err)
	}
	return filepath.Join(cache, "clodex", "wire"), nil
}

func redactBody(body []byte, contentType string) string {
	if len(body) == 0 {
		return ""
	}
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return string(redact.SSE(body))
	}
	if json.Valid(body) {
		if redacted, err := redact.JSON(body); err == nil {
			return string(redacted)
		}
	}
	return redact.Text(string(body))
}

func isEventStreamResponse(response *http.Response) bool {
	if response == nil || response.Header == nil {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	return err == nil && strings.EqualFold(mediaType, "text/event-stream")
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

type wireDump struct {
	Request  wireDumpSide `json:"request"`
	Response wireDumpSide `json:"response"`
}

type wireDumpSide struct {
	URL           string      `json:"url,omitempty"`
	Status        int         `json:"status,omitempty"`
	Headers       http.Header `json:"headers,omitempty"`
	Body          string      `json:"body,omitempty"`
	BodyTruncated bool        `json:"body_truncated,omitempty"`
}

type capturingBody struct {
	io.ReadCloser
	limit  int
	finish func([]byte, bool)

	mu        sync.Mutex
	once      sync.Once
	buffer    bytes.Buffer
	truncated bool
	eof       bool
}

func (b *capturingBody) Read(destination []byte) (int, error) {
	n, err := b.ReadCloser.Read(destination)
	b.mu.Lock()
	if n > 0 && b.buffer.Len() < b.limit {
		remaining := b.limit - b.buffer.Len()
		kept := min(n, remaining)
		_, _ = b.buffer.Write(destination[:kept])
		if kept < n {
			b.truncated = true
		}
	} else if n > 0 {
		b.truncated = true
	}
	if errors.Is(err, io.EOF) {
		b.eof = true
	}
	b.mu.Unlock()
	if errors.Is(err, io.EOF) {
		b.complete()
	}
	return n, err
}

func (b *capturingBody) Close() error {
	err := b.ReadCloser.Close()
	b.mu.Lock()
	if !b.eof {
		b.truncated = true
	}
	b.mu.Unlock()
	b.complete()
	return err
}

func (b *capturingBody) complete() {
	b.once.Do(func() {
		b.mu.Lock()
		captured := bytes.Clone(b.buffer.Bytes())
		truncated := b.truncated
		b.mu.Unlock()
		if b.finish != nil {
			b.finish(captured, truncated)
		}
	})
}
