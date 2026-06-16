// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── state_load tests ────────────────────────────────────────────────────────

func TestProvider_Execute_StateLoad_FileExists(t *testing.T) {
	t.Parallel()

	stateData := map[string]any{
		"app_name": "my-app",
		"version":  "1.0.0",
	}
	stateJSON, _ := json.Marshal(stateData)

	p, baseURL := testProvider(t, graphqlHandler(t,
		func(query string, vars map[string]any) {
			assert.Contains(t, query, "object(expression:")
			assert.Equal(t, "test-org", vars["owner"])
			assert.Equal(t, "state-repo", vars["name"])
			assert.Equal(t, "main:state/app.json", vars["expression"])
		},
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": map[string]any{
						"text": string(stateJSON),
					},
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"ref":       "main",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	data := result["data"].(map[string]any)
	assert.Equal(t, "my-app", data["app_name"])
	assert.Equal(t, "1.0.0", data["version"])
}

func TestProvider_Execute_StateLoad_FileNotFound(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": nil,
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"ref":       "main",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	data := result["data"].(map[string]any)
	assert.Empty(t, data, "should return empty state for missing file")
}

func TestProvider_Execute_StateLoad_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"ref":       "main",
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.NotNil(t, result["data"])
}

func TestProvider_Execute_StateLoad_MissingPath(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"ref":       "main",
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'path' is required")
}

func TestProvider_Execute_StateLoad_MissingRef(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'ref' is required")
}

func TestProvider_Execute_StateLoad_InvalidJSON(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": map[string]any{
						"text": "not valid json {{{",
					},
				},
			},
		},
	))

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"ref":       "main",
		"api_base":  baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse state JSON")
}

func TestProvider_Execute_StateLoad_RepoNotFound(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"errors": []any{
				map[string]any{
					"message": "Could not resolve to a Repository",
					"type":    "NOT_FOUND",
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "nonexistent",
		"path":      "state/app.json",
		"ref":       "main",
		"api_base":  baseURL,
	})

	// NOT_FOUND on repo is now propagated as a real error, not treated as file-not-found.
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Could not resolve to a Repository")
	assert.Nil(t, output)
}

// ─── state_save tests ────────────────────────────────────────────────────────

func TestProvider_Execute_StateSave_Success(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		w.Header().Set("Content-Type", "application/json")

		// Intercept viewerPermission queries
		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"viewerPermission": "ADMIN"},
				},
			})
			return
		}

		n := callCount.Add(1)
		switch n {
		case 1:
			// getHeadOID query
			assert.Contains(t, req.Query, "ref(qualifiedName:")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{
								"oid": "abc123def456789012345678901234567890abcd",
							},
						},
					},
				},
			})
		case 2:
			// createCommitOnBranch mutation
			assert.Contains(t, req.Query, "createCommitOnBranch")
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "abc123def456789012345678901234567890abcd", input["expectedHeadOid"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":           "new123abc456",
							"url":           "https://github.com/test-org/state-repo/commit/new123abc456",
							"committedDate": "2026-01-01T00:00:00Z",
							"message":       "chore(state): update state",
						},
					},
				},
			})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"app_name": "my-app", "counter": float64(42)},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "new123abc456", result["commit_oid"])
	assert.Equal(t, int32(2), callCount.Load())
}

func TestProvider_Execute_StateSave_OIDConflict(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"viewerPermission": "ADMIN"},
				},
			})
			return
		}

		n := callCount.Add(1)
		switch n {
		case 1:
			// getHeadOID
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{
								"oid": "abc123def456789012345678901234567890abcd",
							},
						},
					},
				},
			})
		case 2:
			// createCommitOnBranch -- OID conflict
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"errors": []any{
					map[string]any{
						"message": "The expectedHeadOid didn't match the actual head oid",
						"type":    "UNPROCESSABLE",
					},
				},
			})
		}
	})

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"key": "value"},
		"api_base":  baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "state save conflict")
	assert.Contains(t, err.Error(), "concurrent commit")
}

func TestProvider_Execute_StateSave_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"key": "value"},
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
}

func TestProvider_Execute_StateSave_MissingData(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'data' is required")
}

func TestProvider_Execute_StateSave_MissingBranch(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"data":      map[string]any{"key": "value"},
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'branch' is required")
}

func TestProvider_Execute_StateSave_CustomMessage(t *testing.T) {
	t.Parallel()

	var commitMessage string
	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"viewerPermission": "ADMIN"},
				},
			})
			return
		}

		n := callCount.Add(1)
		switch n {
		case 1:
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{
								"oid": "abc123def456789012345678901234567890abcd",
							},
						},
					},
				},
			})
		case 2:
			input := req.Variables["input"].(map[string]any)
			msg := input["message"].(map[string]any)
			commitMessage = msg["headline"].(string)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":     "newoid",
							"url":     "https://github.com/o/r/commit/newoid",
							"message": commitMessage,
						},
					},
				},
			})
		}
	})

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"x": "y"},
		"message":   "chore: custom state message",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	assert.Equal(t, "chore: custom state message", commitMessage)
}

// ─── state_delete tests ──────────────────────────────────────────────────────

func TestProvider_Execute_StateDelete_FileExists(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		w.Header().Set("Content-Type", "application/json")

		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"viewerPermission": "ADMIN"},
				},
			})
			return
		}

		n := callCount.Add(1)
		switch n {
		case 1:
			// getFileContentRaw -- file exists
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"object": map[string]any{
							"text": `{"old": "data"}`,
						},
					},
				},
			})
		case 2:
			// getHeadOID
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{
								"oid": "abc123def456789012345678901234567890abcd",
							},
						},
					},
				},
			})
		case 3:
			// createCommitOnBranch with deletion
			assert.Contains(t, req.Query, "createCommitOnBranch")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":     "del789",
							"url":     "https://github.com/o/r/commit/del789",
							"message": "chore(state): delete state",
						},
					},
				},
			})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_delete",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, int32(3), callCount.Load())
}

func TestProvider_Execute_StateDelete_FileNotFound(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": nil,
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_delete",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
}

func TestProvider_Execute_StateDelete_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation": "state_delete",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
}

func TestProvider_Execute_StateDelete_MissingPath(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_delete",
		"owner":     "test-org",
		"repo":      "state-repo",
		"branch":    "main",
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'path' is required")
}

func TestProvider_Execute_StateDelete_MissingBranch(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_delete",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"api_base":  "http://localhost",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'branch' is required")
}

// ─── Helper tests ────────────────────────────────────────────────────────────

func TestProvider_Execute_StateLoad_NonObjectJSON(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": map[string]any{
						"text": `["a","b","c"]`,
					},
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_load",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"ref":       "main",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	data, ok := result["data"].([]any)
	require.True(t, ok, "data should be an array")
	assert.Len(t, data, 3)
}

func TestProvider_GetFileContentRaw_NonTextBlob(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		nil,
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"object": map[string]any{
						// Blob without text field (e.g. binary file).
					},
				},
			},
		},
	))

	_, err := p.getFileContentRaw(
		context.Background(),
		p.getClient(),
		baseURL,
		"test-org", "state-repo",
		"binary-file.bin", "main",
	)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a readable text blob")
}

func TestIsFileNotFound(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "fileNotFoundError",
			err:  &fileNotFoundError{path: "state.json", ref: "main"},
			want: true,
		},
		{
			name: "wrapped fileNotFoundError",
			err:  fmt.Errorf("outer: %w", &fileNotFoundError{path: "state.json", ref: "main"}),
			want: true,
		},
		{
			name: "other error",
			err:  fmt.Errorf("something else"),
			want: false,
		},
		{
			name: "nil error",
			err:  nil,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isFileNotFound(tc.err))
		})
	}
}

func TestIsOIDMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "expectedHeadOid mismatch",
			err:  &GraphQLError{Errors: []graphqlError{{Message: "The expectedHeadOid didn't match"}}},
			want: true,
		},
		{
			name: "head OID mismatch",
			err:  &GraphQLError{Errors: []graphqlError{{Message: "head OID has changed"}}},
			want: true,
		},
		{
			name: "other GraphQL error",
			err:  &GraphQLError{Errors: []graphqlError{{Message: "not found", Type: "NOT_FOUND"}}},
			want: false,
		},
		{
			name: "non-GraphQL error",
			err:  fmt.Errorf("something else"),
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.want, isOIDMismatch(tc.err))
		})
	}
}
