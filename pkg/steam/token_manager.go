package steam

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

type cachedToken struct {
	token     string
	expiresAt time.Time
}

// TokenManager manages CDN authentication tokens per (app_id, depot_id, host).
type TokenManager struct {
	client    *Client
	anonymous bool
	mu        sync.RWMutex
	tokens    map[string]*cachedToken
}

func NewTokenManager(client *Client, anonymous bool) *TokenManager {
	return &TokenManager{
		client:    client,
		anonymous: anonymous,
		tokens:    make(map[string]*cachedToken),
	}
}

func (tm *TokenManager) cacheKey(appID, depotID uint32, host string) string {
	return fmt.Sprintf("%d_%d_%s", appID, depotID, host)
}

// GetToken returns a valid CDN token for (appID, depotID, host).
// If anonymous is false, returns empty string.
func (tm *TokenManager) GetToken(appID, depotID uint32, host string) (string, error) {
	if !tm.anonymous || tm.client == nil {
		return "", nil
	}

	key := tm.cacheKey(appID, depotID, host)

	tm.mu.RLock()
	cached, ok := tm.tokens[key]
	if ok && time.Now().Add(2*time.Minute).Before(cached.expiresAt) {
		tok := cached.token
		tm.mu.RUnlock()
		return tok, nil
	}
	tm.mu.RUnlock()

	// Fetch new token
	return tm.RefreshToken(appID, depotID, host)
}

// RefreshToken forces a request to Steam to get an updated CDN Auth Token.
func (tm *TokenManager) RefreshToken(appID, depotID uint32, host string) (string, error) {
	if !tm.anonymous || tm.client == nil {
		return "", nil
	}

	key := tm.cacheKey(appID, depotID, host)
	tm.mu.Lock()
	defer tm.mu.Unlock()

	// Double check
	if cached, ok := tm.tokens[key]; ok && time.Now().Add(1*time.Minute).Before(cached.expiresAt) {
		return cached.token, nil
	}

	req := &CDNAuthTokenRequest{
		DepotID:  depotID,
		HostName: host,
		AppID:    appID,
	}

	respBytes, err := tm.client.CallServiceMethod("ContentServerDirectory.GetCDNAuthToken#1", req.Marshal(), 10*time.Second)
	if err != nil {
		return "", fmt.Errorf("failed to get CDN auth token for depot %d on %s: %w", depotID, host, err)
	}

	resp, err := unmarshalCDNAuthTokenResponse(respBytes)
	if err != nil {
		return "", fmt.Errorf("failed to parse CDN auth token response: %w", err)
	}

	token := resp.Token
	if token != "" && !strings.HasPrefix(token, "?") {
		token = "?" + token
	}
	var expiresAt time.Time
	if resp.ExpirationTime > 0 {
		expiresAt = time.Unix(int64(resp.ExpirationTime), 0)
	} else {
		// Default to 1 hour if not specified
		expiresAt = time.Now().Add(1 * time.Hour)
	}

	tm.tokens[key] = &cachedToken{
		token:     token,
		expiresAt: expiresAt,
	}

	log.Printf("[Token] Successfully obtained CDN token for depot %d on %s (expires: %s)",
		depotID, host, expiresAt.Format("15:04:05"))

	return token, nil
}

// InvalidateToken invalidates the cached token for (appID, depotID, host).
// Called when CDN returns 401/403 Forbidden.
func (tm *TokenManager) InvalidateToken(appID, depotID uint32, host string) {
	key := tm.cacheKey(appID, depotID, host)
	tm.mu.Lock()
	delete(tm.tokens, key)
	tm.mu.Unlock()
	log.Printf("[Token] Invalidated cached token for depot %d on %s due to auth failure", depotID, host)
}
