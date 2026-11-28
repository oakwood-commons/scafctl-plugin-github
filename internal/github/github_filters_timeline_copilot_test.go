// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testRoutedProvider routes any logical api_base (such as default https://api.github.com
// or https://ghe.example.com/api/v3) to a local httptest.Server without external network I/O.
func testRoutedProvider(t testing.TB, handler http.HandlerFunc) *Provider {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	targetURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	client := httpc.NewClient(&httpc.ClientConfig{
		EnableCache: false,
		RetryMax:    0,
		IPPolicy:    httpc.AllowAllPrivateIPs(),
		RequestHooks: []httpc.RequestHook{
			func(req *http.Request) error {
				req.Header.Set("X-Logical-Host", req.URL.Host)
				req.URL.Scheme = targetURL.Scheme
				req.URL.Host = targetURL.Host
				return nil
			},
		},
	})
	return newProvider(
		WithClient(client),
		WithRetryConfig(5, time.Millisecond, 15, time.Millisecond, 3, time.Millisecond),
	)
}

// ─── list_pull_requests head/base Filter Tests ───────────────────────────────

func TestProvider_Execute_ListPullRequests_HeadAndBaseFilters(t *testing.T) {
	t.Parallel()

	t.Run("passes head and base branch names to GraphQL", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, graphqlHandler(t,
			func(query string, vars map[string]any) {
				assert.Contains(t, query, "headRefName: $head")
				assert.Contains(t, query, "baseRefName: $base")
				assert.Equal(t, "feat/demo", vars["head"])
				assert.Equal(t, "main", vars["base"])
				assert.Equal(t, []any{"OPEN"}, vars["states"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequests": map[string]any{
							"nodes": []any{
								map[string]any{
									"number":      float64(12),
									"title":       "feat: demo",
									"state":       "OPEN",
									"headRefName": "feat/demo",
									"baseRefName": "main",
								},
							},
						},
					},
				},
			},
		))

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"state":     "open",
			"head":      "feat/demo",
			"base":      "main",
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		prs := out.Data.(map[string]any)["result"].([]any)
		require.Len(t, prs, 1)
		assert.Equal(t, float64(12), prs[0].(map[string]any)["number"])
	})

	t.Run("splits owner:branch head prefix and paginates before post-filtering fork owners", func(t *testing.T) {
		t.Parallel()

		var gqlPages atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			gqlPages.Add(1)
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Equal(t, "feat/demo", req.Variables["head"])
			assert.Equal(t, float64(defaultGraphQLPageSize), req.Variables["first"], "owner:branch must scan full pages before client-side owner filter")
			_, hasBase := req.Variables["base"]
			assert.False(t, hasBase)

			w.Header().Set("Content-Type", "application/json")
			after, _ := req.Variables["after"].(string)
			if after == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequests": map[string]any{
								"nodes": []any{
									map[string]any{
										"number":         float64(101),
										"headRefName":    "feat/demo",
										"headRepository": map[string]any{"owner": map[string]any{"login": "other-fork"}},
									},
									map[string]any{
										"number":         float64(103),
										"headRefName":    "feat/demo",
										"headRepository": nil,
									},
								},
								"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "pr_cur_1"},
							},
						},
					},
				})
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequests": map[string]any{
							"nodes": []any{
								map[string]any{
									"number":              float64(102),
									"headRefName":         "feat/demo",
									"headRepository":      nil,
									"headRepositoryOwner": map[string]any{"login": "OctoCat"},
								},
								map[string]any{
									"number":         float64(104),
									"headRefName":    "feat/demo",
									"headRepository": map[string]any{"owner": map[string]any{"login": "octocat"}},
								},
							},
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"head":      "octocat:feat/demo",
			"per_page":  1,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(2), gqlPages.Load())
		prs := out.Data.(map[string]any)["result"].([]any)
		require.Len(t, prs, 1, "must honor per_page after client-side headOwner filtering")
		assert.Equal(t, float64(102), prs[0].(map[string]any)["number"])
	})

	t.Run("accepts leading colon without owner post-filter", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, graphqlHandler(t,
			func(_ string, vars map[string]any) {
				assert.Equal(t, "feat/demo", vars["head"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequests": map[string]any{
							"nodes": []any{
								map[string]any{"number": float64(7), "headRefName": "feat/demo"},
							},
						},
					},
				},
			},
		))

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"head":      ":feat/demo",
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		prs := out.Data.(map[string]any)["result"].([]any)
		require.Len(t, prs, 1)
	})

	t.Run("rejects invalid head and base inputs", func(t *testing.T) {
		t.Parallel()

		p := newProvider()
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"head":      "octocat:",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "branch name cannot be empty")

		_, err = p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"head":      123,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "'head' must be a string")

		_, err = p.execute(context.Background(), map[string]any{
			"operation": "list_pull_requests",
			"owner":     "public-org",
			"repo":      "demo",
			"base":      false,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "'base' must be a string")
	})
}

// ─── list_issues Server-Side Filter Tests ────────────────────────────────────

func TestProvider_Execute_ListIssues_FiltersAndSchema(t *testing.T) {
	t.Parallel()

	t.Run("schema validates list_issues filter fields", func(t *testing.T) {
		t.Parallel()

		p := newProvider()
		schema := p.Descriptor().Schema
		sv := sdkprovider.NewSchemaValidator()

		// Milestone as string title
		require.NoError(t, sv.ValidateInputs(map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"state":     "open",
			"labels":    []any{"bug", "p1"},
			"assignee":  "octocat",
			"milestone": "v1.0",
			"creator":   "hubot",
			"mentioned": "monalisa",
			"since":     "2026-01-01T00:00:00Z",
			"sort":      "updated",
			"direction": "asc",
		}, schema))

		// Milestone as integer number
		require.NoError(t, sv.ValidateInputs(map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"milestone": 3,
		}, schema))

		// Invalid sort enum rejected by schema
		require.Error(t, sv.ValidateInputs(map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"sort":      "popularity",
		}, schema))

		// Zero milestone integer rejected by schema minimum
		require.Error(t, sv.ValidateInputs(map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"milestone": 0,
		}, schema))
	})

	t.Run("wires filterBy and orderBy to GraphQL", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, graphqlHandler(t,
			func(query string, vars map[string]any) {
				assert.Contains(t, query, "filterBy: $filterBy")
				assert.Contains(t, query, "orderBy: {field: $orderField, direction: $orderDirection}")
				assert.Equal(t, "UPDATED_AT", vars["orderField"])
				assert.Equal(t, "ASC", vars["orderDirection"])

				fb, ok := vars["filterBy"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, []any{"bug", "triaged"}, fb["labels"])
				assert.Equal(t, "octocat", fb["assignee"])
				assert.Equal(t, "hubot", fb["createdBy"])
				assert.Equal(t, "monalisa", fb["mentioned"])
				assert.Equal(t, "2026-01-01T00:00:00Z", fb["since"])
				assert.Equal(t, "4", fb["milestoneNumber"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{
							"nodes": []any{
								map[string]any{"number": float64(5), "title": "Filtered bug", "state": "OPEN"},
							},
						},
					},
				},
			},
		))

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"state":     "open",
			"labels":    []any{"bug", "triaged"},
			"assignee":  "octocat",
			"creator":   "hubot",
			"mentioned": "monalisa",
			"since":     "2026-01-01T00:00:00Z",
			"milestone": 4,
			"sort":      "updated",
			"direction": "asc",
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		issues := out.Data.(map[string]any)["result"].([]any)
		require.Len(t, issues, 1)
	})

	t.Run("supports none and wildcard sentinels for assignee and milestone", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, graphqlHandler(t,
			func(_ string, vars map[string]any) {
				fb := vars["filterBy"].(map[string]any)
				assigneeVal, hasAssignee := fb["assignee"]
				assert.True(t, hasAssignee, "assignee=none must include explicit null key")
				assert.Nil(t, assigneeVal)

				msVal, hasMilestone := fb["milestoneNumber"]
				assert.True(t, hasMilestone, "milestone=none must include explicit null key")
				assert.Nil(t, msVal)

				assert.Equal(t, "COMMENTS", vars["orderField"])
				assert.Equal(t, "DESC", vars["orderDirection"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{"nodes": []any{}},
					},
				},
			},
		))

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"assignee":  "none",
			"milestone": "NONE",
			"sort":      "comments",
			"api_base":  baseURL,
		})
		require.NoError(t, err)

		pWild, baseURLWild := testProvider(t, graphqlHandler(t,
			func(_ string, vars map[string]any) {
				fb := vars["filterBy"].(map[string]any)
				assert.Equal(t, "*", fb["assignee"])
				assert.Equal(t, "*", fb["milestoneNumber"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{"nodes": []any{}},
					},
				},
			},
		))

		_, err = pWild.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"assignee":  "*",
			"milestone": "*",
			"api_base":  baseURLWild,
		})
		require.NoError(t, err)
	})

	t.Run("resolves milestone title across paginated milestones and handles unmatched title", func(t *testing.T) {
		t.Parallel()

		var gqlCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			gqlCalls.Add(1)
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			w.Header().Set("Content-Type", "application/json")

			if strings.Contains(req.Query, "milestones(") {
				after, _ := req.Variables["after"].(string)
				if after == "" {
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"repository": map[string]any{
								"milestones": map[string]any{
									"nodes": []any{
										map[string]any{"number": float64(1), "title": "v1.0"},
									},
									"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "ms_cur_1"},
								},
							},
						},
					})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"milestones": map[string]any{
								"nodes": []any{
									map[string]any{"number": float64(9), "title": "V2.0"},
								},
								"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
							},
						},
					},
				})
				return
			}

			fb := req.Variables["filterBy"].(map[string]any)
			assert.Equal(t, "9", fb["milestoneNumber"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{
							"nodes": []any{
								map[string]any{"number": float64(88), "title": "Release blocker"},
							},
						},
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"milestone": "v2.0",
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(3), gqlCalls.Load())
		issues := out.Data.(map[string]any)["result"].([]any)
		require.Len(t, issues, 1)

		// Unmatched milestone title returns empty slice immediately without querying issues
		var unmatchedCalls atomic.Int32
		pUnmatched, baseURLUnmatched := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			unmatchedCalls.Add(1)
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.Contains(t, req.Query, "milestones(")
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"milestones": map[string]any{
							"nodes":    []any{map[string]any{"number": float64(1), "title": "v1.0"}},
							"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
						},
					},
				},
			})
		})

		outEmpty, err := pUnmatched.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"milestone": "nonexistent-milestone",
			"api_base":  baseURLUnmatched,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), unmatchedCalls.Load(), "must not query issues when milestone title does not exist")
		assert.Empty(t, outEmpty.Data.(map[string]any)["result"].([]any))
	})

	t.Run("accepts numeric milestone string and rejects invalid filter values", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, graphqlHandler(t,
			func(_ string, vars map[string]any) {
				fb := vars["filterBy"].(map[string]any)
				assert.Equal(t, "14", fb["milestoneNumber"])
			},
			map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"issues": map[string]any{"nodes": []any{}},
					},
				},
			},
		))

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issues",
			"owner":     "public-org",
			"repo":      "demo",
			"milestone": "14",
			"api_base":  baseURL,
		})
		require.NoError(t, err)

		badCases := []struct {
			name    string
			inputs  map[string]any
			wantSub string
		}{
			{
				name:    "invalid sort",
				inputs:  map[string]any{"sort": "stars"},
				wantSub: "invalid 'sort'",
			},
			{
				name:    "invalid direction",
				inputs:  map[string]any{"direction": "sideways"},
				wantSub: "invalid 'direction'",
			},
			{
				name:    "zero milestone int",
				inputs:  map[string]any{"milestone": 0},
				wantSub: "'milestone' number must be >= 1",
			},
			{
				name:    "negative milestone string",
				inputs:  map[string]any{"milestone": "-3"},
				wantSub: "'milestone' number must be >= 1",
			},
			{
				name:    "bool milestone",
				inputs:  map[string]any{"milestone": true},
				wantSub: "'milestone' must be a string or integer",
			},
			{
				name:    "non-string assignee",
				inputs:  map[string]any{"assignee": 42},
				wantSub: "'assignee' must be a string",
			},
			{
				name:    "non-string labels item",
				inputs:  map[string]any{"labels": []any{99}},
				wantSub: "labels[0] must be a string",
			},
			{
				name:    "invalid RFC 3339 since timestamp fails before milestone lookup",
				inputs:  map[string]any{"since": "not-a-timestamp", "milestone": "v1.0"},
				wantSub: "'since' must be a valid RFC 3339 timestamp",
			},
		}

		for _, tc := range badCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				in := map[string]any{
					"operation": "list_issues",
					"owner":     "public-org",
					"repo":      "demo",
				}
				for k, v := range tc.inputs {
					in[k] = v
				}
				_, execErr := p.execute(context.Background(), in)
				require.Error(t, execErr)
				assert.Contains(t, execErr.Error(), tc.wantSub)
			})
		}
	})
}

// ─── list_issue_timeline Tests ───────────────────────────────────────────────

func TestProvider_Execute_ListIssueTimeline(t *testing.T) {
	t.Parallel()

	t.Run("paginates via Link header and normalizes timeline event fields", func(t *testing.T) {
		t.Parallel()

		var pageCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			n := pageCalls.Add(1)
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/repos/public-org/demo/issues/42/timeline", r.URL.Path)
			assert.Equal(t, "2", r.URL.Query().Get("per_page"))
			w.Header().Set("Content-Type", "application/json")

			switch r.URL.Query().Get("page") {
			case "1":
				w.Header().Add("Link", fmt.Sprintf(`<%s/repos/public-org/demo/issues/42/timeline?per_page=2&page=1>; rel="first"`, r.Host))
				w.Header().Add("Link", fmt.Sprintf(`<%s/repos/public-org/demo/issues/42/timeline?per_page=2&page=2>; rel=next`, r.Host))
				_ = json.NewEncoder(w).Encode([]any{
					map[string]any{
						"event":      "review_requested",
						"created_at": "2026-01-01T10:00:00Z",
						"actor":      map[string]any{"login": "octocat"},
						"requested_reviewer": map[string]any{
							"login":    "Copilot",
							"html_url": "https://github.com/apps/copilot-pull-request-reviewer",
						},
						"commit_id": nil,
					},
					map[string]any{
						"event":      "copilot_work_started",
						"created_at": "2026-01-01T10:00:35Z",
						"actor":      map[string]any{"login": "public-org"},
					},
				})
			case "2":
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/public-org/demo/issues/42/timeline?per_page=2&page=1>; rel="prev"`, r.Host))
				_ = json.NewEncoder(w).Encode([]any{
					map[string]any{
						"event":        "reviewed",
						"submitted_at": "2026-01-01T10:03:00Z",
						"user": map[string]any{
							"login":    "Copilot",
							"html_url": "https://github.com/apps/copilot-pull-request-reviewer",
						},
						"state":     "commented",
						"commit_id": "aaaa1111bbbb2222cccc3333dddd4444eeee5555",
					},
					map[string]any{
						"event":  "committed",
						"sha":    "ffff9999eeee8888dddd7777cccc6666bbbb5555",
						"author": map[string]any{"name": "Dev", "date": "2026-01-01T10:05:00Z"},
					},
				})
			default:
				t.Fatalf("unexpected page query: %s (call %d)", r.URL.Query().Get("page"), n)
			}
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"per_page":  2,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(2), pageCalls.Load(), "Link header without rel=next must stop at page 2 without fetching page 3")

		events, ok := out.Data.(map[string]any)["result"].([]map[string]any)
		require.True(t, ok)
		require.Len(t, events, 4)

		// Event 0: review_requested
		assert.Equal(t, "review_requested", events[0]["event"])
		assert.Equal(t, "2026-01-01T10:00:00Z", events[0]["created_at"])
		assert.Nil(t, events[0]["state"])
		_, hasState := events[0]["state"]
		assert.True(t, hasState, "state key must always exist for CEL compatibility")

		// Event 1: copilot_work_started
		assert.Equal(t, "copilot_work_started", events[1]["event"])
		_, hasReqReviewer := events[1]["requested_reviewer"]
		assert.True(t, hasReqReviewer)

		// Event 2: reviewed normalizes user -> actor and submitted_at -> created_at while preserving raw keys
		assert.Equal(t, "reviewed", events[2]["event"])
		assert.Equal(t, "2026-01-01T10:03:00Z", events[2]["created_at"])
		assert.Equal(t, "commented", events[2]["state"])
		assert.Equal(t, "aaaa1111bbbb2222cccc3333dddd4444eeee5555", events[2]["commit_id"])
		actorMap, ok := events[2]["actor"].(map[string]any)
		require.True(t, ok)
		assert.Equal(t, "Copilot", actorMap["login"])
		assert.NotNil(t, events[2]["user"], "raw user field must remain preserved")

		// Event 3: committed normalizes sha -> commit_id and author.date -> created_at
		assert.Equal(t, "committed", events[3]["event"])
		assert.Equal(t, "ffff9999eeee8888dddd7777cccc6666bbbb5555", events[3]["commit_id"])
		assert.Equal(t, "2026-01-01T10:05:00Z", events[3]["created_at"])
	})

	t.Run("paginates without Link header until short or empty page and defaults per_page to 100", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "100", r.URL.Query().Get("per_page"))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"event": "labeled", "created_at": "2026-01-01T00:00:00Z"},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    7,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		events := out.Data.(map[string]any)["result"].([]map[string]any)
		require.Len(t, events, 1)
	})

	t.Run("supports dry-run and enforces pagination safety cap and input validation", func(t *testing.T) {
		t.Parallel()

		p := newProvider()
		dryOut, err := p.execute(sdkprovider.WithDryRun(context.Background(), true), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
		})
		require.NoError(t, err)
		dryMap := dryOut.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, true, dryMap["dry_run"])
		assert.Equal(t, "list_issue_timeline", dryMap["operation"])

		_, err = p.execute(context.Background(), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "'number' is required")

		var pageCalls atomic.Int32
		pCap, baseURLCap := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			pageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]any{
				map[string]any{"event": "labeled"},
			})
		})

		_, err = pCap.execute(context.Background(), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"per_page":  1,
			"api_base":  baseURLCap,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeded pagination safety limit")
		assert.Equal(t, int32(maxGraphQLPaginationPages), pageCalls.Load())

		pBadJSON, baseURLBadJSON := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"unexpected": true})
		})
		_, err = pBadJSON.execute(context.Background(), map[string]any{
			"operation": "list_issue_timeline",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"api_base":  baseURLBadJSON,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expected array")
	})
}

// ─── request_reviewers @copilot Alias Resolution Tests ───────────────────────

func TestProvider_Execute_RequestReviewers_CopilotAliases(t *testing.T) {
	t.Parallel()

	t.Run("Method 1 REST succeeds in one request when Copilot appears in requested_reviewers", func(t *testing.T) {
		t.Parallel()

		var httpCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			httpCalls.Add(1)
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/repos/public-org/demo/pulls/42/requested_reviewers", r.URL.Path)

			bodyBytes, _ := io.ReadAll(r.Body)
			var payload map[string]any
			require.NoError(t, json.Unmarshal(bodyBytes, &payload))
			assert.Equal(t, []any{"copilot-pull-request-reviewer[bot]"}, payload["reviewers"])

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":  float64(42),
				"node_id": "PR_42",
				"requested_reviewers": []any{
					map[string]any{
						"login":    "Copilot",
						"node_id":  defaultCopilotBotNodeID,
						"html_url": "https://github.com/apps/copilot-pull-request-reviewer",
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"reviewers": []any{"@copilot"},
			"bot_ids":   []any{defaultCopilotBotNodeID},
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), httpCalls.Load(), "must strip Copilot bot_id when REST already queued Copilot")
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, float64(42), res["number"])
	})

	t.Run("Method 2 GraphQL fallback resolves PR ID and custom Copilot bot via suggestedReviewerActors in one query", func(t *testing.T) {
		t.Parallel()

		var restCalls atomic.Int32
		var gqlOps []string
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/repos/public-org/demo/pulls/42/requested_reviewers" {
				restCalls.Add(1)
				w.WriteHeader(http.StatusUnprocessableEntity)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"message": "Reviews may only be requested from collaborators.",
				})
				return
			}

			var gqlReq graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
			switch {
			case strings.Contains(gqlReq.Query, "suggestedReviewerActors"):
				gqlOps = append(gqlOps, "suggestedReviewerActors")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"id": "PR_42_single_hop",
								"suggestedReviewerActors": map[string]any{
									"nodes": []any{
										map[string]any{
											"reviewer": map[string]any{
												"__typename": "Bot",
												"id":         "BOT_swe_agent_wrong",
												"login":      "copilot-swe-agent",
											},
										},
										map[string]any{
											"reviewer": map[string]any{
												"__typename": "Bot",
												"id":         "BOT_custom_copilot_reviewer",
												"login":      "copilot-pull-request-reviewer",
											},
										},
									},
								},
							},
						},
					},
				})
			case strings.Contains(gqlReq.Query, "requestReviews"):
				gqlOps = append(gqlOps, "requestReviews")
				input := gqlReq.Variables["input"].(map[string]any)
				assert.Equal(t, "PR_42_single_hop", input["pullRequestId"])
				assert.Equal(t, []any{"BOT_custom_copilot_reviewer"}, input["botIds"])
				assert.Equal(t, true, input["union"])
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"requestReviews": map[string]any{
							"pullRequest": map[string]any{
								"id":     "PR_42_single_hop",
								"number": float64(42),
								"reviewRequests": map[string]any{
									"nodes": []any{
										map[string]any{
											"requestedReviewer": map[string]any{
												"__typename": "Bot",
												"id":         "BOT_custom_copilot_reviewer",
												"login":      "copilot-pull-request-reviewer",
											},
										},
									},
								},
							},
						},
					},
				})
			default:
				t.Fatalf("unexpected GraphQL query: %s", gqlReq.Query)
			}
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"reviewers": []any{"copilot"},
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), restCalls.Load())
		assert.Equal(t, []string{"suggestedReviewerActors", "requestReviews"}, gqlOps)

		res := out.Data.(map[string]any)["result"].(map[string]any)
		rr := res["reviewRequests"].([]map[string]any)
		require.Len(t, rr, 1)
		assert.Equal(t, "BOT_custom_copilot_reviewer", rr[0]["id"])
	})

	t.Run("human reviewer plus @copilot on GitHub.com retries human on REST 422 and falls back to default bot ID", func(t *testing.T) {
		t.Parallel()

		var restPayloads []map[string]any
		p := testRoutedProvider(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, "api.github.com", r.Header.Get("X-Logical-Host"))
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/repos/public-org/demo/pulls/42/requested_reviewers" {
				bodyBytes, _ := io.ReadAll(r.Body)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(bodyBytes, &payload))
				restPayloads = append(restPayloads, payload)

				if len(restPayloads) == 1 {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": "bot not allowed via REST"})
					return
				}

				_ = json.NewEncoder(w).Encode(map[string]any{
					"number":              float64(42),
					"node_id":             "PR_from_human_retry",
					"requested_reviewers": []any{map[string]any{"login": "octocat"}},
				})
				return
			}

			var gqlReq graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
			switch {
			case strings.Contains(gqlReq.Query, "suggestedReviewerActors"):
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"id":                      "PR_from_human_retry",
								"suggestedReviewerActors": map[string]any{"nodes": []any{}},
							},
						},
					},
				})
			case strings.Contains(gqlReq.Query, "suggestedActors"):
				// Empty discovery on GitHub.com falls back to defaultCopilotBotNodeID
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"suggestedActors": map[string]any{"nodes": []any{}},
						},
					},
				})
			case strings.Contains(gqlReq.Query, "requestReviews"):
				input := gqlReq.Variables["input"].(map[string]any)
				assert.Equal(t, "PR_from_human_retry", input["pullRequestId"], "must reuse node_id from human retry")
				assert.Equal(t, []any{defaultCopilotBotNodeID}, input["botIds"])
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"requestReviews": map[string]any{
							"pullRequest": map[string]any{
								"id":     "PR_from_human_retry",
								"number": float64(42),
							},
						},
					},
				})
			default:
				t.Fatalf("unexpected GraphQL query: %s", gqlReq.Query)
			}
		})

		_, err := p.execute(context.Background(), map[string]any{
			"operation":      "request_reviewers",
			"owner":          "public-org",
			"repo":           "demo",
			"number":         42,
			"reviewers":      []any{"octocat", "copilot-pull-request-reviewer[bot]"},
			"team_reviewers": []any{"core-team"},
			"bot_ids":        []any{"@Copilot"},
		})
		require.NoError(t, err)
		require.Len(t, restPayloads, 2)
		assert.Equal(t, []any{"octocat", "copilot-pull-request-reviewer[bot]"}, restPayloads[0]["reviewers"])
		assert.Equal(t, []any{"octocat"}, restPayloads[1]["reviewers"])
		assert.Equal(t, []any{"core-team"}, restPayloads[1]["team_reviewers"])
	})

	t.Run("silent REST drop ignores substring-matching human URL and non-422 REST error fails fast", func(t *testing.T) {
		t.Parallel()

		// Silent drop: REST returns 200 with reviewers whose login or profile URL
		// resembles Copilot without being the Copilot reviewer app -- neither may skip GraphQL fallback.
		var silentFallbackMutated atomic.Bool
		pSilent := testRoutedProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/repos/public-org/demo/pulls/10/requested_reviewers" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"number":  float64(10),
					"node_id": "PR_silent_10",
					"requested_reviewers": []any{
						map[string]any{
							"login":    "copilot-pull-request-reviewer-demo",
							"html_url": "https://github.com/copilot-pull-request-reviewer-demo",
						},
						map[string]any{
							"login":    "copilot",
							"type":     "User",
							"html_url": "https://github.com/copilot",
						},
					},
				})
				return
			}
			var gqlReq graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
			if strings.Contains(gqlReq.Query, "suggestedReviewerActors") || strings.Contains(gqlReq.Query, "suggestedActors") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"errors": []any{map[string]any{"message": "field unavailable"}},
				})
				return
			}
			silentFallbackMutated.Store(true)
			input := gqlReq.Variables["input"].(map[string]any)
			assert.Equal(t, "PR_silent_10", input["pullRequestId"])
			assert.Equal(t, []any{defaultCopilotBotNodeID}, input["botIds"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"requestReviews": map[string]any{
						"pullRequest": map[string]any{"id": "PR_silent_10", "number": float64(10)},
					},
				},
			})
		})

		_, err := pSilent.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    10,
			"reviewers": []any{"@copilot"},
			"api_base":  "https://api.github.com:443/",
		})
		require.NoError(t, err)
		assert.True(t, silentFallbackMutated.Load(), "GraphQL requestReviews fallback must execute when REST omits the Copilot app")

		// HTTP 403 on REST is not swallowed
		var gqlCalledOn403 atomic.Bool
		p403, baseURL403 := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/graphql" {
				gqlCalledOn403.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Resource not accessible by integration"})
		})

		_, err = p403.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    10,
			"reviewers": []any{"@copilot"},
			"api_base":  baseURL403,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "HTTP 403")
		assert.False(t, gqlCalledOn403.Load(), "must not fall back to GraphQL on HTTP 403")

		// HTTP 500 on REST is not swallowed
		var gqlCalledOn500 atomic.Bool
		p500, baseURL500 := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/graphql" {
				gqlCalledOn500.Store(true)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "internal failure"})
		})

		_, err = p500.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    10,
			"reviewers": []any{"@copilot"},
			"api_base":  baseURL500,
		})
		require.Error(t, err)
		assert.False(t, gqlCalledOn500.Load(), "must not fall back to GraphQL on HTTP 500")
	})

	t.Run("Enterprise api_base rejects GitHub.com default bot ID across all fallback exits unless discovered", func(t *testing.T) {
		t.Parallel()

		enterpriseExitCases := []struct {
			name                string
			suggestedActorsResp map[string]any
		}{
			{
				name: "GraphQL error on suggestedActors",
				suggestedActorsResp: map[string]any{
					"errors": []any{map[string]any{"message": "field suggestedActors unavailable"}},
				},
			},
			{
				name: "malformed suggestedActors without nodes array",
				suggestedActorsResp: map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"suggestedActors": map[string]any{"unexpected": true},
						},
					},
				},
			},
			{
				name: "empty suggestedActors discovery",
				suggestedActorsResp: map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"suggestedActors": map[string]any{"nodes": []any{}},
						},
					},
				},
			},
		}

		for _, tc := range enterpriseExitCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()

				var mutationCalled atomic.Bool
				pGHE := testRoutedProvider(t, func(w http.ResponseWriter, r *http.Request) {
					assert.Equal(t, "ghe.example.com", r.Header.Get("X-Logical-Host"))
					w.Header().Set("Content-Type", "application/json")
					if r.URL.Path == "/api/v3/repos/ent-org/demo/pulls/42/requested_reviewers" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_ = json.NewEncoder(w).Encode(map[string]any{"message": "unprocessable"})
						return
					}

					assert.Equal(t, "/api/graphql", r.URL.Path)
					var gqlReq graphqlRequest
					require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
					switch {
					case strings.Contains(gqlReq.Query, "suggestedReviewerActors"):
						_ = json.NewEncoder(w).Encode(map[string]any{
							"data": map[string]any{
								"repository": map[string]any{
									"pullRequest": map[string]any{
										"id":                      "PR_ghe_42",
										"suggestedReviewerActors": map[string]any{"nodes": []any{}},
									},
								},
							},
						})
					case strings.Contains(gqlReq.Query, "suggestedActors"):
						_ = json.NewEncoder(w).Encode(tc.suggestedActorsResp)
					case strings.Contains(gqlReq.Query, "requestReviews"):
						mutationCalled.Store(true)
					default:
						t.Fatalf("unexpected GraphQL query: %s", gqlReq.Query)
					}
				})

				_, execErr := pGHE.execute(context.Background(), map[string]any{
					"operation": "request_reviewers",
					"owner":     "ent-org",
					"repo":      "demo",
					"number":    42,
					"reviewers": []any{"@copilot"},
					"api_base":  "https://ghe.example.com/api/v3",
				})
				require.Error(t, execErr)
				assert.Contains(t, execErr.Error(), "could not resolve Copilot reviewer bot ID")
				assert.Contains(t, execErr.Error(), "bot_ids")
				assert.False(t, mutationCalled.Load(), "must not send GitHub.com default bot ID to Enterprise endpoint")
			})
		}

		t.Run("preserves valid bot data from partial GraphQL responses on both Enterprise discovery paths", func(t *testing.T) {
			t.Parallel()

			// Path 1: suggestedReviewerActors returns partial data alongside GraphQL errors
			var repoSuggestedCalled atomic.Bool
			pPartialPR := testRoutedProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v3/repos/ent-org/demo/pulls/42/requested_reviewers" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": "unprocessable"})
					return
				}
				var gqlReq graphqlRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
				switch {
				case strings.Contains(gqlReq.Query, "suggestedReviewerActors"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"repository": map[string]any{
								"pullRequest": map[string]any{
									"id": "PR_ghe_partial_1",
									"suggestedReviewerActors": map[string]any{
										"nodes": []any{
											nil,
											map[string]any{
												"reviewer": map[string]any{
													"__typename": "Bot",
													"id":         "BOT_ghe_partial_pr",
													"login":      "copilot-pull-request-reviewer",
												},
											},
										},
									},
								},
							},
						},
						"errors": []any{map[string]any{"message": "partial actor resolution error"}},
					})
				case strings.Contains(gqlReq.Query, "suggestedActors"):
					repoSuggestedCalled.Store(true)
				case strings.Contains(gqlReq.Query, "requestReviews"):
					input := gqlReq.Variables["input"].(map[string]any)
					assert.Equal(t, "PR_ghe_partial_1", input["pullRequestId"])
					assert.Equal(t, []any{"BOT_ghe_partial_pr"}, input["botIds"])
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"requestReviews": map[string]any{
								"pullRequest": map[string]any{"id": "PR_ghe_partial_1", "number": float64(42)},
							},
						},
					})
				default:
					t.Fatalf("unexpected GraphQL query: %s", gqlReq.Query)
				}
			})

			_, err := pPartialPR.execute(context.Background(), map[string]any{
				"operation": "request_reviewers",
				"owner":     "ent-org",
				"repo":      "demo",
				"number":    42,
				"reviewers": []any{"@copilot"},
				"api_base":  "https://ghe.example.com/api/v3",
			})
			require.NoError(t, err)
			assert.False(t, repoSuggestedCalled.Load(), "partial suggestedReviewerActors match must succeed without second discovery query")

			// Path 2: suggestedReviewerActors returns partial PR ID with errors, and suggestedActors returns partial bot data with errors
			var resolvePRCalled atomic.Bool
			pPartialRepo := testRoutedProvider(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v3/repos/ent-org/demo/pulls/43/requested_reviewers" {
					w.WriteHeader(http.StatusUnprocessableEntity)
					_ = json.NewEncoder(w).Encode(map[string]any{"message": "unprocessable"})
					return
				}
				var gqlReq graphqlRequest
				require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
				switch {
				case strings.Contains(gqlReq.Query, "suggestedReviewerActors"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"repository": map[string]any{
								"pullRequest": map[string]any{
									"id":                      "PR_ghe_partial_2",
									"suggestedReviewerActors": nil,
								},
							},
						},
						"errors": []any{map[string]any{"message": "suggestedReviewerActors forbidden"}},
					})
				case strings.Contains(gqlReq.Query, "suggestedActors"):
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"repository": map[string]any{
								"suggestedActors": map[string]any{
									"nodes": []any{
										nil,
										map[string]any{
											"__typename": "Bot",
											"id":         "BOT_ghe_partial_repo",
											"login":      "copilot-pull-request-reviewer",
										},
									},
								},
							},
						},
						"errors": []any{map[string]any{"message": "partial suggestedActors node error"}},
					})
				case strings.Contains(gqlReq.Query, "pullRequest(number:"):
					resolvePRCalled.Store(true)
				case strings.Contains(gqlReq.Query, "requestReviews"):
					input := gqlReq.Variables["input"].(map[string]any)
					assert.Equal(t, "PR_ghe_partial_2", input["pullRequestId"], "must preserve pullRequest.id from partial PR discovery response")
					assert.Equal(t, []any{"BOT_ghe_partial_repo"}, input["botIds"])
					_ = json.NewEncoder(w).Encode(map[string]any{
						"data": map[string]any{
							"requestReviews": map[string]any{
								"pullRequest": map[string]any{"id": "PR_ghe_partial_2", "number": float64(43)},
							},
						},
					})
				default:
					t.Fatalf("unexpected GraphQL query: %s", gqlReq.Query)
				}
			})

			_, err = pPartialRepo.execute(context.Background(), map[string]any{
				"operation": "request_reviewers",
				"owner":     "ent-org",
				"repo":      "demo",
				"number":    43,
				"reviewers": []any{"@copilot"},
				"api_base":  "https://ghe.example.com/api/v3",
			})
			require.NoError(t, err)
			assert.False(t, resolvePRCalled.Load(), "must reuse PR ID extracted from partial PR discovery response")
		})
	})
}

// ─── Benchmarks ──────────────────────────────────────────────────────────────

func BenchmarkExecuteListIssueTimeline(b *testing.B) {
	p, baseURL := testProvider(b, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]any{
			map[string]any{
				"event":      "review_requested",
				"created_at": "2026-01-01T00:00:00Z",
				"actor":      map[string]any{"login": "octocat"},
				"requested_reviewer": map[string]any{
					"login": "Copilot",
				},
			},
			map[string]any{
				"event":        "reviewed",
				"submitted_at": "2026-01-01T00:02:00Z",
				"user":         map[string]any{"login": "Copilot"},
				"state":        "commented",
				"commit_id":    "deadbeef",
			},
		})
	})

	inputs := map[string]any{
		"operation": "list_issue_timeline",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    1,
		"api_base":  baseURL,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(context.Background(), inputs)
	}
}

func BenchmarkExecuteRequestReviewers_CopilotAlias(b *testing.B) {
	p, baseURL := testProvider(b, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number":  float64(1),
			"node_id": "PR_1",
			"requested_reviewers": []any{
				map[string]any{"login": "Copilot"},
			},
		})
	})

	inputs := map[string]any{
		"operation": "request_reviewers",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    1,
		"reviewers": []string{"@copilot"},
		"api_base":  baseURL,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(context.Background(), inputs)
	}
}

func BenchmarkExecuteListIssues_Filtered(b *testing.B) {
	p, baseURL := testProvider(b, graphqlHandler(b, nil, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"issues": map[string]any{
					"nodes": []any{
						map[string]any{"number": float64(1), "title": "Bug", "state": "OPEN"},
					},
				},
			},
		},
	}))

	inputs := map[string]any{
		"operation": "list_issues",
		"owner":     "public-org",
		"repo":      "demo",
		"state":     "open",
		"labels":    []string{"bug"},
		"assignee":  "octocat",
		"sort":      "updated",
		"direction": "desc",
		"api_base":  baseURL,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(context.Background(), inputs)
	}
}

func BenchmarkExecuteListPullRequests_Filtered(b *testing.B) {
	p, baseURL := testProvider(b, graphqlHandler(b, nil, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequests": map[string]any{
					"nodes": []any{
						map[string]any{
							"number":         float64(1),
							"headRefName":    "feat/demo",
							"headRepository": map[string]any{"owner": map[string]any{"login": "octocat"}},
						},
					},
					"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
				},
			},
		},
	}))

	inputs := map[string]any{
		"operation": "list_pull_requests",
		"owner":     "public-org",
		"repo":      "demo",
		"state":     "open",
		"head":      "octocat:feat/demo",
		"base":      "main",
		"api_base":  baseURL,
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(context.Background(), inputs)
	}
}
