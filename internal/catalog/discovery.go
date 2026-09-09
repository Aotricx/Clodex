package catalog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Aotricx/Clodex/internal/auth"
	"github.com/Aotricx/Clodex/internal/redact"
)

const (
	ModelsEndpoint          = BackendURL + "/models"
	discoveryOriginator     = "codex_cli_rs"
	discoveryProductVersion = "0.1.0"
	maxCatalogResponseBytes = 16 << 20
	maxCatalogErrorBytes    = 64 << 10

	// HTTPTimeout is the live catalog Fetch deadline. Spawned `clodex serve`
	// runs this Fetch before it binds, so launcher ready-wait must exceed it.
	HTTPTimeout = 10 * time.Second
)

var defaultDiscoveryHTTPClient = &http.Client{
	Timeout: HTTPTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// DiscoveryHTTPError is one non-success response from the concrete ChatGPT
// Codex model-catalog endpoint.
type DiscoveryHTTPError struct {
	StatusCode int
	Message    string
	RetryAfter string
}

func (e *DiscoveryHTTPError) Error() string {
	if e == nil {
		return "discover Codex models: unknown HTTP error"
	}
	if e.Message == "" {
		return fmt.Sprintf("discover Codex models: HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("discover Codex models: HTTP %d: %s", e.StatusCode, e.Message)
}

// DiscoveryClient fetches the sole supported remote catalog using shared
// Codex OAuth state. Endpoint is configurable only for loopback tests.
type DiscoveryClient struct {
	HTTPClient *http.Client
	Auth       *auth.Coordinator
	Endpoint   string
	Now        func() time.Time
}

// Fetch retrieves, stamps, and validates the live models response. Auth.Do
// performs at most one forced refresh and retry after an HTTP 401.
func (c *DiscoveryClient) Fetch(ctx context.Context) (Catalog, error) {
	if ctx == nil {
		return Catalog{}, errors.New("discover Codex models: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Catalog{}, err
	}
	if c == nil {
		return Catalog{}, errors.New("discover Codex models: nil client")
	}
	if c.Auth == nil {
		return Catalog{}, errors.New("discover Codex models: auth coordinator is nil")
	}
	endpoint, err := c.endpointURL()
	if err != nil {
		return Catalog{}, err
	}

	response, err := c.Auth.Do(ctx, func(attemptContext context.Context, credentials auth.Credentials) (*http.Response, error) {
		request, requestErr := http.NewRequestWithContext(attemptContext, http.MethodGet, endpoint.String(), nil)
		if requestErr != nil {
			return nil, fmt.Errorf("discover Codex models: create request: %w", requestErr)
		}
		setDiscoveryHeaders(request, credentials)
		return c.httpClient().Do(request)
	})
	if err != nil {
		return Catalog{}, fmt.Errorf("discover Codex models: request failed: %w", err)
	}
	if response == nil {
		return Catalog{}, errors.New("discover Codex models: HTTP client returned nil response")
	}
	defer response.Body.Close()
	limit := int64(maxCatalogResponseBytes)
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		limit = maxCatalogErrorBytes
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return Catalog{}, fmt.Errorf("discover Codex models: read response: %w", err)
	}
	if int64(len(body)) > limit {
		return Catalog{}, fmt.Errorf("discover Codex models: response exceeds %d bytes", limit)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return Catalog{}, &DiscoveryHTTPError{
			StatusCode: response.StatusCode,
			Message:    discoveryErrorMessage(body),
			RetryAfter: response.Header.Get("Retry-After"),
		}
	}

	catalog, err := parseLiveCatalog(body, c.now(), response.Header.Get("ETag"))
	if err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func (c *DiscoveryClient) endpointURL() (*url.URL, error) {
	value := c.Endpoint
	if value == "" {
		value = ModelsEndpoint
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("discover Codex models: parse endpoint: %w", err)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("discover Codex models: endpoint credentials, query, and fragment are forbidden")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, errors.New("discover Codex models: endpoint must use HTTP or HTTPS")
	}
	if parsed.Host == "" {
		return nil, errors.New("discover Codex models: endpoint host is required")
	}
	production := parsed.Scheme == "https" && strings.EqualFold(parsed.Host, "chatgpt.com") &&
		parsed.Path == "/backend-api/codex/models"
	if !production && !isDiscoveryLoopback(parsed.Hostname()) {
		return nil, errors.New("discover Codex models: exact HTTPS chatgpt.com models endpoint required; only loopback tests may override it")
	}
	query := parsed.Query()
	query.Set("client_version", ClientVersion)
	parsed.RawQuery = query.Encode()
	return parsed, nil
}

func isDiscoveryLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	address := net.ParseIP(host)
	return address != nil && address.IsLoopback()
}

func setDiscoveryHeaders(request *http.Request, credentials auth.Credentials) {
	request.Header.Set("Authorization", "Bearer "+credentials.AccessToken())
	if credentials.AccountID() != "" {
		request.Header.Set("ChatGPT-Account-ID", credentials.AccountID())
	}
	request.Header.Set("originator", discoveryOriginator)
	request.Header.Set("version", ClientVersion)
	request.Header.Set("User-Agent", fmt.Sprintf("%s/%s (%s %s) clodex/%s", discoveryOriginator, ClientVersion, runtime.GOOS, runtime.GOARCH, discoveryProductVersion))
}

func (c *DiscoveryClient) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultDiscoveryHTTPClient
}

func (c *DiscoveryClient) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

func discoveryErrorMessage(body []byte) string {
	var document struct {
		Detail  string `json:"detail"`
		Message string `json:"message"`
		Error   any    `json:"error"`
	}
	if json.Unmarshal(body, &document) == nil {
		if document.Detail != "" {
			return redact.Text(document.Detail)
		}
		if document.Message != "" {
			return redact.Text(document.Message)
		}
		switch value := document.Error.(type) {
		case string:
			if value != "" {
				return redact.Text(value)
			}
		case map[string]any:
			if message, ok := value["message"].(string); ok && message != "" {
				return redact.Text(message)
			}
		}
	}
	return strings.TrimSpace(redact.Text(string(body)))
}

func parseLiveCatalog(data []byte, fetchedAt time.Time, etag string) (Catalog, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var remote struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := decoder.Decode(&remote); err != nil {
		return Catalog{}, fmt.Errorf("discover Codex models: decode response: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Catalog{}, errors.New("discover Codex models: trailing JSON value")
		}
		return Catalog{}, fmt.Errorf("discover Codex models: trailing response data: %w", err)
	}
	if remote.Models == nil {
		return Catalog{}, errors.New("discover Codex models: response models field is required")
	}
	wire := envelope{
		FetchedAt:     fetchedAt.UTC(),
		ETag:          etag,
		ClientVersion: ClientVersion,
		Backend:       BackendURL,
		Models:        remote.Models,
	}
	stamped, err := json.Marshal(wire)
	if err != nil {
		return Catalog{}, fmt.Errorf("discover Codex models: stamp response: %w", err)
	}
	catalog, err := Parse(stamped, SourceLive)
	if err != nil {
		return Catalog{}, fmt.Errorf("discover Codex models: validate response: %w", err)
	}
	return catalog, nil
}

// Resolution reports which catalog was selected and preserves all recoverable
// cache/live failures for status telemetry.
type Resolution struct {
	Catalog    Catalog
	Source     Source
	Age        time.Duration
	CacheError error
	LiveError  error
}

type discoveryFlight struct {
	done   chan struct{}
	result Resolution
	err    error
}

// Manager resolves fresh cache, live discovery, stale cache, then embedded
// fallback in that order. Refresh calls on one Manager are coalesced.
type Manager struct {
	CachePath string
	Discovery *DiscoveryClient
	TTL       time.Duration
	Now       func() time.Time

	mu     sync.Mutex
	flight *discoveryFlight
}

// Resolve returns the best valid catalog available without erasing failures
// that caused a stale-cache or fallback selection.
func (m *Manager) Resolve(ctx context.Context) (Resolution, error) {
	if ctx == nil {
		return Resolution{}, errors.New("resolve Codex catalog: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Resolution{}, err
	}
	if m == nil {
		return Resolution{}, errors.New("resolve Codex catalog: nil manager")
	}
	if strings.TrimSpace(m.CachePath) == "" {
		return Resolution{}, errors.New("resolve Codex catalog: cache path is required")
	}
	now := m.now()
	cached, cacheErr := m.loadMatchingCache()
	if cacheErr == nil && cached != nil && cached.Fresh(now, m.ttl(), ClientVersion, BackendURL) {
		return detachedResolution(Resolution{Catalog: *cached, Source: SourceCache, Age: cached.Age(now)}), nil
	}

	m.mu.Lock()
	if flight := m.flight; flight != nil {
		m.mu.Unlock()
		select {
		case <-ctx.Done():
			return Resolution{}, ctx.Err()
		case <-flight.done:
			return detachedResolution(flight.result), flight.err
		}
	}
	flight := &discoveryFlight{done: make(chan struct{})}
	m.flight = flight
	m.mu.Unlock()

	func() {
		defer func() {
			m.mu.Lock()
			if m.flight == flight {
				m.flight = nil
			}
			close(flight.done)
			m.mu.Unlock()
		}()
		flight.result, flight.err = m.resolveSlow(ctx, cached, cacheErr, now)
	}()
	return detachedResolution(flight.result), flight.err
}

func (m *Manager) resolveSlow(ctx context.Context, cached *Catalog, cacheErr error, now time.Time) (Resolution, error) {
	var live Catalog
	var liveErr error
	if m.Discovery == nil {
		liveErr = errors.New("resolve Codex catalog: discovery client is nil")
	} else {
		live, liveErr = m.Discovery.Fetch(ctx)
	}
	if liveErr == nil {
		if err := Save(m.CachePath, live); err != nil {
			cacheErr = errors.Join(cacheErr, fmt.Errorf("resolve Codex catalog: save live cache: %w", err))
		}
		return Resolution{Catalog: live, Source: SourceLive, Age: live.Age(now), CacheError: cacheErr}, nil
	}

	if cached != nil {
		return Resolution{
			Catalog:    *cached,
			Source:     SourceCache,
			Age:        cached.Age(now),
			CacheError: cacheErr,
			LiveError:  liveErr,
		}, nil
	}
	fallback, fallbackErr := LoadFallback()
	if fallbackErr != nil {
		return Resolution{}, fmt.Errorf("resolve Codex catalog: load embedded fallback: %w", fallbackErr)
	}
	return Resolution{
		Catalog:    fallback,
		Source:     SourceFallback,
		Age:        0,
		CacheError: cacheErr,
		LiveError:  liveErr,
	}, nil
}

func (m *Manager) loadMatchingCache() (*Catalog, error) {
	catalog, err := LoadFile(m.CachePath, SourceCache)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("resolve Codex catalog: load cache: %w", err)
	}
	// Identity is only a freshness gate. A parseable, validated cache with a
	// mismatched client_version or backend is still preferred over embedded
	// fallback when live discovery fails.
	return &catalog, nil
}

func (m *Manager) ttl() time.Duration {
	if m.TTL <= 0 {
		return DefaultTTL
	}
	return m.TTL
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func detachedResolution(source Resolution) Resolution {
	source.Catalog = cloneCatalog(source.Catalog)
	return source
}

func cloneCatalog(source Catalog) Catalog {
	clone := source
	clone.Models = make([]Model, len(source.Models))
	for index, model := range source.Models {
		clone.Models[index] = model
		clone.Models[index].SupportedReasoningLevels = append([]ReasoningLevel(nil), model.SupportedReasoningLevels...)
		clone.Models[index].AdditionalSpeedTiers = append([]string(nil), model.AdditionalSpeedTiers...)
		clone.Models[index].ServiceTiers = append([]ServiceTier(nil), model.ServiceTiers...)
		clone.Models[index].InputModalities = append([]string(nil), model.InputModalities...)
		clone.Models[index].Raw = append(json.RawMessage(nil), model.Raw...)
		if model.AutoCompactTokenLimit != nil {
			value := *model.AutoCompactTokenLimit
			clone.Models[index].AutoCompactTokenLimit = &value
		}
	}
	return clone
}
