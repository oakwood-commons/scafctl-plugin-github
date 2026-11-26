// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/go-logr/logr"
	"github.com/oakwood-commons/httpc"
	sdkplugin "github.com/oakwood-commons/scafctl-plugin-sdk/plugin"
)

type requestAuthOverrideKey struct{}

// requestAuthOverride carries per-execution credential overrides on request contexts
// so a single pooled httpc.Client remains safe for concurrent steps using distinct identities.
type requestAuthOverride struct {
	token       string
	authProfile string
}

// withRequestAuthOverride validates optional per-step token and auth_profile inputs
// and attaches any override to ctx. Both fields fail closed when empty, whitespace-only,
// non-string, or supplied together.
func withRequestAuthOverride(ctx context.Context, inputs map[string]any) (context.Context, error) {
	rawToken, hasToken := inputs["token"]
	rawProfile, hasProfile := inputs["auth_profile"]

	if !hasToken && !hasProfile {
		return ctx, nil
	}
	if hasToken && hasProfile {
		return nil, fmt.Errorf("'token' and 'auth_profile' are mutually exclusive; specify at most one")
	}

	if hasToken {
		tok, ok := rawToken.(string)
		if !ok {
			return nil, fmt.Errorf("invalid 'token': must be a non-empty string")
		}
		tok = strings.TrimSpace(tok)
		if tok == "" {
			return nil, fmt.Errorf("'token' must not be empty or whitespace")
		}
		if strings.ContainsAny(tok, "\r\n") {
			return nil, fmt.Errorf("invalid 'token': must not contain newline characters")
		}
		return context.WithValue(ctx, requestAuthOverrideKey{}, requestAuthOverride{token: tok}), nil
	}

	prof, ok := rawProfile.(string)
	if !ok {
		return nil, fmt.Errorf("invalid 'auth_profile': must be a non-empty string")
	}
	prof = strings.TrimSpace(prof)
	if prof == "" {
		return nil, fmt.Errorf("'auth_profile' must not be empty or whitespace")
	}
	if strings.ContainsAny(prof, "\r\n") {
		return nil, fmt.Errorf("invalid 'auth_profile': must not contain newline characters")
	}
	return context.WithValue(ctx, requestAuthOverrideKey{}, requestAuthOverride{authProfile: prof}), nil
}

func authOverrideFromContext(ctx context.Context) requestAuthOverride {
	ov, _ := ctx.Value(requestAuthOverrideKey{}).(requestAuthOverride)
	return ov
}

// injectRequestAuth sets the Authorization header on outbound GitHub API requests.
func injectRequestAuth(req *http.Request) error {
	reqCtx := req.Context()
	ov := authOverrideFromContext(reqCtx)

	if ov.token != "" {
		req.Header.Set("Authorization", "Bearer "+ov.token)
		return nil
	}

	hostClient := sdkplugin.HostClientFromContext(reqCtx)
	if ov.authProfile != "" {
		if hostClient == nil {
			return fmt.Errorf("host service client not available for auth_profile %q", ov.authProfile)
		}
		resp, err := hostClient.GetAuthTokenWithOptions(reqCtx, sdkplugin.AuthTokenRequest{
			Handler: "github",
			Profile: ov.authProfile,
		})
		if err != nil {
			return fmt.Errorf("getting github token for auth_profile %q: %w", ov.authProfile, err)
		}
		if resp == nil || strings.TrimSpace(resp.AccessToken) == "" {
			return fmt.Errorf("empty github token returned for auth_profile %q", ov.authProfile)
		}
		req.Header.Set("Authorization", "Bearer "+resp.AccessToken)
		return nil
	}

	if hostClient == nil {
		lgr := logr.FromContextOrDiscard(reqCtx)
		lgr.V(1).Info("host service client not available, making unauthenticated request")
		return nil
	}
	resp, err := hostClient.GetAuthTokenWithOptions(reqCtx, sdkplugin.AuthTokenRequest{
		Handler: "github",
	})
	if err != nil {
		lgr := logr.FromContextOrDiscard(reqCtx)
		lgr.V(1).Info("GitHub auth token unavailable, making unauthenticated request", "error", err)
		return nil
	}
	if resp != nil && resp.AccessToken != "" {
		req.Header.Set("Authorization", "Bearer "+resp.AccessToken)
	}
	return nil
}

// refreshRequestAuth handles HTTP 401 token refresh callbacks for httpc.Client.
func refreshRequestAuth(reqCtx context.Context) (string, error) {
	ov := authOverrideFromContext(reqCtx)
	if ov.token != "" {
		// Explicit per-step token must never fall back to or refresh ambient host credentials on 401.
		return "", nil
	}

	hostClient := sdkplugin.HostClientFromContext(reqCtx)
	if hostClient == nil {
		if ov.authProfile != "" {
			return "", fmt.Errorf("host service client not available for token refresh (auth_profile %q)", ov.authProfile)
		}
		return "", fmt.Errorf("host service client not available for token refresh")
	}

	reqOpts := sdkplugin.AuthTokenRequest{
		Handler:      "github",
		ForceRefresh: true,
	}
	if ov.authProfile != "" {
		reqOpts.Profile = ov.authProfile
	}

	resp, err := hostClient.GetAuthTokenWithOptions(reqCtx, reqOpts)
	if err != nil {
		if ov.authProfile != "" {
			return "", fmt.Errorf("refreshing github token for auth_profile %q: %w", ov.authProfile, err)
		}
		return "", fmt.Errorf("refreshing github token: %w", err)
	}
	if resp == nil || strings.TrimSpace(resp.AccessToken) == "" {
		return "", fmt.Errorf("empty refreshed github token")
	}
	return "Bearer " + resp.AccessToken, nil
}

// installCrossOriginRedirectStripper wraps client's underlying CheckRedirect hook so
// GitHub-specific credentials and headers are removed whenever a redirect crosses
// scheme or host:port origins, while preserving httpc's SSRF IPPolicy and maxRedirects checks.
func installCrossOriginRedirectStripper(client *httpc.Client) {
	if client == nil {
		return
	}
	rc := client.RetryableClient()
	if rc == nil || rc.HTTPClient == nil {
		return
	}
	orig := rc.HTTPClient.CheckRedirect
	rc.HTTPClient.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if orig != nil {
			if err := orig(req, via); err != nil {
				return err
			}
		}
		if len(via) > 0 && hasCrossOriginRedirect(via, req.URL) {
			req.Header.Del("Authorization")
			req.Header.Del("X-GitHub-Api-Version")
		}
		return nil
	}
}

func hasCrossOriginRedirect(via []*http.Request, target *url.URL) bool {
	if len(via) == 0 || via[0] == nil || via[0].URL == nil || target == nil {
		return false
	}
	origin := via[0].URL
	if !sameSchemeAndHost(origin, target) {
		return true
	}
	for _, hop := range via[1:] {
		if hop != nil && hop.URL != nil && !sameSchemeAndHost(origin, hop.URL) {
			return true
		}
	}
	return false
}

func sameSchemeAndHost(a, b *url.URL) bool {
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

// quotedLocationOrURLPattern matches %q-quoted URL/Location tokens emitted by net/http
// and net/url (including relative Location headers and URLs containing spaces or quotes).
var quotedLocationOrURLPattern = regexp.MustCompile(`((?:Location header |parse |Get |Post |Put |Patch |Delete |Head |Options )?)"((?:\\.|[^"\\])*)"`)

// unquotedHTTPURLPattern matches unquoted HTTP(S) URLs (case-insensitive scheme,
// allowing RFC 3986 single quotes) such as retryablehttp wrapper prefixes.
var unquotedHTTPURLPattern = regexp.MustCompile(`(?i)https?://[^\s"]+`)

type redactedTransportError struct {
	msg   string
	cause error
}

func (e *redactedTransportError) Error() string {
	return e.msg
}

func (e *redactedTransportError) Unwrap() error {
	return e.cause
}

func looksLikeURLOrLocationToken(prefix, inner string) bool {
	if prefix != "" {
		return true
	}
	lower := strings.ToLower(inner)
	return strings.HasPrefix(lower, "http://") ||
		strings.HasPrefix(lower, "https://") ||
		strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "/")
}

func redactURLBody(s string) (string, bool) {
	qIdx := strings.IndexByte(s, '?')
	fIdx := strings.IndexByte(s, '#')
	if qIdx < 0 && fIdx < 0 {
		return s, false
	}
	if qIdx >= 0 && (fIdx < 0 || qIdx < fIdx) {
		return s[:qIdx] + "?REDACTED", true
	}
	return s[:fIdx] + "#REDACTED", true
}

// redactURLErrorQuery scrubs query parameters and fragments from any URLs or
// redirect Location values appearing in err's formatted message without exposing
// the unredacted cause via Error(), while preserving errors.Is/As chains.
func redactURLErrorQuery(err error) error {
	if err == nil {
		return nil
	}
	rawMsg := err.Error()
	redacted := false

	stage1 := quotedLocationOrURLPattern.ReplaceAllStringFunc(rawMsg, func(match string) string {
		qStart := strings.IndexByte(match, '"')
		if qStart < 0 || match[len(match)-1] != '"' {
			return match
		}
		prefix := match[:qStart]
		inner := match[qStart+1 : len(match)-1]
		if !looksLikeURLOrLocationToken(prefix, inner) {
			return match
		}
		cleaned, changed := redactURLBody(inner)
		if !changed {
			return match
		}
		redacted = true
		return prefix + `"` + cleaned + `"`
	})

	sanitized := unquotedHTTPURLPattern.ReplaceAllStringFunc(stage1, func(u string) string {
		suffix := ""
		trimmed := strings.TrimRight(u, ":;,.)]")
		if len(trimmed) < len(u) {
			suffix = u[len(trimmed):]
			u = trimmed
		}
		// Skip already-redacted suffix tokens.
		if strings.HasSuffix(u, "?REDACTED") || strings.HasSuffix(u, "#REDACTED") {
			return u + suffix
		}
		cleaned, changed := redactURLBody(u)
		if !changed {
			return u + suffix
		}
		redacted = true
		return cleaned + suffix
	})

	if !redacted {
		return err
	}
	return &redactedTransportError{
		msg:   sanitized,
		cause: err,
	}
}
