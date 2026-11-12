package codex

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/MoeForge/moego/base64"
)

// Office grants belong to a different OAuth client than Codex and SIWC.
const (
	BasispointsClientID     = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	BasispointsAuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	BasispointsTokenURL     = "https://auth.openai.com/oauth/token?unified=true"
	BasispointsRedirectURI  = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
	BasispointsScope        = "openid offline_access email profile organization.read"
)

// BasispointsTokenError exposes only a normalized error classification.
type BasispointsTokenError struct {
	Status                  int
	ReauthorizationRequired bool
}

func (e *BasispointsTokenError) Error() string {
	return fmt.Sprintf("Office token request failed (HTTP %d)", e.Status)
}

// BasispointsTokenData is persisted only in the account's Office partition.
type BasispointsTokenData struct {
	CodexSIWCTokenData
	AccountID string
	UserID    string
}

func (d *BasispointsTokenData) ToMap() map[string]any {
	if d == nil {
		return nil
	}
	out := d.CodexSIWCTokenData.ToMap()
	out["account_id"] = d.AccountID
	out["user_id"] = d.UserID
	return out
}

func (o *CodexAuth) GenerateBasispointsAuthURL(state string, pkce *PKCECodes) (string, error) {
	if strings.TrimSpace(state) == "" || pkce == nil || pkce.CodeChallenge == "" {
		return "", fmt.Errorf("Basispoints authorization requires state and PKCE")
	}
	params := url.Values{
		"client_id": {BasispointsClientID}, "response_type": {"code"},
		"redirect_uri": {BasispointsRedirectURI}, "scope": {BasispointsScope},
		"state": {state}, "code_challenge_method": {"S256"}, "code_challenge": {pkce.CodeChallenge},
		"audience": {"https://api.openai.com/v1"}, "platform": {"PC"},
	}
	return BasispointsAuthorizeURL + "?" + params.Encode(), nil
}

func (o *CodexAuth) ExchangeBasispointsCodeForTokens(ctx context.Context, code string, pkce *PKCECodes) (*BasispointsTokenData, error) {
	if strings.TrimSpace(code) == "" || pkce == nil || pkce.CodeVerifier == "" {
		return nil, fmt.Errorf("Basispoints authorization code and verifier are required")
	}
	return o.requestBasispointsTokens(ctx, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {BasispointsClientID},
		"code": {code}, "code_verifier": {pkce.CodeVerifier}, "redirect_uri": {BasispointsRedirectURI},
	}, nil)
}

func (o *CodexAuth) RefreshBasispointsTokens(ctx context.Context, current map[string]any) (*BasispointsTokenData, error) {
	refresh, _ := current["refresh_token"].(string)
	client, _ := current["client_id"].(string)
	if strings.TrimSpace(refresh) == "" || client != BasispointsClientID {
		return nil, fmt.Errorf("Basispoints Office authorization is required")
	}
	return o.requestBasispointsTokens(ctx, url.Values{
		"grant_type": {"refresh_token"}, "client_id": {BasispointsClientID}, "refresh_token": {refresh},
	}, current)
}

func (o *CodexAuth) requestBasispointsTokens(ctx context.Context, form url.Values, previous map[string]any) (*BasispointsTokenData, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, codexRefreshTimeout)
	defer cancel()
	req, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, BasispointsTokenURL, strings.NewReader(form.Encode()))
	if errRequest != nil {
		return nil, fmt.Errorf("create Office token request: %w", errRequest)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	client := *o.httpClient
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, errRequest := client.Do(req)
	if errRequest != nil {
		return nil, fmt.Errorf("Office token endpoint unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	raw, errRead := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if errRead != nil {
		return nil, fmt.Errorf("read Office token response")
	}
	if response.StatusCode != http.StatusOK {
		// Upstream bodies may contain credentials and must never enter logs.
		var failure struct {
			Error json.RawMessage `json:"error"`
		}
		var code string
		if json.Unmarshal(raw, &failure) == nil {
			if json.Unmarshal(failure.Error, &code) != nil {
				var detail struct {
					Code string `json:"code"`
				}
				if json.Unmarshal(failure.Error, &detail) == nil {
					code = detail.Code
				}
			}
		}
		terminal := response.StatusCode == http.StatusUnauthorized
		for _, known := range []string{"invalid_grant", "invalid_client", "refresh_token_reused", "refresh_token_expired", "refresh_token_invalidated", "token_revoked"} {
			terminal = terminal || code == known
		}
		return nil, &BasispointsTokenError{Status: response.StatusCode, ReauthorizationRequired: terminal}
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
		Scope        string `json:"scope"`
	}
	if json.Unmarshal(raw, &result) != nil {
		return nil, fmt.Errorf("invalid Office token response")
	}
	if previous != nil {
		if result.RefreshToken == "" {
			result.RefreshToken, _ = previous["refresh_token"].(string)
		}
		if result.IDToken == "" {
			result.IDToken, _ = previous["id_token"].(string)
		}
		if result.Scope == "" {
			result.Scope, _ = previous["scope"].(string)
		}
	}
	grant := map[string]any{"access_token": result.AccessToken, "refresh_token": result.RefreshToken, "id_token": result.IDToken, "client_id": BasispointsClientID}
	account, user, expiry, errValidate := ValidateBasispointsGrant(grant, time.Now(), false)
	if errValidate != nil {
		return nil, errValidate
	}
	if previous != nil {
		if oldAccount, _ := previous["account_id"].(string); oldAccount != "" && oldAccount != account {
			return nil, fmt.Errorf("Office refresh changed workspace")
		}
		if oldUser, _ := previous["user_id"].(string); oldUser != "" && oldUser != user {
			return nil, fmt.Errorf("Office refresh changed user")
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	return &BasispointsTokenData{CodexSIWCTokenData: CodexSIWCTokenData{
		AccessToken: result.AccessToken, RefreshToken: result.RefreshToken, IDToken: result.IDToken,
		ClientID: BasispointsClientID, Scope: result.Scope, Scopes: strings.Fields(result.Scope),
		Expire: expiry.UTC().Format(time.RFC3339), ExpiresAt: expiry.UTC().Format(time.RFC3339), LastRefresh: now,
	}, AccountID: account, UserID: user}, nil
}

// ValidateBasispointsGrant checks consistency, not JWT signatures. Fresh grants
// come from the fixed TLS token endpoint; imported grants still require upstream authorization.
func ValidateBasispointsGrant(grant map[string]any, now time.Time, allowExpired bool) (string, string, time.Time, error) {
	invalid := func() (string, string, time.Time, error) {
		return "", "", time.Time{}, fmt.Errorf("invalid or expired Basispoints Office grant; authorize Basispoints separately")
	}
	if grant["client_id"] != BasispointsClientID {
		return invalid()
	}
	access, _ := grant["access_token"].(string)
	refresh, _ := grant["refresh_token"].(string)
	claims, errClaims := basispointsClaims(access)
	if errClaims != nil || refresh == "" {
		return invalid()
	}
	client, _ := claims["client_id"].(string)
	if client == "" {
		client, _ = claims["azp"].(string)
	}
	if client != BasispointsClientID {
		return invalid()
	}
	auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
	account, _ := auth["chatgpt_account_id"].(string)
	user, _ := auth["chatgpt_user_id"].(string)
	if user == "" {
		user, _ = auth["user_id"].(string)
	}
	exp, _ := claims["exp"].(float64)
	expiry := time.Unix(int64(exp), 0)
	if account == "" || exp <= 0 || (!allowExpired && !expiry.After(now)) {
		return invalid()
	}
	if saved, _ := grant["account_id"].(string); saved != "" && saved != account {
		return invalid()
	}
	id, _ := grant["id_token"].(string)
	if id == "" {
		return invalid()
	}
	{
		identity, errIdentity := basispointsClaims(id)
		if errIdentity != nil {
			return invalid()
		}
		audienceOK := identity["aud"] == BasispointsClientID
		if audiences, ok := identity["aud"].([]any); ok {
			for _, audience := range audiences {
				audienceOK = audienceOK || audience == BasispointsClientID
			}
		}
		if !audienceOK {
			return invalid()
		}
		if user == "" {
			identityAuth, _ := identity["https://api.openai.com/auth"].(map[string]any)
			user, _ = identityAuth["chatgpt_user_id"].(string)
			if user == "" {
				user, _ = identityAuth["user_id"].(string)
			}
		}
	}
	return account, user, expiry, nil
}

func basispointsClaims(token string) (map[string]any, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid Office token")
	}
	raw, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode != nil {
		return nil, fmt.Errorf("invalid Office token")
	}
	var claims map[string]any
	if json.Unmarshal(raw, &claims) != nil || claims == nil {
		return nil, fmt.Errorf("invalid Office token")
	}
	return claims, nil
}

// ValidateBasispointsIdentity prevents binding a different member of the same workspace.
func ValidateBasispointsIdentity(grant, outer map[string]any) error {
	account, user, _, err := ValidateBasispointsGrant(grant, time.Now(), true)
	if err != nil {
		return err
	}
	if expected, _ := outer["account_id"].(string); expected != "" && account != expected {
		return fmt.Errorf("Office workspace does not match target")
	}
	for _, key := range []string{"access_token", "id_token"} {
		token, _ := outer[key].(string)
		claims, errClaims := basispointsClaims(token)
		if errClaims != nil {
			continue
		}
		auth, _ := claims["https://api.openai.com/auth"].(map[string]any)
		expected, _ := auth["chatgpt_user_id"].(string)
		if expected == "" {
			expected, _ = auth["user_id"].(string)
		}
		if expected != "" && (user == "" || user != expected) {
			return fmt.Errorf("Office user does not match target")
		}
	}
	return nil
}
