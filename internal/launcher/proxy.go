package launcher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	maxHealthResponseBytes = 64 << 10
	healthRequestTimeout   = time.Second
	proxyCleanupTimeout    = 5 * time.Second
)

var (
	// ErrProxyUnavailable means no HTTP service answered the loopback probe.
	ErrProxyUnavailable = errors.New("Clodex proxy unavailable")
	// ErrForeignListener means the port answered but did not prove Clodex identity.
	ErrForeignListener = errors.New("foreign listener on Clodex port")
)

// Health is the exact identity contract served by Clodex /healthz.
type Health struct {
	Service string `json:"service"`
	Status  string `json:"status"`
	Version string `json:"version"`
}

// ProbeFunc probes one exact health URL.
type ProbeFunc func(context.Context, string) (Health, error)

// ProbeHealth performs one bounded GET and accepts only an HTTP 200 JSON Clodex
// identity with status ok and a non-empty version.
func ProbeHealth(ctx context.Context, target string) (Health, error) {
	if ctx == nil {
		return Health{}, errors.New("probe Clodex health: nil context")
	}
	if err := ctx.Err(); err != nil {
		return Health{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return Health{}, fmt.Errorf("probe Clodex health: create request: %w", err)
	}
	client := &http.Client{
		Timeout: healthRequestTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Health{}, ctx.Err()
		}
		kind := ErrProxyUnavailable
		if isProbeTimeout(err) {
			kind = ErrForeignListener
		}
		return Health{}, fmt.Errorf("%w at %s: %v", kind, target, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return Health{}, fmt.Errorf("%w at %s: HTTP %d", ErrForeignListener, target, response.StatusCode)
	}

	limited := io.LimitReader(response.Body, maxHealthResponseBytes+1)
	decoder := json.NewDecoder(limited)
	var health Health
	if err := decoder.Decode(&health); err != nil {
		return Health{}, fmt.Errorf("%w at %s: invalid health JSON: %v", ErrForeignListener, target, err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return Health{}, fmt.Errorf("%w at %s: trailing health JSON", ErrForeignListener, target)
		}
		return Health{}, fmt.Errorf("%w at %s: invalid trailing health data: %v", ErrForeignListener, target, err)
	}
	if err := validateHealth(target, health); err != nil {
		return Health{}, err
	}
	return health, nil
}

func isProbeTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}

func validateHealth(target string, health Health) error {
	if health.Service != "clodex" || health.Status != "ok" || strings.TrimSpace(health.Version) == "" {
		return fmt.Errorf("%w at %s: service=%q status=%q version=%q", ErrForeignListener, target, health.Service, health.Status, health.Version)
	}
	return nil
}

func ensureProxy(ctx context.Context, healthURL string, environment []string, options Options, dependencies Dependencies) error {
	err := probeOnce(ctx, healthURL, dependencies.Probe)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrProxyUnavailable) {
		return fmt.Errorf("run Claude Code: refusing proxy reuse: %w", err)
	}

	executable, err := dependencies.Executable()
	if err != nil {
		return fmt.Errorf("run Claude Code: resolve current Clodex executable: %w", err)
	}
	if strings.TrimSpace(executable) == "" {
		return errors.New("run Claude Code: current executable lookup returned an empty path")
	}
	process, err := dependencies.Spawn(context.Background(), Command{
		Path:               executable,
		Args:               []string{"serve", "--port", strconv.Itoa(options.Port)},
		Env:                append([]string(nil), environment...),
		Stdout:             dependencies.Stdout,
		Stderr:             dependencies.Stderr,
		DetachProcessGroup: true,
	})
	if err != nil {
		return fmt.Errorf("run Claude Code: start Clodex proxy: %w", err)
	}
	if process == nil {
		return errors.New("run Claude Code: start Clodex proxy returned a nil process")
	}

	exited := make(chan error, 1)
	go func() {
		exited <- process.Wait()
	}()
	return waitForProxy(ctx, healthURL, process, exited, options, dependencies.Probe)
}

func waitForProxy(ctx context.Context, healthURL string, process Process, exited <-chan error, options Options, probe ProbeFunc) error {
	deadline := time.NewTimer(options.ReadyTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(options.ProbeInterval)
	defer ticker.Stop()

	for {
		err := probeOnce(ctx, healthURL, probe)
		if err == nil {
			return nil
		}
		if !errors.Is(err, ErrProxyUnavailable) {
			cleanupErr := stopFailedProxy(process, exited)
			return errors.Join(fmt.Errorf("run Claude Code: spawned proxy failed identity check: %w", err), cleanupErr)
		}

		select {
		case <-ctx.Done():
			cleanupErr := stopFailedProxy(process, exited)
			return errors.Join(ctx.Err(), cleanupErr)
		case waitErr := <-exited:
			probeErr := probeOnce(ctx, healthURL, probe)
			if probeErr == nil {
				return nil
			}
			if !errors.Is(probeErr, ErrProxyUnavailable) {
				return fmt.Errorf("run Claude Code: spawned proxy failed identity check: %w", probeErr)
			}
			if waitErr == nil {
				return errors.New("run Claude Code: proxy exited before becoming healthy")
			}
			return fmt.Errorf("run Claude Code: proxy exited before becoming healthy: %w", waitErr)
		case <-deadline.C:
			cleanupErr := stopFailedProxy(process, exited)
			return errors.Join(fmt.Errorf("run Claude Code: proxy did not become healthy within %s", options.ReadyTimeout), cleanupErr)
		case <-ticker.C:
		}
	}
}

func probeOnce(ctx context.Context, target string, probe ProbeFunc) error {
	health, err := probe(ctx, target)
	if err != nil {
		return err
	}
	return validateHealth(target, health)
}

func stopFailedProxy(process Process, exited <-chan error) error {
	killErr := process.Kill()
	if errors.Is(killErr, os.ErrProcessDone) {
		killErr = nil
	}
	timer := time.NewTimer(proxyCleanupTimeout)
	defer timer.Stop()
	select {
	case <-exited:
		if killErr != nil {
			return fmt.Errorf("kill failed proxy: %w", killErr)
		}
		return nil
	case <-timer.C:
		if killErr != nil {
			return errors.Join(fmt.Errorf("kill failed proxy: %w", killErr), errors.New("wait for failed proxy timed out"))
		}
		return errors.New("wait for failed proxy timed out")
	}
}
