// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/oakwood-commons/httpc"
	sdkauth "github.com/oakwood-commons/scafctl-plugin-sdk/auth"
	sdkplugin "github.com/oakwood-commons/scafctl-plugin-sdk/plugin"
	"github.com/oakwood-commons/scafctl-plugin-sdk/plugin/proto"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

type mockAuthHostService struct {
	proto.UnimplementedHostServiceServer
	mu       sync.Mutex
	requests []*proto.GetAuthTokenRequest
	handler  func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error)
}

func (m *mockAuthHostService) GetAuthToken(_ context.Context, req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
	cloned := &proto.GetAuthTokenRequest{
		HandlerName:        req.HandlerName,
		Scope:              req.Scope,
		MinValidForSeconds: req.MinValidForSeconds,
		ForceRefresh:       req.ForceRefresh,
		Profile:            req.Profile,
		Hostname:           req.Hostname,
	}
	m.mu.Lock()
	m.requests = append(m.requests, cloned)
	fn := m.handler
	m.mu.Unlock()
	if fn != nil {
		return fn(cloned)
	}
	return &proto.GetAuthTokenResponse{AccessToken: "default-host-tok", TokenType: "Bearer"}, nil
}

func (m *mockAuthHostService) getRequests() []*proto.GetAuthTokenRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*proto.GetAuthTokenRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

func startTestAuthHostClient(t testing.TB, fn func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error)) (*mockAuthHostService, *sdkplugin.HostServiceClient) {
	t.Helper()
	lis, err := (&net.ListenConfig{}).Listen(context.Background(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	svc := &mockAuthHostService{handler: fn}
	srv := grpc.NewServer()
	proto.RegisterHostServiceServer(srv, svc)
	go func() { _ = srv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = conn.Close()
		srv.Stop()
	})
	return svc, sdkplugin.NewHostServiceClient(conn)
}

func TestDescriptor_AuthFieldsAndSensitivity(t *testing.T) {
	t.Parallel()

	p := newProvider()
	desc := p.Descriptor()
	assert.Contains(t, desc.SensitiveFields, "webhook_secret")
	assert.Contains(t, desc.SensitiveFields, "token")

	tokenSchema := desc.Schema.Properties["token"]
	require.NotNil(t, tokenSchema)
	assert.True(t, tokenSchema.WriteOnly, "token property must be marked WriteOnly")

	profileSchema := desc.Schema.Properties["auth_profile"]
	require.NotNil(t, profileSchema)
	assert.Equal(t, "string", profileSchema.Type)
}

func TestProvider_AuthOverride_FailClosedValidation(t *testing.T) {
	t.Parallel()

	p := newProvider()
	sentinelVal := "sentinel-echo-check"

	tests := []struct {
		name    string
		inputs  map[string]any
		wantErr string
	}{
		{
			name: "mutually exclusive token and auth_profile",
			inputs: map[string]any{
				"operation":    "get_repo",
				"owner":        "public-org",
				"repo":         "demo",
				"token":        sentinelVal,
				"auth_profile": "work-org",
			},
			wantErr: "mutually exclusive",
		},
		{
			name: "empty token fails closed",
			inputs: map[string]any{
				"operation": "get_repo",
				"owner":     "public-org",
				"repo":      "demo",
				"token":     "",
			},
			wantErr: "'token' must not be empty or whitespace",
		},
		{
			name: "whitespace token fails closed",
			inputs: map[string]any{
				"operation": "get_repo",
				"owner":     "public-org",
				"repo":      "demo",
				"token":     "   \t ",
			},
			wantErr: "'token' must not be empty or whitespace",
		},
		{
			name: "non-string token fails closed",
			inputs: map[string]any{
				"operation": "get_repo",
				"owner":     "public-org",
				"repo":      "demo",
				"token":     12345,
			},
			wantErr: "invalid 'token'",
		},
		{
			name: "newline in token fails closed",
			inputs: map[string]any{
				"operation": "get_repo",
				"owner":     "public-org",
				"repo":      "demo",
				"token":     "tok\ninjected: header",
			},
			wantErr: "newline",
		},
		{
			name: "empty auth_profile fails closed",
			inputs: map[string]any{
				"operation":    "get_repo",
				"owner":        "public-org",
				"repo":         "demo",
				"auth_profile": "",
			},
			wantErr: "'auth_profile' must not be empty or whitespace",
		},
		{
			name: "whitespace auth_profile fails closed",
			inputs: map[string]any{
				"operation":    "get_repo",
				"owner":        "public-org",
				"repo":         "demo",
				"auth_profile": "   ",
			},
			wantErr: "'auth_profile' must not be empty or whitespace",
		},
		{
			name: "non-string auth_profile fails closed",
			inputs: map[string]any{
				"operation":    "get_repo",
				"owner":        "public-org",
				"repo":         "demo",
				"auth_profile": true,
			},
			wantErr: "invalid 'auth_profile'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			_, err := p.execute(context.Background(), tt.inputs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.NotContains(t, err.Error(), sentinelVal)

			// Validation must also fail closed under dry-run.
			dryCtx := sdkprovider.WithDryRun(context.Background(), true)
			_, dryErr := p.execute(dryCtx, tt.inputs)
			require.Error(t, dryErr)
			assert.Contains(t, dryErr.Error(), tt.wantErr)
		})
	}
}

func TestProvider_AuthOverride_TokenAndProfileAnd401(t *testing.T) {
	t.Parallel()

	t.Run("explicit token bypasses host service and never refreshes ambient token on 401", func(t *testing.T) {
		t.Parallel()

		var sawAuth atomic.Value
		var statusCode atomic.Int32
		statusCode.Store(http.StatusOK)

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			sawAuth.Store(r.Header.Get("Authorization"))
			code := int(statusCode.Load())
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(code)
			if code == http.StatusUnauthorized {
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "Bad credentials"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"repository": map[string]any{"name": "demo", "nameWithOwner": "public-org/demo"}},
			})
		}))
		t.Cleanup(srv.Close)

		svc, hc := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{AccessToken: "ambient-host-token"}, nil
		})

		p := newProvider()
		p.allowPrivateIPs = true

		ctx := sdkplugin.WithHostClient(context.Background(), hc)
		_, err := p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
			"token":     "explicit-step-token",
		})
		require.NoError(t, err)
		assert.Equal(t, "Bearer explicit-step-token", sawAuth.Load())
		assert.Empty(t, svc.getRequests(), "explicit token must not query HostService")

		// Now return 401: explicit token must NOT trigger OnUnauthorized refresh on HostService.
		statusCode.Store(http.StatusUnauthorized)
		_, err = p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
			"token":     "expired-explicit-token",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "Bad credentials")
		assert.Empty(t, svc.getRequests(), "401 with explicit token must never fall back to ambient HostService")
	})

	t.Run("auth_profile overrides ambient profile and refreshes same profile on 401", func(t *testing.T) {
		t.Parallel()

		var reqCount atomic.Int32
		var authHeaders []string
		var mu sync.Mutex

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			authHeaders = append(authHeaders, r.Header.Get("Authorization"))
			mu.Unlock()

			w.Header().Set("Content-Type", "application/json")
			if reqCount.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "token expired"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"repository": map[string]any{"name": "demo", "nameWithOwner": "work-org/demo"}},
			})
		}))
		t.Cleanup(srv.Close)

		svc, hc := startTestAuthHostClient(t, func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			if req.ForceRefresh {
				return &proto.GetAuthTokenResponse{AccessToken: "refreshed-" + req.Profile}, nil
			}
			return &proto.GetAuthTokenResponse{AccessToken: "initial-" + req.Profile}, nil
		})

		p := newProvider()
		p.allowPrivateIPs = true

		ctx := sdkauth.WithProfile(context.Background(), "ambient-default")
		ctx = sdkplugin.WithHostClient(ctx, hc)

		out, err := p.execute(ctx, map[string]any{
			"operation":    "get_repo",
			"owner":        "work-org",
			"repo":         "demo",
			"api_base":     srv.URL,
			"auth_profile": "work-secondary",
		})
		require.NoError(t, err)
		require.NotNil(t, out)

		reqs := svc.getRequests()
		require.Len(t, reqs, 2)
		assert.Equal(t, "work-secondary", reqs[0].Profile)
		assert.False(t, reqs[0].ForceRefresh)
		assert.Equal(t, "work-secondary", reqs[1].Profile)
		assert.True(t, reqs[1].ForceRefresh)

		mu.Lock()
		assert.Equal(t, []string{"Bearer initial-work-secondary", "Bearer refreshed-work-secondary"}, authHeaders)
		mu.Unlock()
	})

	t.Run("auth_profile fails closed when HostClient is missing", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		_, err := p.execute(context.Background(), map[string]any{
			"operation":    "get_repo",
			"owner":        "work-org",
			"repo":         "demo",
			"api_base":     srv.URL,
			"auth_profile": "work-secondary",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "host service client not available for auth_profile")
	})

	t.Run("default ambient HostClient injects profile token, refreshes on 401, and handles refresh errors", func(t *testing.T) {
		t.Parallel()

		var reqCount atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			n := reqCount.Add(1)
			if n == 1 || n >= 3 {
				w.WriteHeader(http.StatusUnauthorized)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "expired"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{"repository": map[string]any{"name": "demo", "nameWithOwner": "public-org/demo"}},
			})
		}))
		t.Cleanup(srv.Close)

		var failRefresh atomic.Bool
		svc, hc := startTestAuthHostClient(t, func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			if req.ForceRefresh && failRefresh.Load() {
				return &proto.GetAuthTokenResponse{Error: "refresh denied"}, nil
			}
			return &proto.GetAuthTokenResponse{AccessToken: "ambient-" + req.Profile}, nil
		})

		p := newProvider()
		p.allowPrivateIPs = true

		ctx := sdkauth.WithProfile(context.Background(), "ambient-prof")
		ctx = sdkplugin.WithHostClient(ctx, hc)

		_, err := p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
		})
		require.NoError(t, err)
		reqs := svc.getRequests()
		require.Len(t, reqs, 2)
		assert.Equal(t, "ambient-prof", reqs[0].Profile)
		assert.False(t, reqs[0].ForceRefresh)
		assert.Equal(t, "ambient-prof", reqs[1].Profile)
		assert.True(t, reqs[1].ForceRefresh)

		// Verify 401 refresh failure closes body cleanly and returns error.
		failRefresh.Store(true)
		_, err = p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "refresh denied")
	})
}

func TestProvider_AuthOverride_ConcurrentSafety(t *testing.T) {
	t.Parallel()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var gqlReq graphqlRequest
		_ = json.NewDecoder(r.Body).Decode(&gqlReq)
		repoName, _ := gqlReq.Variables["name"].(string)

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"name":        repoName,
					"description": r.Header.Get("Authorization"),
				},
			},
		})
	}))
	t.Cleanup(srv.Close)

	_, hc := startTestAuthHostClient(t, func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
		return &proto.GetAuthTokenResponse{AccessToken: "prof-tok-" + req.Profile}, nil
	})

	p := newProvider()
	p.allowPrivateIPs = true

	const workers = 20
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := range workers {
		go func(idx int) {
			defer wg.Done()

			ctx := sdkplugin.WithHostClient(context.Background(), hc)
			inputs := map[string]any{
				"operation": "get_repo",
				"owner":     "acme-corp",
				"repo":      fmt.Sprintf("repo-%d", idx),
				"api_base":  srv.URL,
			}
			var expectedAuth string
			if idx%2 == 0 {
				tok := fmt.Sprintf("direct-tok-%d", idx)
				inputs["token"] = tok
				expectedAuth = "Bearer " + tok
			} else {
				prof := fmt.Sprintf("profile-%d", idx)
				inputs["auth_profile"] = prof
				expectedAuth = "Bearer prof-tok-" + prof
			}

			out, err := p.execute(ctx, inputs)
			if !assert.NoError(t, err) {
				return
			}
			res := out.Data.(map[string]any)["result"].(map[string]any)
			assert.Equal(t, fmt.Sprintf("repo-%d", idx), res["name"])
			assert.Equal(t, expectedAuth, res["description"])
		}(i)
	}

	wg.Wait()
}

func TestProvider_RedirectCredentialStripping(t *testing.T) {
	t.Parallel()

	t.Run("strips Authorization and X-GitHub-Api-Version on cross-origin redirect", func(t *testing.T) {
		t.Parallel()

		var blobAuthHeader atomic.Value
		var blobAPIVersionHeader atomic.Value

		blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			blobAuthHeader.Store(r.Header.Get("Authorization"))
			blobAPIVersionHeader.Store(r.Header.Get("X-GitHub-Api-Version"))
			if r.Header.Get("Authorization") != "" {
				http.Error(w, "InvalidAuthenticationInfo: Authorization header must not be sent alongside SAS", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("step 1 ok\nstep 2 failed\n"))
		}))
		t.Cleanup(blobSrv.Close)

		var apiAuthHeader atomic.Value
		var apiVersionHeader atomic.Value

		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			apiAuthHeader.Store(r.Header.Get("Authorization"))
			apiVersionHeader.Store(r.Header.Get("X-GitHub-Api-Version"))
			http.Redirect(w, r, blobSrv.URL+"/container/job-logs.txt?sig=sas-token", http.StatusFound)
		}))
		t.Cleanup(apiSrv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "get_job_logs",
			"owner":     "acme-corp",
			"repo":      "service-a",
			"job_id":    42,
			"api_base":  apiSrv.URL,
			"token":     "override-val-1",
		})
		require.NoError(t, err)

		assert.Equal(t, "Bearer override-val-1", apiAuthHeader.Load())
		assert.Equal(t, "2022-11-28", apiVersionHeader.Load())
		assert.Equal(t, "", blobAuthHeader.Load())
		assert.Equal(t, "", blobAPIVersionHeader.Load())

		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, "step 1 ok\nstep 2 failed\n", res["logs"])
	})

	t.Run("preserves Authorization on same-origin redirect", func(t *testing.T) {
		t.Parallel()

		var redirectedAuth atomic.Value
		var srv *httptest.Server
		srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/repos/acme-corp/old-name/labels" {
				http.Redirect(w, r, srv.URL+"/repositories/99/labels", http.StatusMovedPermanently)
				return
			}
			redirectedAuth.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{})
		}))
		t.Cleanup(srv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_labels",
			"owner":     "acme-corp",
			"repo":      "old-name",
			"api_base":  srv.URL,
			"token":     "same-origin-val",
		})
		require.NoError(t, err)
		assert.Equal(t, "Bearer same-origin-val", redirectedAuth.Load())
	})

	t.Run("keeps Authorization stripped across multi-hop origin->external->origin bounce", func(t *testing.T) {
		t.Parallel()

		var bouncedAuth atomic.Value
		var apiSrv *httptest.Server
		externalSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, apiSrv.URL+"/bounced-target", http.StatusFound)
		}))
		t.Cleanup(externalSrv.Close)

		apiSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/bounced-target" {
				bouncedAuth.Store(r.Header.Get("Authorization"))
				w.Header().Set("Content-Type", "text/plain")
				_, _ = w.Write([]byte("bounced ok\n"))
				return
			}
			http.Redirect(w, r, externalSrv.URL+"/hop", http.StatusFound)
		}))
		t.Cleanup(apiSrv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "get_job_logs",
			"owner":     "acme-corp",
			"repo":      "service-a",
			"job_id":    7,
			"api_base":  apiSrv.URL,
			"token":     "bounce-test-val",
		})
		require.NoError(t, err)
		assert.Equal(t, "", bouncedAuth.Load(), "credentials must stay stripped after bouncing through an external origin")
	})

	t.Run("preserves SSRF IPPolicy on redirects and redacts presigned query parameters from errors", func(t *testing.T) {
		t.Parallel()

		presignedVal := "presigned-query-marker-9988"
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/?sig="+presignedVal, http.StatusFound)
		}))
		t.Cleanup(apiSrv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		stepTok := "override-val-1"
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "get_job_logs",
			"owner":     "acme-corp",
			"repo":      "service-a",
			"job_id":    8,
			"api_base":  apiSrv.URL,
			"token":     stepTok,
		})
		require.Error(t, err)
		assert.True(t, errors.Is(err, httpc.ErrBlockedByPolicy), "cloud metadata redirect must remain blocked by httpc IPPolicy")
		assert.NotContains(t, err.Error(), presignedVal, "presigned redirect query parameter must be redacted from error string")
		assert.Contains(t, err.Error(), "REDACTED")
	})

	t.Run("redacts signed query and fragment from malformed redirect Location errors while preserving Unwrap", func(t *testing.T) {
		t.Parallel()

		malformedQueryMarker := "malformed-loc-query-9911"
		malformedFragMarker := "malformed-loc-frag-8822"
		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Location", "https://storage.example/%zz?sig="+malformedQueryMarker+"#"+malformedFragMarker)
			w.WriteHeader(http.StatusFound)
		}))
		t.Cleanup(apiSrv.Close)

		p := newProvider()
		p.allowPrivateIPs = true

		stepTok := "override-val-2"
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "get_job_logs",
			"owner":     "acme-corp",
			"repo":      "service-a",
			"job_id":    9,
			"api_base":  apiSrv.URL,
			"token":     stepTok,
		})
		require.Error(t, err)
		assert.NotContains(t, err.Error(), malformedQueryMarker, "nested malformed redirect query marker must be redacted")
		assert.NotContains(t, err.Error(), malformedFragMarker, "nested malformed redirect fragment marker must be redacted")
		assert.Contains(t, err.Error(), "?REDACTED")

		var urlErr *url.Error
		assert.ErrorAs(t, err, &urlErr, "underlying *url.Error must remain unwrappable")
	})
}

func TestRedactURLErrorQuery_EdgeCasesAndUnwrap(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("sentinel cause")
	wrappedParseErr := fmt.Errorf("outer: %w", &url.Error{
		Op:  "parse",
		URL: "https://storage.example/%zz?sig=marker-parse-val",
		Err: fmt.Errorf("%w: %w", url.EscapeError("%zz"), sentinel),
	})
	redactedErr := redactURLErrorQuery(wrappedParseErr)
	require.Error(t, redactedErr)
	assert.NotContains(t, redactedErr.Error(), "marker-parse-val")
	assert.Contains(t, redactedErr.Error(), "?REDACTED")
	assert.ErrorIs(t, redactedErr, sentinel)
	var escErr url.EscapeError
	assert.ErrorAs(t, redactedErr, &escErr)

	cases := []struct {
		name   string
		rawErr string
		marker string
	}{
		{
			name:   "uppercase scheme",
			rawErr: `Get "http://127.0.0.1:8080/logs": failed to parse Location header "HTTPS://storage.example/%zz?sig=upper-val": invalid URL escape "%zz"`,
			marker: "upper-val",
		},
		{
			name:   "relative Location header without scheme",
			rawErr: `failed to parse Location header "blobs/%zz?sig=rel-val#frag-val": parse "blobs/%zz?sig=rel-val#frag-val": invalid URL escape "%zz"`,
			marker: "rel-val",
		},
		{
			name:   "single quote in URL path and query",
			rawErr: `GET https://storage.example/team's-logs?tag=o'reilly&sig=quote-val giving up after 1 attempt(s)`,
			marker: "quote-val",
		},
		{
			name:   "spaces inside quoted Location header",
			rawErr: `failed to parse Location header "https://storage.example/my file?note=a b&sig=space-val": invalid character " " in host name`,
			marker: "space-val",
		},
		{
			name:   "fragment-only value",
			rawErr: `parse "https://storage.example/callback#frag_param=frag-only-val": invalid URI`,
			marker: "frag-only-val",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := redactURLErrorQuery(errors.New(tc.rawErr))
			require.Error(t, got)
			assert.NotContains(t, got.Error(), tc.marker)
			assert.Contains(t, got.Error(), "REDACTED")
		})
	}
}

func TestPlugin_ConcurrentConfigureStopAndExecute(t *testing.T) {
	t.Parallel()

	var unauthenticatedRequests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			unauthenticatedRequests.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"name":"demo","nameWithOwner":"acme-corp/demo"}}}`))
	}))
	t.Cleanup(srv.Close)

	_, unauthHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
		return &proto.GetAuthTokenResponse{Error: "no active session for github"}, nil
	})
	unauthCtx := sdkplugin.WithHostClient(context.Background(), unauthHC)

	rawCfg, err := json.Marshal(map[string]any{"allowPrivateIPs": true})
	require.NoError(t, err)
	cfg := sdkplugin.ProviderConfig{
		Settings: map[string]json.RawMessage{"httpClient": rawCfg},
	}

	plug := NewPlugin()
	require.NoError(t, plug.ConfigureProvider(context.Background(), ProviderName, cfg))

	var wg sync.WaitGroup
	for i := range 16 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			switch idx % 4 {
			case 0:
				_ = plug.ConfigureProvider(context.Background(), ProviderName, cfg)
			case 1:
				_ = plug.StopProvider(context.Background(), ProviderName)
				_ = plug.ConfigureProvider(context.Background(), ProviderName, cfg)
			case 2:
				_, execErr := plug.ExecuteProvider(unauthCtx, ProviderName, map[string]any{
					"operation": "get_repo",
					"owner":     "acme-corp",
					"repo":      "demo",
					"api_base":  srv.URL,
				})
				assert.Error(t, execErr)
			default:
				_, _ = plug.ExecuteProvider(context.Background(), ProviderName, map[string]any{
					"operation": "get_repo",
					"owner":     "acme-corp",
					"repo":      "demo",
					"api_base":  srv.URL,
					"token":     "race-lifecycle-val",
				})
			}
		}(i)
	}
	wg.Wait()
	assert.Equal(t, int32(0), unauthenticatedRequests.Load(), "unauthenticated GraphQL call must never reach HTTP server across concurrent reconfiguration")
}

func BenchmarkExecuteWithTokenOverride(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"name":"demo","nameWithOwner":"public-org/demo"}}}`))
	}))
	b.Cleanup(srv.Close)

	p := newProvider()
	p.allowPrivateIPs = true
	inputs := map[string]any{
		"operation": "get_repo",
		"owner":     "public-org",
		"repo":      "demo",
		"api_base":  srv.URL,
		"token":     "bench-override-val",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(context.Background(), inputs)
	}
}

func TestGraphQLDo_PreFlightFailsFastWhenUnauthenticated(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		hostHandler func(req *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error)
		wantErrSub  string
	}{
		{
			name: "host auth handler returns not authenticated error",
			hostHandler: func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
				return &proto.GetAuthTokenResponse{Error: "no active session for github"}, nil
			},
			wantErrSub: "no active session for github",
		},
		{
			name: "host auth handler returns empty token",
			hostHandler: func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
				return &proto.GetAuthTokenResponse{AccessToken: ""}, nil
			},
		},
		{
			name: "host auth handler returns whitespace token",
			hostHandler: func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
				return &proto.GetAuthTokenResponse{AccessToken: "   \t "}, nil
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var serverHits atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				serverHits.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			t.Cleanup(srv.Close)

			svc, hc := startTestAuthHostClient(t, tc.hostHandler)

			p := newProvider()
			p.allowPrivateIPs = true

			ctx := sdkplugin.WithHostClient(context.Background(), hc)
			_, err := p.execute(ctx, map[string]any{
				"operation": "get_issue",
				"owner":     "oakwood-commons",
				"repo":      "scafctl",
				"number":    1,
				"api_base":  srv.URL,
			})
			require.Error(t, err)
			assert.Equal(t, int32(0), serverHits.Load(), "GraphQL pre-flight must fail before any HTTP request is sent")
			assert.Len(t, svc.getRequests(), 1, "host auth RPC should be queried exactly once")
			assert.Contains(t, err.Error(), "not authenticated with GitHub: run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'")
			if tc.wantErrSub != "" {
				assert.Contains(t, err.Error(), tc.wantErrSub)
			}
		})
	}

	t.Run("cancelled context propagates cancellation instead of auth guidance", func(t *testing.T) {
		t.Parallel()

		var serverHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serverHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		_, hc := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{AccessToken: "valid-tok"}, nil
		})

		p := newProvider()
		p.allowPrivateIPs = true

		cancelCtx, cancel := context.WithCancel(context.Background())
		cancel()
		ctx := sdkplugin.WithHostClient(cancelCtx, hc)

		_, err := graphqlDo(ctx, p.getClient(), srv.URL, "query { viewer { login } }", nil)
		require.Error(t, err)
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotContains(t, err.Error(), "not authenticated with GitHub")
		assert.Equal(t, int32(0), serverHits.Load())

		// Also verify gRPC Canceled / DeadlineExceeded returned mid-RPC is not misreported as missing login.
		_, rpcCancelHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return nil, status.Error(codes.DeadlineExceeded, "host RPC deadline exceeded")
		})
		_, rpcErr := graphqlDo(sdkplugin.WithHostClient(context.Background(), rpcCancelHC), p.getClient(), srv.URL, "query { viewer { login } }", nil)
		require.Error(t, rpcErr)
		assert.NotContains(t, rpcErr.Error(), "not authenticated with GitHub")
		assert.Equal(t, int32(0), serverHits.Load())
	})

	t.Run("custom WithClient auth hook succeeds on GraphQL when host client is unauthenticated", func(t *testing.T) {
		t.Parallel()

		var serverHits atomic.Int32
		var sawAuth atomic.Value
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			serverHits.Add(1)
			sawAuth.Store(r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"name":          "demo",
						"nameWithOwner": "public-org/demo",
					},
				},
			})
		}))
		t.Cleanup(srv.Close)

		svc, unauthHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{Error: "no active session for github"}, nil
		})

		customClient := httpc.NewClient(&httpc.ClientConfig{
			EnableCache: false,
			RetryMax:    0,
			IPPolicy:    httpc.AllowAllPrivateIPs(),
			RequestHooks: []httpc.RequestHook{
				func(req *http.Request) error {
					req.Header.Set("Authorization", "Bearer custom-hook-val")
					return nil
				},
			},
		})
		t.Cleanup(func() { _ = customClient.Close() })

		p := newProvider(WithClient(customClient))
		ctx := sdkplugin.WithHostClient(context.Background(), unauthHC)

		out, err := p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
		})
		require.NoError(t, err)
		require.NotNil(t, out)
		assert.Equal(t, int32(1), serverHits.Load())
		assert.Equal(t, "Bearer custom-hook-val", sawAuth.Load())
		assert.Empty(t, svc.getRequests(), "custom WithClient GraphQL call must not preflight ambient HostService")
	})

	t.Run("retained default client still fails preflight after ConfigureProvider and StopProvider", func(t *testing.T) {
		t.Parallel()

		var serverHits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			serverHits.Add(1)
			w.WriteHeader(http.StatusOK)
		}))
		t.Cleanup(srv.Close)

		svc, unauthHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{Error: "no active session for github"}, nil
		})

		rawCfg, cfgErr := json.Marshal(map[string]any{"allowPrivateIPs": true})
		require.NoError(t, cfgErr)
		cfg := sdkplugin.ProviderConfig{
			Settings: map[string]json.RawMessage{"httpClient": rawCfg},
		}

		plug := NewPlugin()
		require.NoError(t, plug.ConfigureProvider(context.Background(), ProviderName, cfg))

		retainedClient := plug.provider.getClient()
		require.NoError(t, plug.ConfigureProvider(context.Background(), ProviderName, cfg))
		require.NoError(t, plug.StopProvider(context.Background(), ProviderName))

		ctx := sdkplugin.WithHostClient(context.Background(), unauthHC)
		_, err := graphqlDo(ctx, retainedClient, srv.URL, "query { viewer { login } }", nil)
		require.Error(t, err)
		assert.Equal(t, int32(0), serverHits.Load(), "retained default client must still fail preflight before sending any HTTP request")
		assert.Len(t, svc.getRequests(), 1)
		assert.Contains(t, err.Error(), "not authenticated with GitHub: run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'")
	})
}

func TestUnauthenticatedHTTPErrorGuidance(t *testing.T) {
	t.Parallel()

	t.Run("graphqlDo unauthenticated 401 and 403 include auth guidance", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name       string
			statusCode int
			apiMessage string
		}{
			{
				name:       "HTTP 401 Unauthorized",
				statusCode: http.StatusUnauthorized,
				apiMessage: "Requires authentication",
			},
			{
				name:       "HTTP 403 Forbidden rate limit",
				statusCode: http.StatusForbidden,
				apiMessage: "API rate limit exceeded",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.statusCode)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": tc.apiMessage})
				}))
				t.Cleanup(srv.Close)

				p := newProvider()
				p.allowPrivateIPs = true

				_, err := graphqlDo(context.Background(), p.getClient(), srv.URL, "query { viewer { login } }", nil)
				require.Error(t, err)
				assert.Contains(t, err.Error(), fmt.Sprintf("not authenticated with GitHub (HTTP %d): run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'", tc.statusCode))
				assert.Contains(t, err.Error(), tc.apiMessage)
			})
		}
	})

	t.Run("doRESTRequest and executeGetJobLogs unauthenticated 401 and 403 include auth guidance and preserve restError", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name           string
			statusCode     int
			apiMessage     string
			withHostNoAuth bool
		}{
			{
				name:       "HTTP 401 without host client",
				statusCode: http.StatusUnauthorized,
				apiMessage: "Requires authentication",
			},
			{
				name:       "HTTP 403 without host client",
				statusCode: http.StatusForbidden,
				apiMessage: "API rate limit exceeded for 203.0.113.10",
			},
			{
				name:           "HTTP 401 with unauthenticated host client",
				statusCode:     http.StatusUnauthorized,
				apiMessage:     "Requires authentication",
				withHostNoAuth: true,
			},
			{
				name:           "HTTP 403 with unauthenticated host client",
				statusCode:     http.StatusForbidden,
				apiMessage:     "Resource not accessible without authentication",
				withHostNoAuth: true,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.statusCode)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": tc.apiMessage})
				}))
				t.Cleanup(srv.Close)

				p := newProvider()
				p.allowPrivateIPs = true

				ctx := context.Background()
				if tc.withHostNoAuth {
					_, hc := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
						return &proto.GetAuthTokenResponse{Error: "not logged in"}, nil
					})
					ctx = sdkplugin.WithHostClient(ctx, hc)
				}

				_, err := p.doRESTRequest(ctx, p.getClient(), http.MethodGet, srv.URL+"/repos/public-org/demo/labels", nil)
				require.Error(t, err)
				assert.Contains(t, err.Error(), fmt.Sprintf("not authenticated with GitHub (HTTP %d): run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'", tc.statusCode))
				assert.Contains(t, err.Error(), tc.apiMessage)

				var re *restError
				require.ErrorAs(t, err, &re)
				assert.Equal(t, tc.statusCode, re.StatusCode)
				assert.Equal(t, tc.apiMessage, re.Message)

				// Verify executeGetJobLogs also enriches unauthenticated 401/403 while wrapping restError.
				_, logsErr := p.execute(ctx, map[string]any{
					"operation": "get_job_logs",
					"owner":     "public-org",
					"repo":      "demo",
					"job_id":    101,
					"api_base":  srv.URL,
				})
				require.Error(t, logsErr)
				assert.Contains(t, logsErr.Error(), fmt.Sprintf("not authenticated with GitHub (HTTP %d): run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'", tc.statusCode))
				var logsRE *restError
				require.ErrorAs(t, logsErr, &logsRE)
				assert.Equal(t, tc.statusCode, logsRE.StatusCode)
			})
		}
	})

	t.Run("unauthenticated REST 200 OK against public repository still succeeds when host client has no token", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"name": "bug"}})
		}))
		t.Cleanup(srv.Close)

		_, hc := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{Error: "not logged in"}, nil
		})

		p := newProvider()
		p.allowPrivateIPs = true

		ctx := sdkplugin.WithHostClient(context.Background(), hc)
		out, err := p.execute(ctx, map[string]any{
			"operation": "list_labels",
			"owner":     "public-org",
			"repo":      "demo",
			"api_base":  srv.URL,
		})
		require.NoError(t, err)
		require.NotNil(t, out)
	})

	t.Run("authenticated 401 and 403 preserve existing errors without misleading not-authenticated prefix", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name       string
			statusCode int
			apiMessage string
		}{
			{
				name:       "HTTP 401 Bad credentials",
				statusCode: http.StatusUnauthorized,
				apiMessage: "Bad credentials",
			},
			{
				name:       "HTTP 403 Resource not accessible by integration",
				statusCode: http.StatusForbidden,
				apiMessage: "Resource not accessible by integration",
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "Bearer ghp_test", r.Header.Get("Authorization"))
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.statusCode)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": tc.apiMessage})
				}))
				t.Cleanup(srv.Close)

				p := newProvider()
				p.allowPrivateIPs = true

				stepToken := "ghp_test" //nolint:gosec // synthetic test credential
				authCtx, err := withRequestAuthOverride(context.Background(), map[string]any{"token": stepToken})
				require.NoError(t, err)

				_, gqlErr := graphqlDo(authCtx, p.getClient(), srv.URL, "query { viewer { login } }", nil)
				require.Error(t, gqlErr)
				assert.EqualError(t, gqlErr, fmt.Sprintf("GitHub GraphQL API error (HTTP %d): %s", tc.statusCode, tc.apiMessage))
				assert.NotContains(t, gqlErr.Error(), "not authenticated with GitHub")

				_, restErr := p.doRESTRequest(authCtx, p.getClient(), http.MethodGet, srv.URL+"/repos/public-org/demo/labels", nil)
				require.Error(t, restErr)
				assert.EqualError(t, restErr, fmt.Sprintf("GitHub API error (HTTP %d): %s", tc.statusCode, tc.apiMessage))
				assert.NotContains(t, restErr.Error(), "not authenticated with GitHub")

				var re *restError
				require.ErrorAs(t, restErr, &re)
				assert.Equal(t, tc.statusCode, re.StatusCode)
			})
		}
	})

	t.Run("custom WithClient injecting Authorization header preserves existing 403 error on GraphQL and REST", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "Bearer custom-hook-val", r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "SSO authorization required"})
		}))
		t.Cleanup(srv.Close)

		svc, unauthHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{Error: "not logged in"}, nil
		})
		ctx := sdkplugin.WithHostClient(context.Background(), unauthHC)

		customClient := httpc.NewClient(&httpc.ClientConfig{
			EnableCache: false,
			RetryMax:    0,
			IPPolicy:    httpc.AllowAllPrivateIPs(),
			RequestHooks: []httpc.RequestHook{
				func(req *http.Request) error {
					req.Header.Set("Authorization", "Bearer custom-hook-val")
					return nil
				},
			},
		})
		t.Cleanup(func() { _ = customClient.Close() })

		p := newProvider(WithClient(customClient))
		_, gqlErr := p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "org",
			"repo":      "repo",
			"api_base":  srv.URL,
		})
		require.Error(t, gqlErr)
		assert.EqualError(t, gqlErr, "github: GitHub GraphQL API error (HTTP 403): SSO authorization required")
		assert.NotContains(t, gqlErr.Error(), "not authenticated with GitHub")

		_, err := p.doRESTRequest(ctx, customClient, http.MethodGet, srv.URL+"/repos/org/repo", nil)
		require.Error(t, err)
		assert.EqualError(t, err, "GitHub API error (HTTP 403): SSO authorization required")
		assert.NotContains(t, err.Error(), "not authenticated with GitHub")
		assert.Empty(t, svc.getRequests())
	})

	t.Run("custom WithClient without auth hook adds guidance on GraphQL 401 even when host client is unauthenticated", func(t *testing.T) {
		t.Parallel()

		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			assert.Empty(t, r.Header.Get("Authorization"))
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Requires authentication"})
		}))
		t.Cleanup(srv.Close)

		svc, unauthHC := startTestAuthHostClient(t, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
			return &proto.GetAuthTokenResponse{Error: "not logged in"}, nil
		})
		ctx := sdkplugin.WithHostClient(context.Background(), unauthHC)

		customClient := httpc.NewClient(&httpc.ClientConfig{
			EnableCache: false,
			RetryMax:    0,
			IPPolicy:    httpc.AllowAllPrivateIPs(),
		})
		t.Cleanup(func() { _ = customClient.Close() })

		p := newProvider(WithClient(customClient))
		_, err := p.execute(ctx, map[string]any{
			"operation": "get_repo",
			"owner":     "org",
			"repo":      "repo",
			"api_base":  srv.URL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not authenticated with GitHub (HTTP 401): run 'scafctl auth login github', set GITHUB_TOKEN, or pass 'token'")
		assert.Contains(t, err.Error(), "Requires authentication")
		assert.Empty(t, svc.getRequests())
	})

	t.Run("cross-origin redirect 403 does not misreport external storage error as GitHub auth failure", func(t *testing.T) {
		t.Parallel()

		blobSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "ExpiredBlobSignature"})
		}))
		t.Cleanup(blobSrv.Close)

		apiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, blobSrv.URL+"/logs.txt", http.StatusFound)
		}))
		t.Cleanup(apiSrv.Close)

		customClient := httpc.NewClient(&httpc.ClientConfig{
			EnableCache: false,
			RetryMax:    0,
			IPPolicy:    httpc.AllowAllPrivateIPs(),
		})
		t.Cleanup(func() { _ = customClient.Close() })

		p := newProvider(WithClient(customClient))
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "get_job_logs",
			"owner":     "public-org",
			"repo":      "demo",
			"job_id":    55,
			"api_base":  apiSrv.URL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "ExpiredBlobSignature")
		assert.NotContains(t, err.Error(), "not authenticated with GitHub")
	})
}

func BenchmarkGraphQLDo_UnauthenticatedPreFlight(b *testing.B) {
	_, hc := startTestAuthHostClient(b, func(_ *proto.GetAuthTokenRequest) (*proto.GetAuthTokenResponse, error) {
		return &proto.GetAuthTokenResponse{Error: "not logged in"}, nil
	})

	p := newProvider()
	p.allowPrivateIPs = true
	client := p.getClient()
	ctx := sdkplugin.WithHostClient(context.Background(), hc)

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = graphqlDo(ctx, client, "https://api.github.com", "query { viewer { login } }", nil)
	}
}
