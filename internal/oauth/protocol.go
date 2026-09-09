package oauth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Aotricx/Clodex/internal/redact"
)

const (
	DefaultClientID       = "app_EMoamEEZ73f0CkXaXp7hrann"
	DefaultIssuer         = "https://auth.openai.com"
	Scopes                = "openid profile email offline_access api.connectors.read api.connectors.invoke"
	DeviceVerificationURI = "https://auth.openai.com/codex/device"
	defaultOriginator     = "codex_cli_rs"
	defaultDeviceTimeout  = 15 * time.Minute
	maxErrorBodyBytes     = 4096
	defaultHTTPTimeout    = 30 * time.Second
)

var defaultHTTPClient = &http.Client{
	Timeout: defaultHTTPTimeout,
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type PKCE struct {
	Verifier  string
	Challenge string
}

type TokenSet struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"`
}

type DeviceCode struct {
	VerificationURI string
	UserCode        string
	DeviceAuthID    string
	Interval        time.Duration
}

type Client struct {
	Issuer        string
	ClientID      string
	HTTPClient    *http.Client
	Random        io.Reader
	Now           func() time.Time
	Sleep         func(context.Context, time.Duration) error
	DeviceTimeout time.Duration
	CallbackPorts []uint16
}

type HTTPError struct {
	StatusCode int
	RetryAfter string
	Body       string
}

func (e *HTTPError) Error() string {
	if e.Body == "" {
		return fmt.Sprintf("oauth endpoint returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("oauth endpoint returned HTTP %d: %s", e.StatusCode, e.Body)
}

func GeneratePKCE(random io.Reader) (PKCE, error) {
	if random == nil {
		random = rand.Reader
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(random, buf); err != nil {
		return PKCE{}, fmt.Errorf("generate PKCE verifier: %w", err)
	}
	verifier := base64.RawURLEncoding.EncodeToString(buf)
	digest := sha256.Sum256([]byte(verifier))
	return PKCE{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(digest[:]),
	}, nil
}

func (c *Client) AuthorizeURL(redirectURI string, pkce PKCE, state string) (string, error) {
	endpoint, err := c.endpoint("/oauth/authorize")
	if err != nil {
		return "", err
	}
	values := endpoint.Query()
	values.Set("response_type", "code")
	values.Set("client_id", c.clientID())
	values.Set("redirect_uri", redirectURI)
	values.Set("scope", Scopes)
	values.Set("code_challenge", pkce.Challenge)
	values.Set("code_challenge_method", "S256")
	values.Set("id_token_add_organizations", "true")
	values.Set("codex_cli_simplified_flow", "true")
	values.Set("state", state)
	values.Set("originator", defaultOriginator)
	endpoint.RawQuery = values.Encode()
	return endpoint.String(), nil
}

func (c *Client) ExchangeCode(ctx context.Context, code, redirectURI string, pkce PKCE) (TokenSet, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {redirectURI},
		"client_id":     {c.clientID()},
		"code_verifier": {pkce.Verifier},
	}
	endpoint, err := c.endpoint("/oauth/token")
	if err != nil {
		return TokenSet{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), strings.NewReader(form.Encode()))
	if err != nil {
		return TokenSet{}, fmt.Errorf("create authorization-code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var tokens TokenSet
	if err := c.doJSON(req, &tokens, code, pkce.Verifier); err != nil {
		return TokenSet{}, err
	}
	if tokens.IDToken == "" {
		return TokenSet{}, errors.New("token response missing id_token")
	}
	if tokens.AccessToken == "" {
		return TokenSet{}, errors.New("token response missing access_token")
	}
	if tokens.RefreshToken == "" {
		return TokenSet{}, errors.New("token response missing refresh_token")
	}
	return tokens, nil
}

func (c *Client) Refresh(ctx context.Context, refreshToken string) (TokenSet, error) {
	body, err := json.Marshal(struct {
		ClientID     string `json:"client_id"`
		GrantType    string `json:"grant_type"`
		RefreshToken string `json:"refresh_token"`
	}{
		ClientID:     c.clientID(),
		GrantType:    "refresh_token",
		RefreshToken: refreshToken,
	})
	if err != nil {
		return TokenSet{}, fmt.Errorf("encode refresh request: %w", err)
	}
	endpoint, err := c.endpoint("/oauth/token")
	if err != nil {
		return TokenSet{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return TokenSet{}, fmt.Errorf("create refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var tokens TokenSet
	if err := c.doJSON(req, &tokens, refreshToken); err != nil {
		return TokenSet{}, err
	}
	if tokens.AccessToken == "" {
		return TokenSet{}, errors.New("token response missing access_token")
	}
	return tokens, nil
}

func (c *Client) StartDevice(ctx context.Context) (DeviceCode, error) {
	body, err := json.Marshal(struct {
		ClientID string `json:"client_id"`
	}{ClientID: c.clientID()})
	if err != nil {
		return DeviceCode{}, fmt.Errorf("encode device-code request: %w", err)
	}
	endpoint, err := c.endpoint("/api/accounts/deviceauth/usercode")
	if err != nil {
		return DeviceCode{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return DeviceCode{}, fmt.Errorf("create device-code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	var response struct {
		DeviceAuthID string         `json:"device_auth_id"`
		UserCode     string         `json:"user_code"`
		UserCodeAlt  string         `json:"usercode"`
		Interval     stringInterval `json:"interval"`
	}
	if err := c.doJSON(req, &response); err != nil {
		return DeviceCode{}, err
	}
	if response.UserCode == "" {
		response.UserCode = response.UserCodeAlt
	}
	if response.DeviceAuthID == "" {
		return DeviceCode{}, errors.New("device-code response missing device_auth_id")
	}
	if response.UserCode == "" {
		return DeviceCode{}, errors.New("device-code response missing user_code")
	}
	if response.Interval == 0 {
		return DeviceCode{}, errors.New("device-code response interval must be positive")
	}
	issuer := strings.TrimRight(c.issuer(), "/")
	return DeviceCode{
		VerificationURI: issuer + "/codex/device",
		UserCode:        response.UserCode,
		DeviceAuthID:    response.DeviceAuthID,
		Interval:        time.Duration(response.Interval) * time.Second,
	}, nil
}

func (c *Client) PollDevice(ctx context.Context, device DeviceCode) (TokenSet, error) {
	if device.Interval <= 0 {
		return TokenSet{}, errors.New("device polling interval must be positive")
	}
	start := c.now()()
	timeout := c.DeviceTimeout
	if timeout <= 0 {
		timeout = defaultDeviceTimeout
	}
	deadline := start.Add(timeout)
	interval := device.Interval

	for {
		if err := ctx.Err(); err != nil {
			return TokenSet{}, err
		}
		if !c.now()().Before(deadline) {
			return TokenSet{}, errors.New("device auth timed out after 15 minutes")
		}
		response, pending, err := c.pollDeviceOnce(ctx, device)
		if err != nil {
			return TokenSet{}, err
		}
		if !pending {
			redirectURI := strings.TrimRight(c.issuer(), "/") + "/deviceauth/callback"
			return c.ExchangeCode(ctx, response.AuthorizationCode, redirectURI, PKCE{
				Verifier:  response.CodeVerifier,
				Challenge: response.CodeChallenge,
			})
		}

		remaining := deadline.Sub(c.now()())
		if remaining <= 0 {
			return TokenSet{}, errors.New("device auth timed out after 15 minutes")
		}
		sleepFor := min(interval, remaining)
		if err := c.sleep()(ctx, sleepFor); err != nil {
			return TokenSet{}, err
		}
	}
}

type devicePollResponse struct {
	AuthorizationCode string `json:"authorization_code"`
	CodeChallenge     string `json:"code_challenge"`
	CodeVerifier      string `json:"code_verifier"`
}

func (c *Client) pollDeviceOnce(ctx context.Context, device DeviceCode) (devicePollResponse, bool, error) {
	body, err := json.Marshal(struct {
		DeviceAuthID string `json:"device_auth_id"`
		UserCode     string `json:"user_code"`
	}{DeviceAuthID: device.DeviceAuthID, UserCode: device.UserCode})
	if err != nil {
		return devicePollResponse{}, false, fmt.Errorf("encode device poll request: %w", err)
	}
	endpoint, err := c.endpoint("/api/accounts/deviceauth/token")
	if err != nil {
		return devicePollResponse{}, false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return devicePollResponse{}, false, fmt.Errorf("create device poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return devicePollResponse{}, false, ctx.Err()
		}
		return devicePollResponse{}, false, fmt.Errorf("device poll request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxErrorBodyBytes))
		return devicePollResponse{}, true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return devicePollResponse{}, false, newHTTPError(resp, device.DeviceAuthID, device.UserCode)
	}
	var response devicePollResponse
	if err := decodeJSON(resp.Body, &response); err != nil {
		return devicePollResponse{}, false, fmt.Errorf("decode device poll response: %w", err)
	}
	if response.AuthorizationCode == "" || response.CodeChallenge == "" || response.CodeVerifier == "" {
		return devicePollResponse{}, false, errors.New("device poll response missing authorization_code, code_challenge, or code_verifier")
	}
	return response, false, nil
}

func (c *Client) doJSON(req *http.Request, target any, secrets ...string) error {
	resp, err := c.httpClient().Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return req.Context().Err()
		}
		return fmt.Errorf("oauth request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return newHTTPError(resp, secrets...)
	}
	if err := decodeJSON(resp.Body, target); err != nil {
		return fmt.Errorf("decode oauth response: %w", err)
	}
	return nil
}

func decodeJSON(r io.Reader, target any) error {
	decoder := json.NewDecoder(io.LimitReader(r, 1<<20))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("trailing JSON value")
		}
		return err
	}
	return nil
}

func newHTTPError(resp *http.Response, secrets ...string) *HTTPError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes+1))
	truncated := len(body) > maxErrorBodyBytes
	if truncated {
		body = body[:maxErrorBodyBytes]
	}
	redacted := redactErrorBody(body)
	for _, secret := range secrets {
		if secret != "" {
			redacted = strings.ReplaceAll(redacted, secret, "<redacted>")
			redacted = strings.ReplaceAll(redacted, url.QueryEscape(secret), "<redacted>")
		}
	}
	if truncated {
		redacted += "..."
	}
	return &HTTPError{
		StatusCode: resp.StatusCode,
		RetryAfter: resp.Header.Get("Retry-After"),
		Body:       redacted,
	}
}

func redactErrorBody(body []byte) string {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		return ""
	}
	if redacted, err := redact.JSON(body); err == nil {
		return string(redacted)
	}
	return redact.Text(redactMalformedStructuredFields(trimmed))
}

func redactMalformedStructuredFields(input string) string {
	var output strings.Builder
	for cursor := 0; cursor < len(input); {
		keyStart := strings.IndexByte(input[cursor:], '"')
		if keyStart < 0 {
			output.WriteString(input[cursor:])
			break
		}
		keyStart += cursor
		keyEnd := quotedStringEnd(input, keyStart+1)
		if keyEnd < 0 {
			output.WriteString(input[cursor:])
			break
		}
		valueStart := keyEnd + 1
		for valueStart < len(input) && (input[valueStart] == ' ' || input[valueStart] == '\t' || input[valueStart] == '\r' || input[valueStart] == '\n') {
			valueStart++
		}
		if valueStart >= len(input) || input[valueStart] != ':' || !redact.SensitiveName(input[keyStart+1:keyEnd]) {
			output.WriteString(input[cursor : keyEnd+1])
			cursor = keyEnd + 1
			continue
		}
		valueStart++
		for valueStart < len(input) && (input[valueStart] == ' ' || input[valueStart] == '\t' || input[valueStart] == '\r' || input[valueStart] == '\n') {
			valueStart++
		}
		output.WriteString(input[cursor:valueStart])
		if valueStart >= len(input) {
			output.WriteString(redact.Marker)
			break
		}
		if input[valueStart] == '"' {
			valueEnd := quotedStringEnd(input, valueStart+1)
			output.WriteByte('"')
			output.WriteString(redact.Marker)
			output.WriteByte('"')
			if valueEnd < 0 {
				break
			}
			cursor = valueEnd + 1
			continue
		}
		output.WriteString(redact.Marker)
		valueEnd := strings.IndexAny(input[valueStart:], ",}\r\n")
		if valueEnd < 0 {
			break
		}
		cursor = valueStart + valueEnd
	}
	return output.String()
}

func quotedStringEnd(input string, start int) int {
	escaped := false
	for i := start; i < len(input); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch input[i] {
		case '\\':
			escaped = true
		case '"':
			return i
		}
	}
	return -1
}

func (c *Client) endpoint(path string) (*url.URL, error) {
	issuer, err := url.Parse(c.issuer())
	if err != nil {
		return nil, fmt.Errorf("parse OAuth issuer: %w", err)
	}
	if issuer.Scheme != "http" && issuer.Scheme != "https" {
		return nil, errors.New("OAuth issuer must use http or https")
	}
	if issuer.Host == "" {
		return nil, errors.New("OAuth issuer must include a host")
	}
	issuer.Path = strings.TrimRight(issuer.Path, "/") + path
	issuer.RawPath = ""
	issuer.RawQuery = ""
	issuer.Fragment = ""
	return issuer, nil
}

func (c *Client) issuer() string {
	if strings.TrimSpace(c.Issuer) == "" {
		return DefaultIssuer
	}
	return c.Issuer
}

func (c *Client) clientID() string {
	if strings.TrimSpace(c.ClientID) == "" {
		return DefaultClientID
	}
	return c.ClientID
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient == nil {
		return defaultHTTPClient
	}
	return c.HTTPClient
}

func (c *Client) now() func() time.Time {
	if c.Now == nil {
		return time.Now
	}
	return c.Now
}

func (c *Client) sleep() func(context.Context, time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep
	}
	return func(ctx context.Context, duration time.Duration) error {
		timer := time.NewTimer(duration)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
}

type stringInterval uint64

func (i *stringInterval) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		*i = 0
		return nil
	}
	var raw string
	if err := json.Unmarshal(data, &raw); err != nil {
		return errors.New("device interval must be a string")
	}
	seconds, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return fmt.Errorf("parse device interval: %w", err)
	}
	*i = stringInterval(seconds)
	return nil
}
