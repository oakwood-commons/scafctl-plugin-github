// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// viewerPermissionResponse returns a mock GraphQL response for the waitForWriteAccess query.
func viewerPermissionResponse() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"viewerPermission": "ADMIN",
			},
		},
	}
}

// ─── Create Repository Tests ─────────────────────────────────────────────────

func TestProvider_Execute_CreateRepo(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle viewerPermission query (waitForWriteAccess)
		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
			return
		}

		if n == 1 {
			// resolveOwnerID
			assert.Contains(t, req.Query, "repositoryOwner")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repositoryOwner": map[string]any{"id": "O_test123"},
				},
			})
			return
		}
		// createRepository
		assert.Contains(t, req.Query, "createRepository")
		input := req.Variables["input"].(map[string]any)
		assert.Equal(t, "my-new-repo", input["name"])
		assert.Equal(t, "PRIVATE", input["visibility"])
		assert.Equal(t, "A test repo", input["description"])
		assert.Equal(t, "O_test123", input["ownerId"])
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"data": map[string]any{
				"createRepository": map[string]any{
					"repository": map[string]any{
						"id":            "R_123",
						"name":          "my-new-repo",
						"nameWithOwner": "test-org/my-new-repo",
						"url":           "https://github.com/test-org/my-new-repo",
						"isPrivate":     true,
						"createdAt":     "2026-01-01T00:00:00Z",
					},
				},
			},
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation":   "create_repo",
		"owner":       "test-org",
		"repo":        "my-new-repo",
		"description": "A test repo",
		"visibility":  "private",
		"api_base":    baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "my-new-repo", result["name"])
	assert.Equal(t, true, result["isPrivate"])
}

func TestProvider_Execute_CreateRepo_DefaultVisibility(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		func(_ string, vars map[string]any) {
			input := vars["input"].(map[string]any)
			assert.Equal(t, "PRIVATE", input["visibility"])
		},
		map[string]any{
			"data": map[string]any{
				"createRepository": map[string]any{
					"repository": map[string]any{
						"id":            "R_456",
						"name":          "default-repo",
						"nameWithOwner": "testuser/default-repo",
					},
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"repo":      "default-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
}

func TestProvider_Execute_CreateRepo_InternalVisibility(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		func(_ string, vars map[string]any) {
			input := vars["input"].(map[string]any)
			assert.Equal(t, "INTERNAL", input["visibility"])
		},
		map[string]any{
			"data": map[string]any{
				"createRepository": map[string]any{
					"repository": map[string]any{
						"id":            "R_789",
						"name":          "internal-repo",
						"nameWithOwner": "test-org/internal-repo",
					},
				},
			},
		},
	))

	output, err := p.execute(context.Background(), map[string]any{
		"operation":  "create_repo",
		"repo":       "internal-repo",
		"visibility": "internal",
		"api_base":   baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
}

func TestProvider_Execute_CreateRepo_MissingRepo(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "test-org",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "'repo' is required")
}

func TestProvider_Execute_CreateRepo_MissingNameWithOwner(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle viewerPermission query (waitForWriteAccess)
		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
			return
		}

		if strings.Contains(req.Query, "repositoryOwner") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repositoryOwner": map[string]any{"id": "O_test123"},
				},
			})
			return
		}

		// createRepository — deliberately omit nameWithOwner
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"data": map[string]any{
				"createRepository": map[string]any{
					"repository": map[string]any{
						"id":   "R_999",
						"name": "no-nwo-repo",
					},
				},
			},
		})
	})

	_, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "test-org",
		"repo":      "no-nwo-repo",
		"api_base":  baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing nameWithOwner")
}

func TestExtractNameWithOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   *sdkprovider.Output
		expected string
	}{
		{
			name:     "nil output",
			output:   nil,
			expected: "",
		},
		{
			name: "valid output",
			output: &sdkprovider.Output{
				Data: map[string]any{
					"result": map[string]any{
						"nameWithOwner": "my-org/my-repo",
					},
				},
			},
			expected: "my-org/my-repo",
		},
		{
			name: "missing nameWithOwner",
			output: &sdkprovider.Output{
				Data: map[string]any{
					"result": map[string]any{
						"name": "my-repo",
					},
				},
			},
			expected: "",
		},
		{
			name: "missing result",
			output: &sdkprovider.Output{
				Data: map[string]any{},
			},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, extractNameWithOwner(tc.output))
		})
	}
}

func TestExtractRESTOwner(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		output   *sdkprovider.Output
		expected string
	}{
		{
			name:     "nil output",
			output:   nil,
			expected: "",
		},
		{
			name: "valid full_name",
			output: &sdkprovider.Output{
				Data: map[string]any{
					"result": map[string]any{
						"full_name": "actual-user/my-repo",
					},
				},
			},
			expected: "actual-user",
		},
		{
			name: "missing full_name",
			output: &sdkprovider.Output{
				Data: map[string]any{
					"result": map[string]any{
						"name": "my-repo",
					},
				},
			},
			expected: "",
		},
		{
			name: "missing result",
			output: &sdkprovider.Output{
				Data: map[string]any{},
			},
			expected: "",
		},
		{
			name: "full_name without slash",
			output: &sdkprovider.Output{
				Data: map[string]any{
					"result": map[string]any{
						"full_name": "noslash",
					},
				},
			},
			expected: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tc.expected, extractRESTOwner(tc.output))
		})
	}
}

func TestProvider_Execute_CreateRepo_WithOwner(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle viewerPermission query (waitForWriteAccess)
		if strings.Contains(req.Query, "viewerPermission") {
			json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
			return
		}

		if n == 1 {
			// First call: resolveOwnerID
			assert.Contains(t, req.Query, "repositoryOwner")
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repositoryOwner": map[string]any{
						"id": "O_org123",
					},
				},
			})
			return
		}
		// Second call: createRepository
		assert.Contains(t, req.Query, "createRepository")
		input := req.Variables["input"].(map[string]any)
		assert.Equal(t, "O_org123", input["ownerId"])
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"data": map[string]any{
				"createRepository": map[string]any{
					"repository": map[string]any{
						"id":            "R_789",
						"name":          "org-repo",
						"nameWithOwner": "my-org/org-repo",
					},
				},
			},
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "my-org",
		"repo":      "org-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "org-repo", result["name"])
}

func TestProvider_Execute_CreateRepo_GraphQLForbidden_FallsBackToREST(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle GraphQL requests
		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			var req graphqlRequest
			json.Unmarshal(body, &req) //nolint:errcheck,gosec

			// Handle viewerPermission query (waitForWriteAccess)
			if strings.Contains(req.Query, "viewerPermission") {
				json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
				return
			}

			// GraphQL createRepository returns FORBIDDEN (EMU user)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"errors": []any{
					map[string]any{
						"message": "As an Enterprise Managed User, you cannot access this content",
						"type":    "FORBIDDEN",
					},
				},
			})
			return
		}

		// REST fallback: POST /orgs/{org}/repos
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/orgs/emu-org/repos", r.URL.Path)

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		json.Unmarshal(body, &reqBody) //nolint:errcheck,gosec
		assert.Equal(t, "emu-repo", reqBody["name"])
		assert.Equal(t, true, reqBody["auto_init"])

		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"id":        float64(200),
			"name":      "emu-repo",
			"full_name": "emu-org/emu-repo",
			"private":   false,
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "emu-org",
		"repo":      "emu-repo",
		"auto_init": true,
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "emu-repo", result["name"])
	assert.Equal(t, int32(3), callCount.Load()) // GraphQL FORBIDDEN + REST create + viewerPermission
}

func TestProvider_Execute_CreateRepo_AutoInit_UserRepo(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle GraphQL requests
		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			var req graphqlRequest
			json.Unmarshal(body, &req) //nolint:errcheck,gosec

			// Handle viewerPermission query (waitForWriteAccess)
			if strings.Contains(req.Query, "viewerPermission") {
				json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
				return
			}

			// GraphQL createRepository
			assert.Contains(t, req.Query, "createRepository")
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "auto-init-repo", input["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRepository": map[string]any{
						"repository": map[string]any{
							"id":            "R_100",
							"name":          "auto-init-repo",
							"nameWithOwner": "testuser/auto-init-repo",
						},
					},
				},
			})
			return
		}
		// REST Contents API to create README.md
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/repos/testuser/auto-init-repo/contents/README.md", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{"name": "README.md"}}) //nolint:errcheck,gosec
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation":   "create_repo",
		"repo":        "auto-init-repo",
		"description": "An auto-init repo",
		"auto_init":   true,
		"api_base":    baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "auto-init-repo", result["name"])
	assert.Equal(t, int32(3), callCount.Load()) // createRepo + viewerPermission + README
}

func TestProvider_Execute_CreateRepo_AutoInit_OrgRepo(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle GraphQL requests
		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			var req graphqlRequest
			json.Unmarshal(body, &req) //nolint:errcheck,gosec

			// Handle viewerPermission query (waitForWriteAccess)
			if strings.Contains(req.Query, "viewerPermission") {
				json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
				return
			}

			if strings.Contains(req.Query, "repositoryOwner") {
				// resolveOwnerID
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"data": map[string]any{
						"repositoryOwner": map[string]any{"id": "O_org456"},
					},
				})
				return
			}

			// GraphQL createRepository
			assert.Contains(t, req.Query, "createRepository")
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "org-auto-repo", input["name"])
			assert.Equal(t, "O_org456", input["ownerId"])
			assert.Equal(t, "PRIVATE", input["visibility"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRepository": map[string]any{
						"repository": map[string]any{
							"id":            "R_101",
							"name":          "org-auto-repo",
							"nameWithOwner": "my-org/org-auto-repo",
						},
					},
				},
			})
			return
		}
		// REST Contents API to create README.md
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/repos/my-org/org-auto-repo/contents/README.md", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{"name": "README.md"}}) //nolint:errcheck,gosec
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation":  "create_repo",
		"owner":      "my-org",
		"repo":       "org-auto-repo",
		"visibility": "private",
		"auto_init":  true,
		"api_base":   baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "org-auto-repo", result["name"])
	assert.Equal(t, int32(4), callCount.Load()) // resolveOwnerID + createRepo + viewerPermission + README
}

// ─── Wait for Write Access Tests ─────────────────────────────────────────────

func TestProvider_WaitForWriteAccess_ImmediateAdmin(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
	})

	client := p.getClient()
	err := p.waitForWriteAccess(context.Background(), client, baseURL, "test-org", "test-repo")
	require.NoError(t, err)
	assert.Equal(t, int32(1), callCount.Load())
}

func TestProvider_WaitForWriteAccess_EventualWrite(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			// First two calls: READ only
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"viewerPermission": "READ"},
				},
			})
			return
		}
		// Third call: WRITE
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"data": map[string]any{
				"repository": map[string]any{"viewerPermission": "WRITE"},
			},
		})
	})

	client := p.getClient()
	err := p.waitForWriteAccess(context.Background(), client, baseURL, "test-org", "test-repo")
	require.NoError(t, err)
	assert.Equal(t, int32(3), callCount.Load())
}

func TestProvider_WaitForWriteAccess_ContextCancelled(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"data": map[string]any{
				"repository": map[string]any{"viewerPermission": "READ"},
			},
		})
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	client := p.getClient()
	err := p.waitForWriteAccess(ctx, client, baseURL, "test-org", "test-repo")
	require.Error(t, err)
}

// ─── Create Ruleset Tests ────────────────────────────────────────────────────

func TestProvider_Execute_CreateRuleset_BranchProtection(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/test-org/test-repo/rulesets", r.URL.Path)

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		json.Unmarshal(body, &reqBody) //nolint:errcheck,gosec

		assert.Equal(t, "main branch protection", reqBody["name"])
		assert.Equal(t, "branch", reqBody["target"])
		assert.Equal(t, "active", reqBody["enforcement"])

		conditions := reqBody["conditions"].(map[string]any)
		refName := conditions["ref_name"].(map[string]any)
		includes := refName["include"].([]any)
		assert.Contains(t, includes, "refs/heads/main")

		rules := reqBody["rules"].([]any)
		assert.GreaterOrEqual(t, len(rules), 3)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"id":          float64(1),
			"name":        "main branch protection",
			"target":      "branch",
			"enforcement": "active",
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation":                       "create_ruleset",
		"owner":                           "test-org",
		"repo":                            "test-repo",
		"ruleset_name":                    "main branch protection",
		"target":                          "branch",
		"enforcement":                     "active",
		"include_refs":                    []any{"refs/heads/main"},
		"required_status_checks_contexts": []any{"test", "lint"},
		"required_approving_review_count": float64(1),
		"required_linear_history":         true,
		"requires_commit_signatures":      true,
		"allow_force_pushes":              false,
		"allow_deletions":                 false,
		"api_base":                        baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "main branch protection", result["name"])
}

func TestProvider_Execute_CreateRuleset_TagProtection(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/repos/test-org/test-repo/rulesets", r.URL.Path)

		body, _ := io.ReadAll(r.Body)
		var reqBody map[string]any
		json.Unmarshal(body, &reqBody) //nolint:errcheck,gosec

		assert.Equal(t, "tag", reqBody["target"])
		conditions := reqBody["conditions"].(map[string]any)
		refName := conditions["ref_name"].(map[string]any)
		includes := refName["include"].([]any)
		assert.Contains(t, includes, "refs/tags/v*")

		rules := reqBody["rules"].([]any)
		ruleTypes := make([]string, 0, len(rules))
		for _, r := range rules {
			rm := r.(map[string]any)
			ruleTypes = append(ruleTypes, rm["type"].(string))
		}
		assert.Contains(t, ruleTypes, "deletion")
		assert.Contains(t, ruleTypes, "non_fast_forward")

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"id":          float64(2),
			"name":        "version tag protection",
			"target":      "tag",
			"enforcement": "active",
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation":          "create_ruleset",
		"owner":              "test-org",
		"repo":               "test-repo",
		"ruleset_name":       "version tag protection",
		"target":             "tag",
		"include_refs":       []any{"refs/tags/v*"},
		"allow_force_pushes": false,
		"allow_deletions":    false,
		"api_base":           baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "tag", result["target"])
}

func TestProvider_Execute_CreateRuleset_MissingName(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation":    "create_ruleset",
		"owner":        "test-org",
		"repo":         "test-repo",
		"include_refs": []any{"refs/heads/main"},
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "'ruleset_name' is required")
}

func TestProvider_Execute_CreateRuleset_MissingIncludeRefs(t *testing.T) {
	t.Parallel()

	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation":    "create_ruleset",
		"owner":        "test-org",
		"repo":         "test-repo",
		"ruleset_name": "test",
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "'include_refs' is required")
}

func TestBuildRulesetRules_AllRules(t *testing.T) {
	t.Parallel()

	inputs := map[string]any{
		"required_status_checks_contexts": []any{"test", "lint"},
		"required_approving_review_count": float64(2),
		"requires_commit_signatures":      true,
		"required_linear_history":         true,
		"allow_force_pushes":              false,
		"allow_deletions":                 false,
	}

	rules := buildRulesetRules(inputs)
	assert.Len(t, rules, 6)

	ruleTypes := make([]string, 0, len(rules))
	for _, r := range rules {
		ruleTypes = append(ruleTypes, r["type"].(string))
	}
	assert.Contains(t, ruleTypes, "required_status_checks")
	assert.Contains(t, ruleTypes, "pull_request")
	assert.Contains(t, ruleTypes, "required_signatures")
	assert.Contains(t, ruleTypes, "required_linear_history")
	assert.Contains(t, ruleTypes, "non_fast_forward")
	assert.Contains(t, ruleTypes, "deletion")
}

func TestBuildRulesetRules_Empty(t *testing.T) {
	t.Parallel()

	rules := buildRulesetRules(map[string]any{})
	assert.Empty(t, rules)
}

func TestBuildRulesetRules_AllowForcePush_True(t *testing.T) {
	t.Parallel()

	// When allow_force_pushes is true, non_fast_forward rule should NOT be added
	rules := buildRulesetRules(map[string]any{
		"allow_force_pushes": true,
	})
	for _, r := range rules {
		assert.NotEqual(t, "non_fast_forward", r["type"])
	}
}

// ─── Enable Vulnerability Alerts Tests ───────────────────────────────────────

func TestProvider_Execute_EnableVulnerabilityAlerts(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/repos/test-org/test-repo/vulnerability-alerts", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "enable_vulnerability_alerts",
		"owner":     "test-org",
		"repo":      "test-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, true, result["enabled"])
}

// ─── Enable Automated Security Fixes Tests ───────────────────────────────────

func TestProvider_Execute_EnableAutomatedSecurityFixes(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/repos/test-org/test-repo/automated-security-fixes", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "enable_automated_security_fixes",
		"owner":     "test-org",
		"repo":      "test-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, true, result["enabled"])
}

// ─── REST User Fallback Tests ────────────────────────────────────────────────

func TestProvider_Execute_CreateRepo_RESTFallback_UserRepo(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")

		// Handle GraphQL requests
		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			var req graphqlRequest
			json.Unmarshal(body, &req) //nolint:errcheck,gosec

			// Handle viewerPermission query (waitForWriteAccess)
			if strings.Contains(req.Query, "viewerPermission") {
				json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
				return
			}

			// GraphQL createRepository returns FORBIDDEN (EMU)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"errors": []any{
					map[string]any{
						"message": "forbidden",
						"type":    "FORBIDDEN",
					},
				},
			})
			return
		}

		// REST: org endpoint returns 404 (owner is a user, not an org)
		if r.URL.Path == "/orgs/my-user/repos" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}) //nolint:errcheck,gosec
			return
		}

		// REST: user endpoint succeeds
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "/user/repos", r.URL.Path)
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"id":        float64(300),
			"name":      "user-repo",
			"full_name": "my-user/user-repo",
			"private":   false,
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "my-user",
		"repo":      "user-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "user-repo", result["name"])
}

func TestProvider_Execute_CreateRepo_RESTFallback_OwnerDerivedFromResponse(t *testing.T) {
	t.Parallel()

	// Verifies that waitForWriteAccess uses the canonical owner from the
	// REST response (full_name), not the caller-provided owner.
	var mu sync.Mutex
	var permOwner string
	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/graphql" {
			body, _ := io.ReadAll(r.Body)
			var req graphqlRequest
			json.Unmarshal(body, &req) //nolint:errcheck,gosec

			if strings.Contains(req.Query, "viewerPermission") {
				// Capture the owner used in the permission check
				mu.Lock()
				permOwner, _ = req.Variables["owner"].(string)
				mu.Unlock()
				json.NewEncoder(w).Encode(viewerPermissionResponse()) //nolint:errcheck,gosec
				return
			}

			// GraphQL createRepository returns FORBIDDEN (EMU)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"errors": []any{
					map[string]any{"message": "forbidden", "type": "FORBIDDEN"},
				},
			})
			return
		}

		// REST: org endpoint returns 404
		if r.URL.Path == "/orgs/wrong-owner/repos" {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}) //nolint:errcheck,gosec
			return
		}

		// REST: user endpoint succeeds — actual owner is "actual-user"
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
			"id":        float64(400),
			"name":      "my-repo",
			"full_name": "actual-user/my-repo",
			"private":   false,
		})
	})

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_repo",
		"owner":     "wrong-owner",
		"repo":      "my-repo",
		"api_base":  baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	// waitForWriteAccess should have polled "actual-user", not "wrong-owner"
	mu.Lock()
	assert.Equal(t, "actual-user", permOwner)
	mu.Unlock()
}

// ─── Dry Run Tests ───────────────────────────────────────────────────────────

func TestProvider_Execute_CreateRepo_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation": "create_repo",
		"owner":     "test-org",
		"repo":      "test-repo",
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
}

func TestProvider_Execute_CreateRuleset_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation":    "create_ruleset",
		"owner":        "test-org",
		"repo":         "test-repo",
		"ruleset_name": "test",
		"include_refs": []any{"refs/heads/main"},
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
}

// ─── Init Repo With README Tests ─────────────────────────────────────────────

func TestProvider_InitRepoWithReadme_Success(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPut, r.Method)
		assert.Equal(t, "/repos/my-org/my-repo/contents/README.md", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{"name": "README.md"}}) //nolint:errcheck,gosec
	})

	client := p.getClient()
	err := p.initRepoWithReadme(context.Background(), client, baseURL, "my-org/my-repo")
	require.NoError(t, err)
}

func TestProvider_InitRepoWithReadme_RetryOn404(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		n := callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n < 3 {
			w.WriteHeader(http.StatusNotFound)
			json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}) //nolint:errcheck,gosec
			return
		}
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"content": map[string]any{"name": "README.md"}}) //nolint:errcheck,gosec
	})

	client := p.getClient()
	err := p.initRepoWithReadme(context.Background(), client, baseURL, "my-org/my-repo")
	require.NoError(t, err)
	assert.Equal(t, int32(3), callCount.Load())
}

func TestProvider_InitRepoWithReadme_ContextCancelled(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"}) //nolint:errcheck,gosec
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	client := p.getClient()
	err := p.initRepoWithReadme(ctx, client, baseURL, "my-org/my-repo")
	require.Error(t, err)
}

func TestProvider_InitRepoWithReadme_NonRetryableError(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
		callCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]any{"message": "Forbidden"}) //nolint:errcheck,gosec
	})

	client := p.getClient()
	err := p.initRepoWithReadme(context.Background(), client, baseURL, "my-org/my-repo")
	require.Error(t, err)
	assert.Equal(t, int32(1), callCount.Load(), "should not retry on non-404 errors")
}

// ─── RESTError Tests ─────────────────────────────────────────────────────────

func TestRESTError(t *testing.T) {
	t.Parallel()

	err := &restError{StatusCode: 404, Message: "Not Found"}
	assert.Equal(t, "GitHub API error (HTTP 404): Not Found", err.Error())
}

// ─── Benchmarks ──────────────────────────────────────────────────────────────

func BenchmarkBuildRulesetRules(b *testing.B) {
	inputs := map[string]any{
		"required_status_checks_contexts": []any{"test", "lint", "build"},
		"required_approving_review_count": float64(2),
		"requires_commit_signatures":      true,
		"required_linear_history":         true,
		"allow_force_pushes":              false,
		"allow_deletions":                 false,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		buildRulesetRules(inputs)
	}
}

func BenchmarkIsGraphQLForbidden(b *testing.B) {
	forbidden := &GraphQLError{Errors: []graphqlError{{Message: "forbidden", Type: "FORBIDDEN"}}}
	notForbidden := &GraphQLError{Errors: []graphqlError{{Message: "not found", Type: "NOT_FOUND"}}}

	b.Run("forbidden", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			isGraphQLForbidden(forbidden)
		}
	})

	b.Run("not_forbidden", func(b *testing.B) {
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			isGraphQLForbidden(notForbidden)
		}
	})
}

func BenchmarkProvider_Execute_CreateRepo_DryRun(b *testing.B) {
	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "create_repo",
		"owner":     "test-org",
		"repo":      "bench-repo",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

func BenchmarkProvider_Execute_CreateRuleset_DryRun(b *testing.B) {
	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation":    "create_ruleset",
		"owner":        "test-org",
		"repo":         "bench-repo",
		"ruleset_name": "bench protection",
		"include_refs": []any{"refs/heads/main"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

// ─── Create Fork PR Tests ───────────────────────────────────────────────────

func TestProvider_Execute_CreateForkPR_HappyPath(t *testing.T) {
	t.Parallel()

	var callCount atomic.Int32
	var mu sync.Mutex
	var requests []string

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		// REST requests
		if r.URL.Path != "/graphql" {
			n := callCount.Add(1)
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.Path)
			mu.Unlock()

			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				// Step 2: Fork repo
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/my-repo",
					"html_url":       "https://github.com/fork-org/my-repo",
					"default_branch": "main",
					"node_id":        "R_fork123",
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				// Step 4: Sync fork
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"message":    "Successfully fetched and fast-forwarded from upstream main.",
					"merge_type": "fast-forward",
				})
			default:
				t.Errorf("unexpected REST call #%d: %s %s", n, r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		// GraphQL requests
		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		mu.Lock()
		requests = append(requests, "GRAPHQL:"+extractGQLOperation(req.Query))
		mu.Unlock()

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			// Step 1: Get upstream repo info
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream123",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// Steps 3/5: Get head OID
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createRef"):
			// Step 5: Create branch
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/feature-branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }") && !strings.Contains(req.Query, "defaultBranchRef"):
			// resolveRepoID for createRef
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"id": "R_fork123"},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			// Step 6: Commit
			input := req.Variables["input"].(map[string]any)
			branchInput := input["branch"].(map[string]any)
			assert.Equal(t, "fork-org/my-repo", branchInput["repositoryNameWithOwner"])
			assert.Equal(t, "feature-branch", branchInput["branchName"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":           "newcommit123456789012345678901234567890",
							"url":           "https://github.com/fork-org/my-repo/commit/newcommit",
							"committedDate": "2026-06-10T12:00:00Z",
							"message":       "feat: add files",
						},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			// Step 7: Create PR
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "R_upstream123", input["repositoryId"])
			assert.Equal(t, "R_fork123", input["headRepositoryId"])
			assert.Equal(t, "feature-branch", input["headRefName"])
			assert.Equal(t, "main", input["baseRefName"])
			assert.Equal(t, "feat: add files", input["title"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{
							"id":          "PR_123",
							"number":      1,
							"title":       "feat: add files",
							"url":         "https://github.com/test-org/my-repo/pull/1",
							"state":       "OPEN",
							"headRefName": "feature-branch",
							"baseRefName": "main",
							"isDraft":     false,
						},
					},
				},
			})
		default:
			t.Errorf("unexpected GraphQL query: %s", req.Query[:min(100, len(req.Query))])
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	p.forkReadyMaxAttempts = 2
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_fork_pr",
		"owner":     "test-org",
		"repo":      "my-repo",
		"fork_org":  "fork-org",
		"branch":    "feature-branch",
		"message":   "feat: add files",
		"additions": []any{
			map[string]any{"path": "hello.txt", "content": "hello world"},
		},
		"api_base": baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	assert.Equal(t, "create_fork_pr", data["operation"])

	result := data["result"].(map[string]any)
	fork := result["fork"].(map[string]any)
	assert.Equal(t, "fork-org/my-repo", fork["full_name"])

	commit := result["commit"].(map[string]any)
	assert.Equal(t, "newcommit123456789012345678901234567890", commit["oid"])

	pr := result["pull_request"].(map[string]any)
	assert.Equal(t, float64(1), pr["number"])
	assert.Equal(t, "https://github.com/test-org/my-repo/pull/1", pr["url"])
}

func TestProvider_Execute_CreateForkPR_ForkAlreadyExists(t *testing.T) {
	t.Parallel()

	var forkCallCount atomic.Int32

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				n := forkCallCount.Add(1)
				if n == 1 {
					// Fork already exists: 422
					w.WriteHeader(http.StatusUnprocessableEntity)
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"message": "Repository already exists",
					})
					return
				}
			case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repos/fork-org/my-repo"):
				// GET existing fork details
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/my-repo",
					"html_url":       "https://github.com/fork-org/my-repo",
					"default_branch": "main",
					"node_id":        "R_existing_fork",
					"fork":           true,
					"parent":         map[string]any{"full_name": "test-org/my-repo"},
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"id": "R_existing_fork"},
				},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/my-branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{
							"oid":     "newcommit123456789012345678901234567890",
							"url":     "https://github.com/fork-org/my-repo/commit/x",
							"message": "test commit",
						},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "R_existing_fork", input["headRepositoryId"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{
							"id":     "PR_2",
							"number": 2,
							"url":    "https://github.com/test-org/my-repo/pull/2",
						},
					},
				},
			})
		}
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_fork_pr",
		"owner":     "test-org",
		"repo":      "my-repo",
		"fork_org":  "fork-org",
		"branch":    "my-branch",
		"message":   "test commit",
		"additions": []any{
			map[string]any{"path": "file.txt", "content": "data"},
		},
		"api_base": baseURL,
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	fork := result["fork"].(map[string]any)
	assert.Equal(t, "fork-org/my-repo", fork["full_name"])
}

func TestProvider_Execute_CreateForkPR_ForkReadinessRetry(t *testing.T) {
	t.Parallel()

	var headOIDCalls atomic.Int32

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/my-repo",
					"default_branch": "main",
					"node_id":        "R_fork",
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			n := headOIDCalls.Add(1)
			if n == 1 {
				// First call: fork not ready yet
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"data": map[string]any{
						"repository": map[string]any{
							"ref": nil,
						},
					},
				})
				return
			}
			// Subsequent calls: fork is ready
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"id": "R_fork"},
				},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": "commit123456789012345678901234567890123"},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{"number": 1, "url": "https://github.com/test-org/my-repo/pull/1"},
					},
				},
			})
		}
	})

	p.forkReadyMaxAttempts = 3
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_fork_pr",
		"owner":     "test-org",
		"repo":      "my-repo",
		"fork_org":  "fork-org",
		"branch":    "branch",
		"message":   "test",
		"additions": []any{
			map[string]any{"path": "f.txt", "content": "c"},
		},
		"api_base": baseURL,
	})

	require.NoError(t, err)
	assert.GreaterOrEqual(t, int(headOIDCalls.Load()), 2, "should have retried get_head_oid")
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
}

func TestProvider_Execute_CreateForkPR_ValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		inputs map[string]any
		errMsg string
	}{
		{
			name: "missing fork_org",
			inputs: map[string]any{
				"operation": "create_fork_pr",
				"owner":     "org",
				"repo":      "repo",
				"branch":    "b",
				"message":   "m",
				"additions": []any{map[string]any{"path": "f", "content": "c"}},
			},
			errMsg: "fork_org",
		},
		{
			name: "missing branch",
			inputs: map[string]any{
				"operation": "create_fork_pr",
				"owner":     "org",
				"repo":      "repo",
				"fork_org":  "fork",
				"message":   "m",
				"additions": []any{map[string]any{"path": "f", "content": "c"}},
			},
			errMsg: "branch",
		},
		{
			name: "missing message",
			inputs: map[string]any{
				"operation": "create_fork_pr",
				"owner":     "org",
				"repo":      "repo",
				"fork_org":  "fork",
				"branch":    "b",
				"additions": []any{map[string]any{"path": "f", "content": "c"}},
			},
			errMsg: "message",
		},
		{
			name: "missing additions and deletions",
			inputs: map[string]any{
				"operation": "create_fork_pr",
				"owner":     "org",
				"repo":      "repo",
				"fork_org":  "fork",
				"branch":    "b",
				"message":   "m",
			},
			errMsg: "additions",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
			})
			tc.inputs["api_base"] = baseURL

			_, err := p.execute(context.Background(), tc.inputs)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.errMsg)
		})
	}
}

func TestProvider_Execute_CreateForkPR_DryRun(t *testing.T) {
	t.Parallel()

	p := newProvider()
	ctx := sdkprovider.WithDryRun(context.Background(), true)

	output, err := p.execute(ctx, map[string]any{
		"operation": "create_fork_pr",
		"owner":     "org",
		"repo":      "repo",
		"fork_org":  "fork",
		"branch":    "b",
		"message":   "m",
		"additions": []any{map[string]any{"path": "f", "content": "c"}},
	})

	require.NoError(t, err)
	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	assert.Equal(t, "create_fork_pr", data["operation"])
	result := data["result"].(map[string]any)
	assert.Equal(t, true, result["dry_run"])
}

func TestProvider_Execute_CreateForkPR_Force(t *testing.T) {
	t.Parallel()

	// The force-reset path (createRef -> already exists -> resolveRefID ->
	// deleteRef -> createRef) fans out over several fork-side calls, so it is
	// run both against a same-named fork and a renamed one to confirm each of
	// those calls follows the fork's name rather than the upstream's.
	tests := []struct {
		name         string
		forkRepoName string // "" -- omit the input entirely
		wantForkRepo string
	}{
		{name: "same name as upstream", forkRepoName: "", wantForkRepo: "my-repo"},
		{name: "renamed fork", forkRepoName: "renamed-repo", wantForkRepo: "renamed-repo"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			var branchDeleted atomic.Bool

			p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")

				if r.URL.Path != "/graphql" {
					switch {
					case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
						assert.Equal(t, "/repos/test-org/my-repo/forks", r.URL.Path)
						json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
							"name":           tc.wantForkRepo,
							"full_name":      "fork-org/" + tc.wantForkRepo,
							"default_branch": "main",
							"node_id":        "R_fork",
						})
					case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
						assert.Equal(t, "/repos/fork-org/"+tc.wantForkRepo+"/merge-upstream", r.URL.Path)
						json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
					default:
						t.Errorf("unexpected REST call: %s %s", r.Method, r.URL.Path)
						w.WriteHeader(http.StatusNotFound)
					}
					return
				}

				body, _ := io.ReadAll(r.Body)
				var req graphqlRequest
				json.Unmarshal(body, &req) //nolint:errcheck,gosec

				switch {
				case strings.Contains(req.Query, "defaultBranchRef"):
					assert.Equal(t, "my-repo", req.Variables["name"], "upstream lookup must use the upstream name")
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"repository": map[string]any{
								"id":               "R_upstream",
								"defaultBranchRef": map[string]any{"name": "main"},
							},
						},
					})
				case strings.Contains(req.Query, "ref(qualifiedName") && strings.Contains(req.Query, "target { oid }"):
					assert.Equal(t, tc.wantForkRepo, req.Variables["name"])
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"repository": map[string]any{
								"ref": map[string]any{
									"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
								},
							},
						},
					})
				case strings.Contains(req.Query, "ref(qualifiedName") && strings.Contains(req.Query, "{ id }"):
					// resolveRefID for deletion
					assert.Equal(t, tc.wantForkRepo, req.Variables["name"])
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"repository": map[string]any{
								"ref": map[string]any{"id": "REF_id_to_delete"},
							},
						},
					})
				case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }") && !strings.Contains(req.Query, "ref"):
					assert.Equal(t, tc.wantForkRepo, req.Variables["name"])
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"repository": map[string]any{"id": "R_fork"},
						},
					})
				case strings.Contains(req.Query, "deleteRef"):
					branchDeleted.Store(true)
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"deleteRef": map[string]any{"clientMutationId": "x"},
						},
					})
				case strings.Contains(req.Query, "createRef"):
					input := req.Variables["input"].(map[string]any)
					// After first createRef fails (already exists), force causes delete + recreate
					if branchDeleted.Load() {
						assert.Equal(t, "refs/heads/feature", input["name"])
					} else {
						// First createRef attempt: simulate "already exists"
						json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
							"errors": []map[string]any{
								{"message": "A ref named refs/heads/feature already exists", "type": "UNPROCESSABLE"},
							},
						})
						return
					}
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"createRef": map[string]any{
								"ref": map[string]any{
									"name":   "refs/heads/feature",
									"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
								},
							},
						},
					})
				case strings.Contains(req.Query, "createCommitOnBranch"):
					input := req.Variables["input"].(map[string]any)
					branchInput := input["branch"].(map[string]any)
					assert.Equal(t, "fork-org/"+tc.wantForkRepo, branchInput["repositoryNameWithOwner"])
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"createCommitOnBranch": map[string]any{
								"commit": map[string]any{"oid": "newoid12345678901234567890123456789012"},
							},
						},
					})
				case strings.Contains(req.Query, "createPullRequest"):
					json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
						"data": map[string]any{
							"createPullRequest": map[string]any{
								"pullRequest": map[string]any{"number": 3, "url": "https://github.com/org/repo/pull/3"},
							},
						},
					})
				}
			})

			p.forkReadyMaxAttempts = 1
			p.forkReadyBackoff = time.Millisecond

			inputs := map[string]any{
				"operation": "create_fork_pr",
				"owner":     "test-org",
				"repo":      "my-repo",
				"fork_org":  "fork-org",
				"branch":    "feature",
				"message":   "update",
				"force":     true,
				"additions": []any{
					map[string]any{"path": "file.go", "content": "package main"},
				},
				"api_base": baseURL,
			}
			if tc.forkRepoName != "" {
				inputs["fork_repo_name"] = tc.forkRepoName
			}

			output, err := p.execute(context.Background(), inputs)

			require.NoError(t, err)
			assert.True(t, branchDeleted.Load(), "branch should have been deleted for force-reset")
			data := output.Data.(map[string]any)
			assert.Equal(t, true, data["success"])
			result := data["result"].(map[string]any)
			assert.Equal(t, tc.wantForkRepo, result["fork_repo_name"])
		})
	}
}

func TestProvider_Execute_CreateForkPR_ForkRepoName(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forkReqBody map[string]any
	var restPaths []string
	var gqlRepoNames []string

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			mu.Lock()
			restPaths = append(restPaths, r.Method+" "+r.URL.Path)
			mu.Unlock()

			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				json.Unmarshal(body, &forkReqBody) //nolint:errcheck,gosec
				mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/renamed-repo",
					"html_url":       "https://github.com/fork-org/renamed-repo",
					"default_branch": "main",
					"node_id":        "R_renamed_fork",
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				t.Errorf("unexpected REST call: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		if name, ok := req.Variables["name"].(string); ok {
			mu.Lock()
			gqlRepoNames = append(gqlRepoNames, extractGQLOperation(req.Query)+":"+name)
			mu.Unlock()
		}

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			// Upstream lookup must still use the upstream repo name.
			assert.Equal(t, "test-org", req.Variables["owner"])
			assert.Equal(t, "my-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream123",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// All fork-side ref lookups must use the renamed fork.
			assert.Equal(t, "fork-org", req.Variables["owner"])
			assert.Equal(t, "renamed-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }") && !strings.Contains(req.Query, "defaultBranchRef"):
			// resolveRepoID for createRef -- must target the renamed fork.
			assert.Equal(t, "fork-org", req.Variables["owner"])
			assert.Equal(t, "renamed-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"id": "R_renamed_fork"},
				},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/feature-branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			branchInput := input["branch"].(map[string]any)
			assert.Equal(t, "fork-org/renamed-repo", branchInput["repositoryNameWithOwner"])
			assert.Equal(t, "feature-branch", branchInput["branchName"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": "newcommit123456789012345678901234567890"},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "R_upstream123", input["repositoryId"])
			assert.Equal(t, "R_renamed_fork", input["headRepositoryId"])
			assert.Equal(t, "feature-branch", input["headRefName"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{
							"number": 7,
							"url":    "https://github.com/test-org/my-repo/pull/7",
						},
					},
				},
			})
		default:
			t.Errorf("unexpected GraphQL query: %s", req.Query[:min(100, len(req.Query))])
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation":      "create_fork_pr",
		"owner":          "test-org",
		"repo":           "my-repo",
		"fork_org":       "fork-org",
		"fork_repo_name": "renamed-repo",
		"branch":         "feature-branch",
		"message":        "feat: add files",
		"additions": []any{
			map[string]any{"path": "hello.txt", "content": "hello world"},
		},
		"api_base": baseURL,
	})

	require.NoError(t, err)

	mu.Lock()
	gotForkBody := forkReqBody
	gotRESTPaths := slices.Clone(restPaths)
	gotGQLNames := slices.Clone(gqlRepoNames)
	mu.Unlock()

	// The fork request must ask GitHub to rename the fork, and must still be
	// POSTed against the upstream repo.
	assert.Equal(t, "renamed-repo", gotForkBody["name"])
	assert.Equal(t, "fork-org", gotForkBody["organization"])
	assert.Contains(t, gotRESTPaths, "POST /repos/test-org/my-repo/forks")

	// merge-upstream (fork side) must target the renamed fork.
	assert.Contains(t, gotRESTPaths, "POST /repos/fork-org/renamed-repo/merge-upstream")
	for _, pth := range gotRESTPaths {
		assert.NotContains(t, pth, "/repos/fork-org/my-repo",
			"fork-side REST call used the upstream repo name")
	}

	// No fork-side GraphQL call may reference the upstream name.
	for _, entry := range gotGQLNames {
		if strings.HasPrefix(entry, "getRepoInfo:") {
			continue // upstream lookup
		}
		assert.True(t, strings.HasSuffix(entry, ":renamed-repo"),
			"fork-side GraphQL call used the wrong repo name: %s", entry)
	}

	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "renamed-repo", result["fork_repo_name"])
	fork := result["fork"].(map[string]any)
	assert.Equal(t, "fork-org/renamed-repo", fork["full_name"])
	pr := result["pull_request"].(map[string]any)
	assert.Equal(t, float64(7), pr["number"])
}

func TestProvider_Execute_CreateForkPR_ForkRepoName_AlreadyExists(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var restPaths []string

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			mu.Lock()
			restPaths = append(restPaths, r.Method+" "+r.URL.Path)
			mu.Unlock()

			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"message": "Repository already exists",
				})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/fork-org/renamed-repo":
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/renamed-repo",
					"default_branch": "main",
					"node_id":        "R_existing_renamed",
					"fork":           true,
					"parent":         map[string]any{"full_name": "test-org/my-repo"},
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				t.Errorf("unexpected REST call: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			assert.Equal(t, "renamed-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }"):
			assert.Equal(t, "renamed-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{"id": "R_existing_renamed"},
				},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/my-branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			branchInput := input["branch"].(map[string]any)
			assert.Equal(t, "fork-org/renamed-repo", branchInput["repositoryNameWithOwner"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": "newcommit123456789012345678901234567890"},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "R_existing_renamed", input["headRepositoryId"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{"number": 8, "url": "https://github.com/test-org/my-repo/pull/8"},
					},
				},
			})
		}
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation":      "create_fork_pr",
		"owner":          "test-org",
		"repo":           "my-repo",
		"fork_org":       "fork-org",
		"fork_repo_name": "renamed-repo",
		"branch":         "my-branch",
		"message":        "test commit",
		"additions": []any{
			map[string]any{"path": "file.txt", "content": "data"},
		},
		"api_base": baseURL,
	})

	require.NoError(t, err)

	mu.Lock()
	gotRESTPaths := slices.Clone(restPaths)
	mu.Unlock()

	// The 422 fallback must fetch the fork by its own name, not the upstream name.
	assert.Contains(t, gotRESTPaths, "GET /repos/fork-org/renamed-repo")
	assert.NotContains(t, gotRESTPaths, "GET /repos/fork-org/my-repo")

	data := output.Data.(map[string]any)
	assert.Equal(t, true, data["success"])
	result := data["result"].(map[string]any)
	assert.Equal(t, "renamed-repo", result["fork_repo_name"])
}

func TestProvider_Execute_CreateForkPR_NoForkRepoName_OmitsNameField(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var forkReqBody map[string]any

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				body, _ := io.ReadAll(r.Body)
				mu.Lock()
				json.Unmarshal(body, &forkReqBody) //nolint:errcheck,gosec
				mu.Unlock()
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"full_name":      "fork-org/my-repo",
					"default_branch": "main",
					"node_id":        "R_fork",
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{"repository": map[string]any{"id": "R_fork"}},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/b",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": "commit123456789012345678901234567890123"},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{"number": 9, "url": "https://github.com/test-org/my-repo/pull/9"},
					},
				},
			})
		}
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation": "create_fork_pr",
		"owner":     "test-org",
		"repo":      "my-repo",
		"fork_org":  "fork-org",
		"branch":    "b",
		"message":   "m",
		"additions": []any{map[string]any{"path": "f.txt", "content": "c"}},
		"api_base":  baseURL,
	})

	require.NoError(t, err)

	mu.Lock()
	gotForkBody := forkReqBody
	mu.Unlock()

	// Without fork_repo_name the request body must not carry a "name" field.
	// The organization assertion proves the body was actually captured, so the
	// negative assertion below cannot pass vacuously on a nil map.
	require.NotNil(t, gotForkBody)
	assert.Equal(t, "fork-org", gotForkBody["organization"])
	assert.NotContains(t, gotForkBody, "name")

	data := output.Data.(map[string]any)
	result := data["result"].(map[string]any)
	assert.Equal(t, "my-repo", result["fork_repo_name"])
}

// TestProvider_Execute_CreateForkPR_ForkRepoName_ExistingForkKeepsItsName
// covers GitHub's actual fork semantics: the "name" field is only applied when
// the fork is created, so if a fork of the upstream already exists in the
// destination org under a different name, the API returns that existing
// repository unrenamed. The requested name must then be abandoned in favour of
// the real one, otherwise every fork-side call targets a repository that does
// not exist.
func TestProvider_Execute_CreateForkPR_ForkRepoName_ExistingForkKeepsItsName(t *testing.T) {
	t.Parallel()

	var mu sync.Mutex
	var restPaths []string

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			mu.Lock()
			restPaths = append(restPaths, r.Method+" "+r.URL.Path)
			mu.Unlock()

			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				// 202, but describing the pre-existing fork under its own
				// name -- the requested "renamed-repo" was ignored.
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"name":           "my-repo",
					"full_name":      "fork-org/my-repo",
					"default_branch": "main",
					"node_id":        "R_preexisting_fork",
				})
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/merge-upstream"):
				json.NewEncoder(w).Encode(map[string]any{"message": "ok"}) //nolint:errcheck,gosec
			default:
				t.Errorf("unexpected REST call: %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		switch {
		case strings.Contains(req.Query, "defaultBranchRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
		case strings.Contains(req.Query, "ref(qualifiedName"):
			// Must follow the fork's real name, not the requested one.
			assert.Equal(t, "fork-org", req.Variables["owner"])
			assert.Equal(t, "my-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"ref": map[string]any{
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "repository") && strings.Contains(req.Query, "{ id }"):
			assert.Equal(t, "my-repo", req.Variables["name"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{"repository": map[string]any{"id": "R_preexisting_fork"}},
			})
		case strings.Contains(req.Query, "createRef"):
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createRef": map[string]any{
						"ref": map[string]any{
							"name":   "refs/heads/feature-branch",
							"target": map[string]any{"oid": "abc123def456abc123def456abc123def456abc1"},
						},
					},
				},
			})
		case strings.Contains(req.Query, "createCommitOnBranch"):
			input := req.Variables["input"].(map[string]any)
			branchInput := input["branch"].(map[string]any)
			assert.Equal(t, "fork-org/my-repo", branchInput["repositoryNameWithOwner"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createCommitOnBranch": map[string]any{
						"commit": map[string]any{"oid": "newcommit123456789012345678901234567890"},
					},
				},
			})
		case strings.Contains(req.Query, "createPullRequest"):
			input := req.Variables["input"].(map[string]any)
			assert.Equal(t, "R_preexisting_fork", input["headRepositoryId"])
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"createPullRequest": map[string]any{
						"pullRequest": map[string]any{"number": 11, "url": "https://github.com/test-org/my-repo/pull/11"},
					},
				},
			})
		default:
			t.Errorf("unexpected GraphQL query: %s", req.Query[:min(100, len(req.Query))])
			w.WriteHeader(http.StatusBadRequest)
		}
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	output, err := p.execute(context.Background(), map[string]any{
		"operation":      "create_fork_pr",
		"owner":          "test-org",
		"repo":           "my-repo",
		"fork_org":       "fork-org",
		"fork_repo_name": "renamed-repo",
		"branch":         "feature-branch",
		"message":        "feat: add files",
		"additions":      []any{map[string]any{"path": "hello.txt", "content": "hello"}},
		"api_base":       baseURL,
	})

	// The operation must succeed against the existing fork rather than time
	// out waiting for a fork that was never created under the requested name.
	require.NoError(t, err)

	mu.Lock()
	gotRESTPaths := slices.Clone(restPaths)
	mu.Unlock()

	assert.Contains(t, gotRESTPaths, "POST /repos/fork-org/my-repo/merge-upstream")
	for _, pth := range gotRESTPaths {
		assert.NotContains(t, pth, "renamed-repo",
			"no call should target the fork name GitHub declined to use")
	}

	// The output must report the name the fork actually has, so callers do not
	// build URLs for a repository that does not exist.
	data := output.Data.(map[string]any)
	result := data["result"].(map[string]any)
	assert.Equal(t, "my-repo", result["fork_repo_name"])
}

// TestProvider_Execute_CreateForkPR_ForkRepoName_UnverifiableParent asserts the
// 422 already-exists fallback fails closed when it cannot confirm the target is
// a fork of the requested upstream. With a caller-supplied fork name the target
// need not be related to the upstream at all, so "it says it's a fork" is not
// sufficient evidence to commit into it.
func TestProvider_Execute_CreateForkPR_ForkRepoName_UnverifiableParent(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path != "/graphql" {
			switch {
			case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/forks"):
				w.WriteHeader(http.StatusUnprocessableEntity)
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"message": "Repository already exists",
				})
			case r.Method == http.MethodGet && r.URL.Path == "/repos/fork-org/renamed-repo":
				// A fork, but of something unknown: no "parent" to check.
				json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
					"name":           "renamed-repo",
					"full_name":      "fork-org/renamed-repo",
					"default_branch": "main",
					"node_id":        "R_unverified",
					"fork":           true,
				})
			default:
				w.WriteHeader(http.StatusNotFound)
			}
			return
		}

		body, _ := io.ReadAll(r.Body)
		var req graphqlRequest
		json.Unmarshal(body, &req) //nolint:errcheck,gosec

		if strings.Contains(req.Query, "defaultBranchRef") {
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck,gosec
				"data": map[string]any{
					"repository": map[string]any{
						"id":               "R_upstream",
						"defaultBranchRef": map[string]any{"name": "main"},
					},
				},
			})
			return
		}
		t.Errorf("no fork-side work should happen after an unverifiable parent: %s", req.Query[:min(80, len(req.Query))])
		w.WriteHeader(http.StatusBadRequest)
	})

	p.forkReadyMaxAttempts = 1
	p.forkReadyBackoff = time.Millisecond

	_, err := p.execute(context.Background(), map[string]any{
		"operation":      "create_fork_pr",
		"owner":          "test-org",
		"repo":           "my-repo",
		"fork_org":       "fork-org",
		"fork_repo_name": "renamed-repo",
		"branch":         "b",
		"message":        "m",
		"additions":      []any{map[string]any{"path": "f.txt", "content": "c"}},
		"api_base":       baseURL,
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "could not be verified")
	assert.Contains(t, err.Error(), "test-org/my-repo")
}

// extractGQLOperation returns a short label for the GraphQL query for logging.
func extractGQLOperation(query string) string {
	if strings.Contains(query, "createPullRequest") {
		return "createPullRequest"
	}
	if strings.Contains(query, "createCommitOnBranch") {
		return "createCommitOnBranch"
	}
	if strings.Contains(query, "createRef") {
		return "createRef"
	}
	if strings.Contains(query, "deleteRef") {
		return "deleteRef"
	}
	if strings.Contains(query, "defaultBranchRef") {
		return "getRepoInfo"
	}
	if strings.Contains(query, "ref(qualifiedName") {
		return "getHeadOID"
	}
	if strings.Contains(query, "{ id }") {
		return "resolveRepoID"
	}
	return "unknown"
}
