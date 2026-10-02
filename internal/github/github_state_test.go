// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
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

func TestProvider_Execute_StateSave_PartialDataReturnsSuccess(t *testing.T) {
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
			// createCommitOnBranch returns commit data alongside a field-level
			// FORBIDDEN error -- the mutation applied and must be treated as
			// success, not replayed.
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":     "new123abc456",
							"url":     "https://github.com/test-org/state-repo/commit/new123abc456",
							"message": "chore(state): update state",
						},
					},
				},
				"errors": []any{
					map[string]any{
						"message": "Resource not accessible by personal access token",
						"type":    "FORBIDDEN",
						"path":    []any{"createCommitOnBranch", "commit", "signature", "signer"},
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
		"data":      map[string]any{"key": "value"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
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

// ─── state_save first-save (branch bootstrap) tests ──────────────────────────

// respondGQLData writes a successful GraphQL response with the given data node.
func respondGQLData(w http.ResponseWriter, data map[string]any) {
	json.NewEncoder(w).Encode(map[string]any{"data": data}) //nolint:errcheck,gosec
}

// respondGQLErrors writes a GraphQL error response with the given message.
func respondGQLErrors(w http.ResponseWriter, message, errType string) {
	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
		"errors": []any{map[string]any{"message": message, "type": errType}},
	})
}

func TestProvider_Execute_StateSave_FirstSave_CreatesBranchFromDefault(t *testing.T) {
	t.Parallel()

	const baseOID = "abc123def456abc123def456abc123def456abc1"
	var createdBranchName string
	var createdBranchOID string

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "defaultBranchRef"):
			respondGQLData(w, map[string]any{"repository": map[string]any{
				"defaultBranchRef": map[string]any{"target": map[string]any{"oid": baseOID}},
			}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// The target state branch does not exist yet.
			assert.Equal(t, "refs/heads/scafctl-state", req.Variables["qualifiedName"])
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "createRef"):
			input := req.Variables["input"].(map[string]any)
			createdBranchName, _ = input["name"].(string)
			createdBranchOID, _ = input["oid"].(string)
			respondGQLData(w, map[string]any{"createRef": map[string]any{"ref": map[string]any{
				"name":   "refs/heads/scafctl-state",
				"target": map[string]any{"oid": baseOID},
			}}})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, baseOID, input["expectedHeadOid"])
			branchInput := input["branch"].(map[string]any)
			assert.Equal(t, "scafctl-state", branchInput["branchName"])
			respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
				"oid":     "newcommit00000000000000000000000000000001",
				"url":     "https://github.com/test-org/state-repo/commit/newcommit01",
				"message": "chore(state): update state",
			}}})
		default:
			// resolveRepoID for createRef.
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "newcommit00000000000000000000000000000001", result["commit_oid"])
	assert.Equal(t, "refs/heads/scafctl-state", createdBranchName)
	assert.Equal(t, baseOID, createdBranchOID)
}

func TestProvider_Execute_StateSave_FirstSave_WithBaseRef(t *testing.T) {
	t.Parallel()

	const baseOID = "dddd1111dddd1111dddd1111dddd1111dddd1111"
	var defaultBranchQueried bool

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "defaultBranchRef"):
			defaultBranchQueried = true
			respondGQLData(w, map[string]any{"repository": map[string]any{"defaultBranchRef": nil}})
		case strings.Contains(req.Query, "branchRef"):
			// base_ref resolution: "develop" resolves as a branch.
			assert.Equal(t, "refs/heads/develop", req.Variables["branch"])
			respondGQLData(w, map[string]any{"repository": map[string]any{
				"branchRef": map[string]any{"target": map[string]any{"oid": baseOID}},
				"tagRef":    nil,
			}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// The target state branch does not exist yet.
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "createRef"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, baseOID, input["oid"])
			respondGQLData(w, map[string]any{"createRef": map[string]any{"ref": map[string]any{
				"name":   "refs/heads/scafctl-state",
				"target": map[string]any{"oid": baseOID},
			}}})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, baseOID, input["expectedHeadOid"])
			respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
				"oid": "committedfrombaseref000000000000000000001",
			}}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"base_ref":  "develop",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "committedfrombaseref000000000000000000001", result["commit_oid"])
	assert.False(t, defaultBranchQueried, "default branch must not be queried when base_ref is provided")
}

func TestProvider_Execute_StateSave_FirstSave_BaseRefNotFound(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "branchRef"):
			// base_ref "nonexistent" is neither a branch nor a tag.
			respondGQLData(w, map[string]any{"repository": map[string]any{"branchRef": nil, "tagRef": nil}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// The target state branch does not exist.
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"base_ref":  "nonexistent",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "base_ref \"nonexistent\" not found")
}

func TestProvider_Execute_StateSave_FirstSave_WithBaseRef_Tag(t *testing.T) {
	t.Parallel()

	// Annotated tag: the tag object OID differs from the underlying commit OID,
	// which is what the new branch must point at.
	const commitOID = "cccc3333cccc3333cccc3333cccc3333cccc3333"

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "branchRef"):
			// base_ref "v1.0.0" is not a branch but resolves as an annotated tag
			// whose target peels to the commit OID.
			assert.Equal(t, "refs/tags/v1.0.0", req.Variables["tag"])
			respondGQLData(w, map[string]any{"repository": map[string]any{
				"branchRef": nil,
				"tagRef": map[string]any{"target": map[string]any{
					"oid":    "tagobject00000000000000000000000000000001",
					"target": map[string]any{"oid": commitOID},
				}},
			}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "createRef"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, commitOID, input["oid"], "branch must be created from the peeled commit OID")
			respondGQLData(w, map[string]any{"createRef": map[string]any{"ref": map[string]any{
				"name":   "refs/heads/scafctl-state",
				"target": map[string]any{"oid": commitOID},
			}}})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, commitOID, input["expectedHeadOid"])
			respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
				"oid": "committedfromtag0000000000000000000000001",
			}}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"base_ref":  "v1.0.0",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "committedfromtag0000000000000000000000001", result["commit_oid"])
}

func TestProvider_Execute_StateSave_FirstSave_WithBaseRef_CommitSHA(t *testing.T) {
	t.Parallel()

	const commitSHA = "abcdef0123456789abcdef0123456789abcdef01"
	var objectQueried bool

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "object(oid"):
			// SHA fallback: base_ref is neither branch nor tag, resolved as commit.
			objectQueried = true
			assert.Equal(t, commitSHA, req.Variables["oid"])
			respondGQLData(w, map[string]any{"repository": map[string]any{"object": map[string]any{
				"__typename": "Commit",
				"oid":        commitSHA,
			}}})
		case strings.Contains(req.Query, "branchRef"):
			// base_ref SHA is neither a branch nor a tag.
			respondGQLData(w, map[string]any{"repository": map[string]any{"branchRef": nil, "tagRef": nil}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "createRef"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, commitSHA, input["oid"])
			respondGQLData(w, map[string]any{"createRef": map[string]any{"ref": map[string]any{
				"name":   "refs/heads/scafctl-state",
				"target": map[string]any{"oid": commitSHA},
			}}})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, commitSHA, input["expectedHeadOid"])
			respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
				"oid": "committedfromsha00000000000000000000000001",
			}}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"base_ref":  commitSHA,
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "committedfromsha00000000000000000000000001", result["commit_oid"])
	assert.True(t, objectQueried, "a full SHA base_ref must be verified via object(oid:)")
}

func TestProvider_Execute_StateSave_FirstSave_BranchAlreadyExistsRace(t *testing.T) {
	t.Parallel()

	const existingOID = "eeee2222eeee2222eeee2222eeee2222eeee2222"
	var branchRefQueries atomic.Int32

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "viewerPermission"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
		case strings.Contains(req.Query, "defaultBranchRef"):
			respondGQLData(w, map[string]any{"repository": map[string]any{
				"defaultBranchRef": map[string]any{"target": map[string]any{"oid": "0000000000000000000000000000000000000000"}},
			}})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// First query: branch missing. Second query (after createRef
			// already-exists): branch now present with its own HEAD.
			if branchRefQueries.Add(1) == 1 {
				respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
				return
			}
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": map[string]any{
				"target": map[string]any{"oid": existingOID},
			}}})
		case strings.Contains(req.Query, "createRef"):
			respondGQLErrors(w, "A ref named refs/heads/scafctl-state already exists in the repository.", "UNPROCESSABLE")
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, existingOID, input["expectedHeadOid"])
			respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
				"oid": "raceresolvedcommit0000000000000000000001",
			}}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "raceresolvedcommit0000000000000000000001", result["commit_oid"])
	assert.Equal(t, int32(2), branchRefQueries.Load())
}

func TestProvider_Execute_StateSave_FirstSave_EmptyRepo_DefaultBranch(t *testing.T) {
	t.Parallel()

	var putBody map[string]any

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		// REST endpoints.
		if r.URL.Path == "/repos/test-org/state-repo" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}) //nolint:errcheck,gosec
			return
		}
		if r.URL.Path == "/repos/test-org/state-repo/contents/state/app.json" && r.Method == http.MethodPut {
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &putBody) //nolint:errcheck,gosec
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"commit": map[string]any{"sha": "emptyrepobootstrapsha000000000000000001"},
			})
			return
		}

		// GraphQL endpoint.
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// Branch does not exist (empty repo).
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "defaultBranchRef"):
			// Empty repo -- no default branch ref.
			respondGQLData(w, map[string]any{"repository": map[string]any{"defaultBranchRef": nil}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, "emptyrepobootstrapsha000000000000000001", result["commit_oid"])
	require.NotNil(t, putBody)
	assert.NotContains(t, putBody, "branch", "branch must be omitted when it equals the default branch on an empty repo")
	assert.NotEmpty(t, putBody["content"], "state content must be sent")
}

func TestProvider_Execute_StateSave_FirstSave_EmptyRepo_CustomBranchError(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/test-org/state-repo" && r.Method == http.MethodGet {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"}) //nolint:errcheck,gosec
			return
		}
		if r.URL.Path == "/repos/test-org/state-repo/contents/state/app.json" && r.Method == http.MethodPut {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "Branch scafctl-state not found"}) //nolint:errcheck,gosec
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec
		w.Header().Set("Content-Type", "application/json")

		switch {
		case strings.Contains(req.Query, "ref(qualifiedName"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
		case strings.Contains(req.Query, "defaultBranchRef"):
			respondGQLData(w, map[string]any{"repository": map[string]any{"defaultBranchRef": nil}})
		default:
			respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
		}
	})

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "state/app.json",
		"branch":    "scafctl-state",
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "has no commits yet")
	assert.Contains(t, err.Error(), "scafctl-state")
	assert.Contains(t, err.Error(), "main")
}

// ─── state_save pull_request tests ───────────────────────────────────────────

const (
	prTestBaseOID   = "abc123def456abc123def456abc123def456abc1"
	prTestCommitOID = "newcommit00000000000000000000000000000001"
	prTestBranch    = "intent/hello-sandbox"
	prTestURL       = "https://github.com/test-org/state-repo/pull/7"
)

// stateSaveFake is a fake GitHub API for state_save pull request tests. It
// answers every GraphQL and REST call state_save makes, records the calls in
// order, and captures the variables of the pull request calls.
type stateSaveFake struct {
	branchExists  bool             // the state branch exists; false makes state_save bootstrap it
	emptyRepo     bool             // the repository has no commits (no default branch ref)
	defaultBranch string           // default_branch returned by GET /repos/{owner}/{repo}
	openPRs       []map[string]any // open pull requests the lookup returns, in order; none by default
	commitErr     string           // GraphQL error message for createCommitOnBranch
	lookupErr     string           // GraphQL error message for the pull request lookup
	createErr     string           // GraphQL error message for createPullRequest

	mu         sync.Mutex
	calls      []string
	lookupVars map[string]any
	createIn   map[string]any
}

func newStateSaveFake() *stateSaveFake {
	return &stateSaveFake{branchExists: true, defaultBranch: "main"}
}

func (f *stateSaveFake) record(call string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// callLog returns the recorded calls in order; never nil.
func (f *stateSaveFake) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...)
}

// captured returns the variables of the pull request lookup and the input of
// the createPullRequest mutation (nil for a call that was never made).
func (f *stateSaveFake) captured() (lookup, create map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lookupVars, f.createIn
}

func (f *stateSaveFake) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/repos/test-org/state-repo":
		f.record("rest:get-repo")
		json.NewEncoder(w).Encode(map[string]any{"default_branch": f.defaultBranch}) //nolint:errcheck,gosec
		return
	case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/repos/test-org/state-repo/contents/"):
		f.record("rest:put-contents")
		json.NewEncoder(w).Encode(map[string]any{"commit": map[string]any{"sha": prTestCommitOID}}) //nolint:errcheck,gosec
		return
	}

	body, _ := io.ReadAll(r.Body)
	var req graphqlRequest
	json.Unmarshal(body, &req) //nolint:errcheck,gosec

	switch {
	case strings.Contains(req.Query, "viewerPermission"):
		respondGQLData(w, map[string]any{"repository": map[string]any{"viewerPermission": "ADMIN"}})
	case strings.Contains(req.Query, "pullRequests("):
		f.record("find-pr")
		f.mu.Lock()
		f.lookupVars = req.Variables
		f.mu.Unlock()
		if f.lookupErr != "" {
			respondGQLErrors(w, f.lookupErr, "UNPROCESSABLE")
			return
		}
		nodes := make([]any, 0, len(f.openPRs))
		for _, pr := range f.openPRs {
			node := maps.Clone(pr)
			// Like GraphQL, return only the fields the query selects.
			if !strings.Contains(req.Query, "isCrossRepository") {
				delete(node, "isCrossRepository")
			}
			nodes = append(nodes, node)
		}
		respondGQLData(w, map[string]any{"repository": map[string]any{"pullRequests": map[string]any{"nodes": nodes}}})
	case strings.Contains(req.Query, "createPullRequest"):
		f.record("create-pr")
		f.mu.Lock()
		f.createIn, _ = req.Variables["input"].(map[string]any)
		f.mu.Unlock()
		if f.createErr != "" {
			respondGQLErrors(w, f.createErr, "UNPROCESSABLE")
			return
		}
		respondGQLData(w, map[string]any{"createPullRequest": map[string]any{"pullRequest": map[string]any{
			"id": "PR_new", "number": 7, "url": prTestURL, "state": "OPEN",
		}}})
	case strings.Contains(req.Query, "defaultBranchRef"):
		f.record("default-branch-oid")
		if f.emptyRepo {
			respondGQLData(w, map[string]any{"repository": map[string]any{"defaultBranchRef": nil}})
			return
		}
		respondGQLData(w, map[string]any{"repository": map[string]any{
			"defaultBranchRef": map[string]any{"target": map[string]any{"oid": prTestBaseOID}},
		}})
	case strings.Contains(req.Query, "branchRef"):
		// base_ref resolution (must precede the ref(qualifiedName case: the
		// query contains both).
		f.record("base-ref-oid")
		respondGQLData(w, map[string]any{"repository": map[string]any{
			"branchRef": map[string]any{"target": map[string]any{"oid": prTestBaseOID}},
			"tagRef":    nil,
		}})
	case strings.Contains(req.Query, "ref(qualifiedName"):
		f.record("branch-head")
		if !f.branchExists {
			respondGQLData(w, map[string]any{"repository": map[string]any{"ref": nil}})
			return
		}
		respondGQLData(w, map[string]any{"repository": map[string]any{
			"ref": map[string]any{"target": map[string]any{"oid": prTestBaseOID}},
		}})
	case strings.Contains(req.Query, "createRef"):
		f.record("create-branch")
		respondGQLData(w, map[string]any{"createRef": map[string]any{"ref": map[string]any{
			"name":   "refs/heads/" + prTestBranch,
			"target": map[string]any{"oid": prTestBaseOID},
		}}})
	case strings.Contains(req.Query, "createCommitOnBranch"):
		f.record("commit")
		if f.commitErr != "" {
			respondGQLErrors(w, f.commitErr, "UNPROCESSABLE")
			return
		}
		respondGQLData(w, map[string]any{"createCommitOnBranch": map[string]any{"commit": map[string]any{
			"oid": prTestCommitOID,
		}}})
	default:
		// resolveRepoID, used by createRef and createPullRequest.
		f.record("repo-id")
		respondGQLData(w, map[string]any{"repository": map[string]any{"id": "R_repo123"}})
	}
}

// saveState runs state_save against the fake with extra inputs layered over a
// baseline save of a small document to prTestBranch.
func (f *stateSaveFake) saveState(t *testing.T, extra map[string]any) (*sdkprovider.Output, error) {
	t.Helper()
	p, baseURL := testProvider(t, f.serve)
	inputs := map[string]any{
		"operation": "state_save",
		"owner":     "test-org",
		"repo":      "state-repo",
		"path":      "intent/state.json",
		"branch":    prTestBranch,
		"data":      map[string]any{"k": "v"},
		"api_base":  baseURL,
	}
	maps.Copy(inputs, extra)
	return p.execute(context.Background(), inputs)
}

func TestProvider_Execute_StateSave_PullRequest_OpensNew(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	output, err := f.saveState(t, map[string]any{
		"pull_request": map[string]any{
			"title": "intent: hello sandbox",
			"body":  "Automated state update",
			"base":  "main",
			"draft": true,
		},
	})

	require.NoError(t, err)
	require.NotNil(t, output)
	result := output.Data.(map[string]any)
	assert.Equal(t, true, result["success"])
	assert.Equal(t, prTestCommitOID, result["commit_oid"])
	// number is normalized from the GraphQL float64 to an int.
	assert.Equal(t, map[string]any{"number": 7, "url": prTestURL, "created": true}, result["pull_request"])

	// The pull request is looked up, then opened, strictly after the commit.
	assert.Equal(t, []string{"branch-head", "commit", "find-pr", "repo-id", "create-pr"}, f.callLog())
	lookup, create := f.captured()
	assert.Equal(t, map[string]any{
		"owner": "test-org", "name": "state-repo", "head": prTestBranch, "base": "main",
	}, lookup)
	assert.Equal(t, map[string]any{
		"repositoryId": "R_repo123",
		"title":        "intent: hello sandbox",
		"body":         "Automated state update",
		"headRefName":  prTestBranch,
		"baseRefName":  "main",
		"draft":        true,
	}, create)

	// The output conforms to the descriptor's state output schema.
	stateSchema := newProvider().Descriptor().OutputSchemas[sdkprovider.CapabilityState]
	require.NoError(t, sdkprovider.NewSchemaValidator().ValidateOutput(output.Data, stateSchema))
}

func TestProvider_Execute_StateSave_PullRequest_ReusesOpen(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	f.openPRs = []map[string]any{
		{"number": 12, "url": "https://github.com/test-org/state-repo/pull/12", "isCrossRepository": false},
	}
	output, err := f.saveState(t, map[string]any{
		"pull_request": map[string]any{"title": "intent: hello sandbox", "base": "main"},
	})

	require.NoError(t, err)
	result := output.Data.(map[string]any)
	assert.Equal(t, prTestCommitOID, result["commit_oid"])
	assert.Equal(t, map[string]any{
		"number": 12, "url": "https://github.com/test-org/state-repo/pull/12", "created": false,
	}, result["pull_request"])

	// The state was still committed, but no second pull request was opened.
	assert.Equal(t, []string{"branch-head", "commit", "find-pr"}, f.callLog())
}

// headRefName matches on the branch name alone, so the lookup also returns pull
// requests from forks whose branch shares the name. They must not be mistaken
// for the state branch's own pull request.
func TestProvider_Execute_StateSave_PullRequest_IgnoresForkPullRequests(t *testing.T) {
	t.Parallel()

	const forkURL = "https://github.com/test-org/state-repo/pull/99"
	const ownURL = "https://github.com/test-org/state-repo/pull/12"

	tests := []struct {
		name      string
		openPRs   []map[string]any
		want      map[string]any
		wantCalls []string
	}{
		{
			name: "own pull request listed after a fork's is reused",
			openPRs: []map[string]any{
				{"number": 99, "url": forkURL, "isCrossRepository": true},
				{"number": 12, "url": ownURL, "isCrossRepository": false},
			},
			want:      map[string]any{"number": 12, "url": ownURL, "created": false},
			wantCalls: []string{"branch-head", "commit", "find-pr"},
		},
		{
			name: "only a fork's pull request means a new one is opened",
			openPRs: []map[string]any{
				{"number": 99, "url": forkURL, "isCrossRepository": true},
			},
			want:      map[string]any{"number": 7, "url": prTestURL, "created": true},
			wantCalls: []string{"branch-head", "commit", "find-pr", "repo-id", "create-pr"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			f.openPRs = tc.openPRs
			output, err := f.saveState(t, map[string]any{
				"pull_request": map[string]any{"title": "intent: hello sandbox", "base": "main"},
			})

			require.NoError(t, err)
			assert.Equal(t, tc.want, output.Data.(map[string]any)["pull_request"])
			assert.Equal(t, tc.wantCalls, f.callLog())
		})
	}
}

func TestProvider_Execute_StateSave_NoPullRequest_NoPullRequestCalls(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	output, err := f.saveState(t, nil)

	require.NoError(t, err)
	result := output.Data.(map[string]any)
	assert.Equal(t, prTestCommitOID, result["commit_oid"])
	assert.NotContains(t, result, "pull_request")
	assert.Equal(t, []string{"branch-head", "commit"}, f.callLog())
}

func TestProvider_Execute_StateSave_PullRequest_BootstrapsBranch(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	f.branchExists = false
	output, err := f.saveState(t, map[string]any{
		"base_ref":     "main",
		"pull_request": map[string]any{"title": "intent: hello sandbox"},
	})

	require.NoError(t, err)
	result := output.Data.(map[string]any)
	assert.Equal(t, prTestCommitOID, result["commit_oid"])
	assert.Equal(t, map[string]any{"number": 7, "url": prTestURL, "created": true}, result["pull_request"])

	// Branch created from base_ref, committed to, then the pull request opened
	// against base_ref (the default base).
	assert.Equal(t, []string{
		"branch-head", "base-ref-oid", "repo-id", "create-branch", "commit", "find-pr", "repo-id", "create-pr",
	}, f.callLog())
	_, create := f.captured()
	assert.Equal(t, prTestBranch, create["headRefName"])
	assert.Equal(t, "main", create["baseRefName"])
}

func TestProvider_Execute_StateSave_PullRequest_BaseDefaulting(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		baseRef      string
		prBase       string
		wantBase     string
		wantRESTRepo bool // the default branch was fetched over REST
	}{
		{name: "explicit base wins over base_ref", baseRef: "develop", prBase: "release", wantBase: "release"},
		{name: "base_ref when base is omitted", baseRef: "develop", wantBase: "develop"},
		{name: "repository default branch when both are omitted", wantBase: "trunk", wantRESTRepo: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			f.defaultBranch = "trunk"
			pr := map[string]any{"title": "intent: hello sandbox"}
			if tc.prBase != "" {
				pr["base"] = tc.prBase
			}
			extra := map[string]any{"pull_request": pr}
			if tc.baseRef != "" {
				extra["base_ref"] = tc.baseRef
			}

			_, err := f.saveState(t, extra)

			require.NoError(t, err)
			lookup, create := f.captured()
			assert.Equal(t, tc.wantBase, lookup["base"])
			assert.Equal(t, tc.wantBase, create["baseRefName"])
			assert.Equal(t, tc.wantRESTRepo, slices.Contains(f.callLog(), "rest:get-repo"))
			// Unset optional fields are omitted so GitHub applies its defaults.
			assert.NotContains(t, create, "body")
			assert.NotContains(t, create, "draft")
		})
	}
}

func TestProvider_Execute_StateSave_PullRequest_BaseMustDifferFromBranch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		branch    string
		baseRef   string
		prBase    string
		wantCalls []string
	}{
		{name: "explicit base equals branch", branch: "intent/x", prBase: "intent/x", wantCalls: []string{}},
		{name: "base_ref equals branch", branch: "main", baseRef: "main", wantCalls: []string{}},
		{name: "default branch equals branch", branch: "main", wantCalls: []string{"rest:get-repo"}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			pr := map[string]any{"title": "intent: hello sandbox"}
			if tc.prBase != "" {
				pr["base"] = tc.prBase
			}
			extra := map[string]any{"branch": tc.branch, "pull_request": pr}
			if tc.baseRef != "" {
				extra["base_ref"] = tc.baseRef
			}

			_, err := f.saveState(t, extra)

			require.Error(t, err)
			assert.Contains(t, err.Error(), "pull_request.base must differ from branch")
			// Rejected before anything was written: at most the default branch lookup.
			assert.Equal(t, tc.wantCalls, f.callLog())
		})
	}
}

// base_ref may be a commit SHA because it only seeds the branch, but a pull
// request needs a branch: a SHA cannot be the defaulted base.
func TestProvider_Execute_StateSave_PullRequest_BaseRefCommitSHA(t *testing.T) {
	t.Parallel()

	const sha = "abcdef0123456789abcdef0123456789abcdef01"

	t.Run("rejected as the defaulted base before anything is written", func(t *testing.T) {
		t.Parallel()

		f := newStateSaveFake()
		_, err := f.saveState(t, map[string]any{
			"base_ref":     sha,
			"pull_request": map[string]any{"title": "intent: hello sandbox"},
		})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "is a commit SHA")
		assert.Contains(t, err.Error(), "pull_request.base")
		assert.Equal(t, []string{}, f.callLog())
	})

	t.Run("accepted when the pull request base is explicit", func(t *testing.T) {
		t.Parallel()

		f := newStateSaveFake()
		_, err := f.saveState(t, map[string]any{
			"base_ref":     sha,
			"pull_request": map[string]any{"title": "intent: hello sandbox", "base": "main"},
		})

		require.NoError(t, err)
		_, create := f.captured()
		assert.Equal(t, "main", create["baseRefName"])
	})
}

func TestProvider_Execute_StateSave_PullRequest_InvalidInput(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		pr      any
		wantErr string
	}{
		{name: "missing title", pr: map[string]any{"body": "b"}, wantErr: "'pull_request.title' is required"},
		{name: "empty block", pr: map[string]any{}, wantErr: "'pull_request.title' is required"},
		{name: "blank title", pr: map[string]any{"title": ""}, wantErr: "'pull_request.title' is required"},
		{name: "whitespace-only title", pr: map[string]any{"title": "  \t"}, wantErr: "'pull_request.title' is required"},
		{name: "not an object", pr: "yes", wantErr: "'pull_request' must be an object"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			_, err := f.saveState(t, map[string]any{"pull_request": tc.pr})

			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, []string{}, f.callLog(), "invalid input must be rejected before any API call")
		})
	}
}

// createPullRequest is shared with the create_pull_request action: body is sent
// only when non-empty and draft only when set, including an explicit false.
func TestProvider_CreatePullRequest_OptionalFields(t *testing.T) {
	t.Parallel()

	yes, no := true, false
	tests := []struct {
		name      string
		body      string
		draft     *bool
		wantBody  bool
		wantDraft any // nil: the draft key must be omitted
	}{
		{name: "neither set"},
		{name: "body only", body: "b", wantBody: true},
		{name: "draft true", draft: &yes, wantDraft: true},
		{name: "explicit draft false is still sent", draft: &no, wantDraft: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			p, baseURL := testProvider(t, f.serve)

			pr, err := p.createPullRequest(context.Background(), p.getClient(), baseURL, "test-org", "state-repo",
				"title", "head-branch", "main", tc.body, tc.draft)

			require.NoError(t, err)
			assert.EqualValues(t, 7, pr["number"])
			_, create := f.captured()
			assert.Equal(t, "title", create["title"])
			assert.Equal(t, "head-branch", create["headRefName"])
			assert.Equal(t, "main", create["baseRefName"])
			if tc.wantBody {
				assert.Equal(t, tc.body, create["body"])
			} else {
				assert.NotContains(t, create, "body")
			}
			if tc.wantDraft == nil {
				assert.NotContains(t, create, "draft")
			} else {
				assert.Equal(t, tc.wantDraft, create["draft"])
			}
		})
	}
}

func TestProvider_Execute_StateSave_PullRequest_FailureAfterCommit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		lookupErr string
		createErr string
		wantCalls []string
	}{
		{
			name:      "create mutation fails",
			createErr: "Head sha can't be blank",
			wantCalls: []string{"branch-head", "commit", "find-pr", "repo-id", "create-pr"},
		},
		{
			name:      "lookup fails",
			lookupErr: "Something went wrong while executing your query",
			wantCalls: []string{"branch-head", "commit", "find-pr"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := newStateSaveFake()
			f.lookupErr = tc.lookupErr
			f.createErr = tc.createErr
			output, err := f.saveState(t, map[string]any{
				"pull_request": map[string]any{"title": "intent: hello sandbox", "base": "main"},
			})

			require.Error(t, err)
			assert.Nil(t, output)
			// The commit is on the branch; the error says so and names it.
			assert.Contains(t, err.Error(), "committed "+prTestCommitOID)
			assert.Contains(t, err.Error(), fmt.Sprintf("branch %q", prTestBranch))
			assert.Contains(t, err.Error(), "failed to open pull request")
			assert.Contains(t, err.Error(), tc.lookupErr+tc.createErr, "the underlying GitHub error is preserved")
			assert.Equal(t, tc.wantCalls, f.callLog())
		})
	}
}

func TestProvider_Execute_StateSave_PullRequest_CommitFailureSkipsPullRequest(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	f.commitErr = "The expectedHeadOid didn't match the actual head oid"
	_, err := f.saveState(t, map[string]any{
		"pull_request": map[string]any{"title": "intent: hello sandbox", "base": "main"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "state save conflict")
	assert.Equal(t, []string{"branch-head", "commit"}, f.callLog(), "no pull request for a commit that failed")
}

func TestProvider_Execute_StateSave_PullRequest_EmptyRepoUnsupported(t *testing.T) {
	t.Parallel()

	f := newStateSaveFake()
	f.branchExists = false
	f.emptyRepo = true
	_, err := f.saveState(t, map[string]any{
		"pull_request": map[string]any{"title": "intent: hello sandbox"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "pull_request is not supported when initializing an empty repository")
	calls := f.callLog()
	assert.NotContains(t, calls, "rest:put-contents", "the empty repository must not be seeded")
	assert.Equal(t, []string{"rest:get-repo", "branch-head", "default-branch-oid"}, calls)
}

func TestStateSave_PullRequestSchema(t *testing.T) {
	t.Parallel()

	desc := newProvider().Descriptor()
	// scafctl rejects top-level input keys that the descriptor schema does not
	// declare, so pull_request must be a declared property.
	require.Contains(t, desc.Schema.Properties, "pull_request")

	withPR := func(pr any) map[string]any {
		return map[string]any{
			"operation":    "state_save",
			"owner":        "test-org",
			"repo":         "state-repo",
			"path":         "intent/state.json",
			"branch":       prTestBranch,
			"data":         map[string]any{"k": "v"},
			"pull_request": pr,
		}
	}

	tests := []struct {
		name    string
		pr      any
		wantErr string
	}{
		{name: "all fields", pr: map[string]any{"title": "t", "body": "b", "base": "main", "draft": true}},
		{name: "title only", pr: map[string]any{"title": "t"}},
		{name: "missing title", pr: map[string]any{"body": "b"}, wantErr: "title"},
		{name: "draft is not a boolean", pr: map[string]any{"title": "t", "draft": "yes"}, wantErr: "draft"},
		{name: "title too long", pr: map[string]any{"title": strings.Repeat("x", 1001)}, wantErr: "title"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := sdkprovider.NewSchemaValidator().ValidateInputs(withPR(tc.pr), desc.Schema)
			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
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
