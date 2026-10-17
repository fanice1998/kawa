package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/fanice1998/kawa/internal/config"
	"github.com/rs/zerolog/log"
)

// OAuthTokenManager manages OAuth2 access & refresh tokens and performs automatic background/on-demand refresh.
type OAuthTokenManager struct {
	mu           sync.Mutex
	providerName string
	accessToken  string
	refreshToken string
	tokenURL     string
	clientID     string
	clientSecret string
	expiresAt    time.Time
	httpClient   *http.Client
}

type tokenEndpointResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func NewOAuthTokenManager(providerName string, cfg config.OAuthModelConfig) *OAuthTokenManager {
	var exp time.Time
	if strings.TrimSpace(cfg.ExpiresAt) != "" {
		if parsed, err := time.Parse(time.RFC3339, strings.TrimSpace(cfg.ExpiresAt)); err == nil {
			exp = parsed
		}
	}
	timeout := time.Duration(cfg.TimeoutSec) * time.Second
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	return &OAuthTokenManager{
		providerName: providerName,
		accessToken:  strings.TrimSpace(cfg.AccessToken),
		refreshToken: strings.TrimSpace(cfg.RefreshToken),
		tokenURL:     strings.TrimSpace(cfg.TokenURL),
		clientID:     strings.TrimSpace(cfg.ClientID),
		clientSecret: strings.TrimSpace(cfg.ClientSecret),
		expiresAt:    exp,
		httpClient:   &http.Client{Timeout: timeout},
	}
}

// GetToken returns a valid access token, automatically refreshing if expired or expiring within 60 seconds.
func (m *OAuthTokenManager) GetToken(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.accessToken != "" && (m.expiresAt.IsZero() || time.Until(m.expiresAt) > 60*time.Second) {
		return m.accessToken, nil
	}
	if m.refreshToken == "" {
		if m.accessToken != "" {
			return m.accessToken, nil
		}
		return "", fmt.Errorf("%s: missing OAuth access_token and refresh_token", m.providerName)
	}
	return m.refreshLocked(ctx)
}

// ForceRefresh forces a token refresh using the configured refresh_token (e.g. after receiving HTTP 401).
func (m *OAuthTokenManager) ForceRefresh(ctx context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.refreshToken == "" {
		return "", fmt.Errorf("%s: cannot refresh OAuth token without refresh_token", m.providerName)
	}
	return m.refreshLocked(ctx)
}

func (m *OAuthTokenManager) refreshLocked(ctx context.Context) (string, error) {
	if m.tokenURL == "" {
		return "", fmt.Errorf("%s: token_url is empty, cannot refresh OAuth token", m.providerName)
	}

	form := url.Values{}
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", m.refreshToken)
	if m.clientID != "" {
		form.Set("client_id", m.clientID)
	}
	if m.clientSecret != "" {
		form.Set("client_secret", m.clientSecret)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", fmt.Errorf("%s: create refresh request: %w", m.providerName, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := m.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%s: execute OAuth refresh: %w", m.providerName, err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 65536))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("%s: OAuth refresh failed with HTTP %d: %s", m.providerName, resp.StatusCode, string(body))
	}

	var tokResp tokenEndpointResponse
	if err := json.Unmarshal(body, &tokResp); err != nil {
		return "", fmt.Errorf("%s: decode OAuth token response: %w", m.providerName, err)
	}
	if tokResp.AccessToken == "" {
		return "", fmt.Errorf("%s: OAuth refresh response missing access_token", m.providerName)
	}

	m.accessToken = tokResp.AccessToken
	if tokResp.RefreshToken != "" {
		m.refreshToken = tokResp.RefreshToken
	}
	if tokResp.ExpiresIn > 0 {
		m.expiresAt = time.Now().Add(time.Duration(tokResp.ExpiresIn) * time.Second)
	} else {
		m.expiresAt = time.Now().Add(55 * time.Minute)
	}

	log.Info().
		Str("provider", m.providerName).
		Time("expires_at", m.expiresAt).
		Msg("OAuth access token refreshed successfully")

	return m.accessToken, nil
}
