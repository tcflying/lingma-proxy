package qodercli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	// defaultClientID is the OAuth client id both Qoder desktop builds register
	// with their gateway; the global site uses the same value as the CN one.
	defaultClientID = "732aef47-9cf2-46a2-95fe-4cebb5d0d1fa"
	// renewMargin is the refreshed-at safety margin.
	renewMargin = 10 * time.Minute
	// defaultJobTokenLifetime is the lifetime assumed when a job token response
	// carries no usable expiry. It equals renewMargin on purpose: such a token is
	// reminted on nearly every use, which is the cheap side of the guess, while a
	// longer guess would serve a token the gateway has already dropped.
	defaultJobTokenLifetime = renewMargin
	// jobTokenEnv names the operator-supplied job credential that stands in for a
	// desktop login, which only Windows can read.
	jobTokenEnv = "LINGMA_QODERCLI_JOB_TOKEN"
)

// JobCredential is what the bundled CLI expects in its <SITE>_JOB_TOKEN
// environment variable: the raw job token response from the OpenAPI service.
type JobCredential struct {
	Raw       json.RawMessage `json:"-"`
	Token     string          `json:"token"`
	ExpiresAt time.Time       `json:"expires_at"`
}

func (c JobCredential) expiresIn(margin time.Duration) bool {
	if c.ExpiresAt.IsZero() {
		return false
	}
	return time.Now().Add(margin).After(c.ExpiresAt)
}

// TokenSource turns the desktop app's device login into short-lived job tokens
// that the bundled CLI accepts, refreshing them as they approach expiry.
type TokenSource struct {
	profileDir string
	label      string
	baseURL    string
	clientID   string
	http       *http.Client

	mu           sync.Mutex
	job          JobCredential
	device       appCredential
	deviceLoaded bool
}

func NewTokenSource(profileDir string, site Site) *TokenSource {
	base := strings.TrimSpace(os.Getenv("LINGMA_QODER_OPENAPI_BASE_URL"))
	if base == "" {
		base = site.profile().openAPIBase
	}
	client := strings.TrimSpace(os.Getenv("LINGMA_QODER_CLIENT_ID"))
	if client == "" {
		client = defaultClientID
	}
	return &TokenSource{
		profileDir: profileDir,
		label:      site.Label(),
		baseURL:    strings.TrimRight(base, "/"),
		clientID:   client,
		http:       &http.Client{Timeout: 30 * time.Second},
	}
}

// jobTokenFromEnv returns the operator-supplied job credential, which replaces the
// whole desktop-login chain when set.
func jobTokenFromEnv() string {
	return strings.TrimSpace(os.Getenv(jobTokenEnv))
}

// JobToken returns a usable job credential, minting or refreshing as needed.
func (t *TokenSource) JobToken(ctx context.Context) (JobCredential, error) {
	if explicit := jobTokenFromEnv(); explicit != "" {
		var cred JobCredential
		if err := json.Unmarshal([]byte(explicit), &cred); err != nil {
			return JobCredential{}, fmt.Errorf("parse LINGMA_QODERCLI_JOB_TOKEN: %w", err)
		}
		if cred.Token == "" {
			return JobCredential{}, errors.New("LINGMA_QODERCLI_JOB_TOKEN has no token field")
		}
		cred.Raw = json.RawMessage(explicit)
		return cred, nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.job.Token != "" && !t.job.expiresIn(renewMargin) {
		return t.job, nil
	}

	if t.job.Token != "" {
		if refreshed, err := t.refreshJobToken(ctx); err == nil {
			t.job = refreshed
			return t.job, nil
		}
	}

	cred, err := t.mintFromLogin(ctx)
	if err != nil {
		return JobCredential{}, err
	}
	t.job = cred
	return t.job, nil
}

func (t *TokenSource) mintFromLogin(ctx context.Context) (JobCredential, error) {
	device, err := t.deviceCredential(ctx)
	if err != nil {
		return JobCredential{}, err
	}
	body, _ := json.Marshal(map[string]string{"clientId": t.clientID})
	payload, err := t.post(ctx, "/api/v1/me/jobToken", body, device.Token)
	if err != nil {
		if !isAuthRejected(err) {
			return JobCredential{}, err
		}
		// The stored device token expired; the app would have refreshed it.
		if device.RefreshToken == "" {
			return JobCredential{}, err
		}
		if _, refreshErr := t.refreshDeviceToken(ctx, device.RefreshToken); refreshErr != nil {
			return JobCredential{}, fmt.Errorf("%w (and refreshing the app login failed: %v)", err, refreshErr)
		}
		fresh, loadErr := t.deviceCredential(ctx)
		if loadErr != nil {
			return JobCredential{}, loadErr
		}
		payload, err = t.post(ctx, "/api/v1/me/jobToken", body, fresh.Token)
		if err != nil {
			return JobCredential{}, err
		}
	}
	return decodeJobCredential(payload)
}

func (t *TokenSource) refreshJobToken(ctx context.Context) (JobCredential, error) {
	var stored struct {
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.Unmarshal(t.job.Raw, &stored); err != nil || stored.RefreshToken == "" {
		return JobCredential{}, errors.New("job credential has no refresh token")
	}
	body, _ := json.Marshal(map[string]string{"refresh_token": stored.RefreshToken})
	payload, err := t.post(ctx, "/api/v1/jobToken/refresh", body, "")
	if err != nil {
		return JobCredential{}, err
	}
	return decodeJobCredential(payload)
}

func (t *TokenSource) refreshDeviceToken(ctx context.Context, refreshToken string) (appCredential, error) {
	body, _ := json.Marshal(map[string]string{"refresh_token": refreshToken})
	payload, err := t.post(ctx, "/api/v1/deviceToken/refresh", body, "")
	if err != nil {
		return appCredential{}, err
	}
	var cred appCredential
	if err := json.Unmarshal(payload, &cred); err != nil {
		return appCredential{}, fmt.Errorf("parse device token refresh: %w", err)
	}
	if !cred.valid() {
		return appCredential{}, errors.New("device token refresh returned no token")
	}
	t.device = cred
	t.deviceLoaded = true
	return cred, nil
}

// deviceCredential reads the decrypted login state, preferring a refresh when
// the stored device token is already past its expiry.
func (t *TokenSource) deviceCredential(ctx context.Context) (appCredential, error) {
	if t.deviceLoaded {
		return t.device, nil
	}
	if t.profileDir == "" {
		return appCredential{}, fmt.Errorf("%s app profile directory was not found", t.label)
	}
	cred, err := loadAppCredential(t.profileDir)
	if err != nil {
		return appCredential{}, err
	}
	if !cred.ExpiresAt.IsZero() && time.Now().Add(renewMargin).After(cred.ExpiresAt) && cred.RefreshToken != "" {
		if refreshed, refreshErr := t.refreshDeviceToken(ctx, cred.RefreshToken); refreshErr == nil {
			return refreshed, nil
		}
	}
	t.device = cred
	t.deviceLoaded = true
	return cred, nil
}

func (t *TokenSource) post(ctx context.Context, path string, body []byte, bearer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.baseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := t.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s%s: %w", t.baseURL, path, err)
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &httpError{status: resp.StatusCode, detail: strings.TrimSpace(string(payload))}
	}
	return payload, nil
}

type httpError struct {
	status int
	detail string
}

func (e *httpError) Error() string {
	if e.detail == "" {
		return fmt.Sprintf("HTTP %d", e.status)
	}
	return fmt.Sprintf("HTTP %d: %s", e.status, e.detail)
}

func isAuthRejected(err error) bool {
	var httpErr *httpError
	return errors.As(err, &httpErr) && (httpErr.status == http.StatusUnauthorized || httpErr.status == http.StatusForbidden)
}

func decodeJobCredential(payload []byte) (JobCredential, error) {
	var decoded struct {
		Token      string `json:"token"`
		ExpiresAt  string `json:"expires_at"`
		ExpiresIn  int64  `json:"expires_in"`
		CreateTime string `json:"created_at"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		return JobCredential{}, fmt.Errorf("parse job token response: %w", err)
	}
	if decoded.Token == "" {
		return JobCredential{}, fmt.Errorf("job token response has no token: %s", strings.TrimSpace(string(payload)))
	}
	cred := JobCredential{Token: decoded.Token, Raw: json.RawMessage(payload)}
	switch {
	case decoded.ExpiresAt != "":
		if parsed, err := time.Parse(time.RFC3339, decoded.ExpiresAt); err == nil {
			cred.ExpiresAt = parsed
		}
	case decoded.ExpiresIn > 0:
		cred.ExpiresAt = time.Now().Add(time.Duration(decoded.ExpiresIn) * time.Millisecond)
	}
	// An unreadable expires_at lands in the first case without setting anything, so
	// both "no expiry field" and "expiry in a shape we do not parse" have to fall
	// back here: a zero ExpiresAt reads as immortal to expiresIn, which kept a
	// minted token in service until the gateway dropped it and then failed every
	// request until the proxy restarted.
	if cred.ExpiresAt.IsZero() {
		cred.ExpiresAt = time.Now().Add(defaultJobTokenLifetime)
	}
	return cred, nil
}
