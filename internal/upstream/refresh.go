package upstream

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"workbuddy2api/internal/auth"
)

// RefreshTokenContext coalesces overlapping refreshes of the same credential,
// including separate Auth objects loaded from the same file. The key contains
// only a digest; authority, region and token differences never share a flight.
func (c *Client) RefreshTokenContext(ctx context.Context, a *auth.Auth) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if enabled, err := a.RefreshShared(ctx, func(current auth.Credentials) (auth.Credentials, error) {
		if strings.TrimSpace(current.RefreshToken) == "" {
			return current, fmt.Errorf("no refreshToken")
		}
		return c.refreshCredential(ctx, authFromCredentials(current), current)
	}); enabled {
		return err
	}
	before := a.Snapshot()
	if strings.TrimSpace(before.RefreshToken) == "" {
		return fmt.Errorf("no refreshToken")
	}
	keyData, _ := json.Marshal([]any{before.UID, before.EnterpriseID, before.Domain, before.AccessToken, before.RefreshToken, before.Authority, c.chatBase(a), c.RefreshIdentityCheck})
	key := sha256.Sum256(keyData)
	ch := c.refreshes.DoChan(hex.EncodeToString(key[:]), func() (any, error) {
		// Callers such as collectionservice may already hold the original
		// account's maintenance lock while refreshing a copy. Do not acquire
		// that lock inside a shared flight: it would invert the wait order.
		// Coalescing and the final credential CAS provide serialization here.
		current := a.Snapshot()
		if !sameRefreshCredential(current, before) {
			if current.UID != before.UID || current.EnterpriseID != before.EnterpriseID || current.Domain != before.Domain || current.Authority != before.Authority {
				return nil, fmt.Errorf("refresh discarded: account changed")
			}
			return current, nil
		}
		next, err := c.refreshCredential(ctx, a, before)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !a.ApplyRefreshIfCurrent(before, next) {
			return nil, fmt.Errorf("refresh discarded: credentials changed")
		}
		return next, nil
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case result := <-ch:
		if result.Err != nil {
			return result.Err
		}
		next := result.Val.(auth.Credentials)
		if !a.ApplyRefreshIfCurrent(before, next) && !sameRefreshCredential(a.Snapshot(), next) {
			return fmt.Errorf("refresh discarded: credentials changed")
		}
		return nil
	}
}

// Persisting a device record or changing a nickname does not rotate tokens.
// Only a changed credential/authority makes an in-flight refresh obsolete.
func sameRefreshCredential(a, b auth.Credentials) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.ExpiresAt == b.ExpiresAt &&
		a.Domain == b.Domain && a.UID == b.UID && a.EnterpriseID == b.EnterpriseID && a.Authority == b.Authority
}

func authFromCredentials(s auth.Credentials) *auth.Auth {
	return &auth.Auth{AccessToken: s.AccessToken, RefreshToken: s.RefreshToken, ExpiresAt: s.ExpiresAt,
		Domain: s.Domain, UID: s.UID, EnterpriseID: s.EnterpriseID, Nickname: s.Nickname,
		FilePath: s.FilePath, DeviceID: s.DeviceID, DeviceProfile: s.DeviceProfile, Authority: s.Authority}
}

func (c *Client) refreshCredential(ctx context.Context, a *auth.Auth, before auth.Credentials) (auth.Credentials, error) {
	next := before
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.chatBase(a)+"/v2/plugin/auth/token/refresh", bytes.NewBufferString("{}"))
	if err != nil {
		return next, err
	}
	c.refreshHeaders(req, authFromCredentials(before))
	data, err := c.doJSON(req, a)
	if err != nil {
		return next, &refreshStageError{stage: "token request", cause: err}
	}
	var tok struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
		ExpiresIn    int64  `json:"expiresIn"`
		Domain       string `json:"domain"`
		TokenType    string `json:"tokenType"`
	}
	if json.Unmarshal(data, &tok) != nil || tok.AccessToken == "" {
		return next, fmt.Errorf("refresh_failed: no accessToken in response — re-login required")
	}
	next.AccessToken = tok.AccessToken
	if tok.RefreshToken != "" {
		next.RefreshToken = tok.RefreshToken
	}
	if tok.Domain != "" {
		next.Domain = tok.Domain
	}
	if tok.TokenType != "" {
		next.TokenType = tok.TokenType
	}
	if tok.ExpiresIn > 0 && tok.ExpiresIn <= int64((365*24*time.Hour)/time.Second) {
		next.ExpiresAt = time.Now().Unix() + tok.ExpiresIn
	}
	// Reuse the credential parser's header validation without exposing values.
	raw, _ := json.Marshal(map[string]any{"accessToken": next.AccessToken, "refreshToken": next.RefreshToken, "domain": next.Domain, "uid": next.UID, "enterpriseId": next.EnterpriseID})
	if _, err := auth.Parse(raw); err != nil {
		return next, fmt.Errorf("refresh_failed: invalid credential fields")
	}
	if c.RefreshIdentityCheck == "" || c.RefreshIdentityCheck == "off" {
		return next, nil
	}
	if before.Domain != "" && authFromCredentials(before).Region() != authFromCredentials(next).Region() {
		return next, fmt.Errorf("refresh identity mismatch: region")
	}
	if before.TokenType != "" && tok.TokenType != "" && before.TokenType != tok.TokenType {
		return next, fmt.Errorf("refresh identity mismatch: token type")
	}
	if before.UID == "" {
		if c.RefreshIdentityCheck == "strict" {
			return next, fmt.Errorf("refresh identity unavailable: missing baseline uid")
		}
		return next, nil
	}
	return c.verifyRefreshedIdentity(ctx, before, next)
}

type refreshAccount struct {
	UID                string  `json:"uid"`
	EnterpriseID       *string `json:"enterpriseId"`
	Type               string  `json:"type"`
	OneIDAccountID     string  `json:"oneidAccountId"`
	IDP                string  `json:"idp"`
	DepartmentFullName string  `json:"departmentFullName"`
}

func (c *Client) identityUnavailable(ctx context.Context, next auth.Credentials, reason string) (auth.Credentials, error) {
	if err := ctx.Err(); err != nil {
		return next, err
	}
	if c.RefreshIdentityCheck == "strict" {
		return next, fmt.Errorf("refresh identity unavailable: %s", reason)
	}
	// Never print the upstream response, account identity or tokens here.
	log.Printf("refresh identity check skipped: %s", reason)
	return next, nil
}

func (c *Client) verifyRefreshedIdentity(ctx context.Context, before, next auth.Credentials) (auth.Credentials, error) {
	staged := authFromCredentials(next)
	path := "/v2/plugin/account"
	if staged.Region() == auth.RegionGlobal {
		path = "/v2/plugin/accounts"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.chatBase(staged)+path, nil)
	if err != nil {
		return next, err
	}
	c.commonHeaders(req, staged)
	if !c.desktopHeaders() {
		accountHeaders(req, staged)
	}
	data, err := c.doJSON(req, staged)
	if err != nil {
		var upstreamErr *Error
		if errors.As(err, &upstreamErr) && (upstreamErr.Status == 401 || upstreamErr.Status == 403) {
			return next, &Error{Kind: upstreamErr.Kind, Code: upstreamErr.Code, Status: upstreamErr.Status, Msg: "refreshed token identity rejected"}
		}
		return c.identityUnavailable(ctx, next, "endpoint unavailable")
	}
	var accounts []refreshAccount
	if path == "/v2/plugin/accounts" {
		var list struct {
			Accounts []refreshAccount `json:"accounts"`
		}
		if json.Unmarshal(data, &list) != nil {
			return c.identityUnavailable(ctx, next, "unsupported response")
		}
		accounts = list.Accounts
	} else {
		var account refreshAccount
		if json.Unmarshal(data, &account) != nil {
			return c.identityUnavailable(ctx, next, "unsupported response")
		}
		accounts = []refreshAccount{account}
	}
	var candidates []refreshAccount
	known := false
	incomplete := false
	for _, account := range accounts {
		if account.UID == "" {
			continue
		}
		known = true
		if account.UID == before.UID && account.EnterpriseID == nil && before.EnterpriseID != "" {
			incomplete = true
			continue
		}
		enterprise := ""
		if account.EnterpriseID != nil {
			enterprise = *account.EnterpriseID
		}
		if account.UID == before.UID && enterprise == before.EnterpriseID &&
			(before.AccountType == "" || account.Type == "" || account.Type == before.AccountType) {
			candidates = append(candidates, account)
		}
	}
	if !known {
		return c.identityUnavailable(ctx, next, "missing account identity")
	}
	if len(candidates) == 0 {
		if incomplete {
			return c.identityUnavailable(ctx, next, "missing enterprise identity")
		}
		return next, fmt.Errorf("refresh identity mismatch: account")
	}
	if len(candidates) != 1 {
		return c.identityUnavailable(ctx, next, "ambiguous account identity")
	}
	account := candidates[0]
	for _, field := range []struct {
		before, returned string
		target           *string
	}{
		{before.AccountType, account.Type, &next.AccountType},
		{before.OneIDAccountID, account.OneIDAccountID, &next.OneIDAccountID},
		{before.IDP, account.IDP, &next.IDP},
	} {
		if field.before != "" && field.returned != "" && field.before != field.returned {
			return next, fmt.Errorf("refresh identity mismatch: authority")
		}
		if field.returned != "" {
			*field.target = field.returned
		}
	}
	if account.DepartmentFullName != "" {
		next.DepartmentFullName = account.DepartmentFullName
	}
	return next, nil
}

// Preserve errors.Is/As for the existing rotation policy, but keep responses,
// request URLs and credentials out of refresh diagnostics.
type refreshStageError struct {
	stage string
	cause error
}

func (e *refreshStageError) Error() string {
	var upstreamErr *Error
	if errors.As(e.cause, &upstreamErr) {
		return fmt.Sprintf("refresh %s failed: HTTP %d (%s)", e.stage, upstreamErr.Status, upstreamErr.Kind)
	}
	if errors.Is(e.cause, context.Canceled) {
		return "refresh " + e.stage + " canceled"
	}
	if errors.Is(e.cause, context.DeadlineExceeded) {
		return "refresh " + e.stage + " timed out"
	}
	if errors.Is(e.cause, ErrNoProxyNode) {
		return "refresh " + e.stage + " failed: proxy unavailable"
	}
	return "refresh " + e.stage + " failed"
}

func (e *refreshStageError) Unwrap() error { return e.cause }
