// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─── Extended get_pull_request Tests ─────────────────────────────────────────

func TestProvider_Execute_GetPullRequest_ExtendedFieldsAndReviewRequests(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t,
		func(query string, _ map[string]any) {
			assert.Contains(t, query, "headRefOid")
			assert.Contains(t, query, "baseRefOid")
			assert.Contains(t, query, "mergeStateStatus")
			assert.Contains(t, query, "mergeCommit")
			assert.Contains(t, query, "reviewRequests")
		},
		map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"id":               "PR_kwDO123",
						"number":           float64(57),
						"title":            "feat: parity",
						"state":            "OPEN",
						"headRefName":      "feat/parity",
						"headRefOid":       "1111111111111111111111111111111111111111",
						"baseRefName":      "main",
						"baseRefOid":       "2222222222222222222222222222222222222222",
						"mergeStateStatus": "CLEAN",
						"mergeCommit":      map[string]any{"oid": "3333333333333333333333333333333333333333"},
						"reviewRequests": map[string]any{
							"nodes": []any{
								map[string]any{
									"requestedReviewer": map[string]any{
										"__typename": "User",
										"login":      "octocat",
									},
								},
								map[string]any{
									"requestedReviewer": map[string]any{
										"__typename": "Team",
										"name":       "Platform Reviewers",
										"slug":       "platform-reviewers",
									},
								},
								map[string]any{
									"requestedReviewer": map[string]any{
										"__typename": "Bot",
										"id":         "BOT_kgDOCnlnWA",
										"login":      "copilot-pull-request-reviewer",
									},
								},
								map[string]any{
									"requestedReviewer": map[string]any{
										"__typename": "Mannequin",
										"login":      "migrated-user",
									},
								},
								map[string]any{
									"requestedReviewer": nil,
								},
							},
						},
						"statusCheckRollup": map[string]any{
							"nodes": []any{
								map[string]any{
									"commit": map[string]any{
										"statusCheckRollup": map[string]any{
											"state":    "SUCCESS",
											"contexts": map[string]any{"nodes": []any{}},
										},
									},
								},
							},
						},
					},
				},
			},
		},
	))

	out, err := p.execute(context.Background(), map[string]any{
		"operation": "get_pull_request",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    57,
		"api_base":  baseURL,
	})
	require.NoError(t, err)

	pr := out.Data.(map[string]any)["result"].(map[string]any)
	assert.Equal(t, "1111111111111111111111111111111111111111", pr["headRefOid"])
	assert.Equal(t, "2222222222222222222222222222222222222222", pr["baseRefOid"])
	assert.Equal(t, "CLEAN", pr["mergeStateStatus"])
	assert.Equal(t, map[string]any{"oid": "3333333333333333333333333333333333333333"}, pr["mergeCommit"])

	rr, ok := pr["reviewRequests"].([]map[string]any)
	require.True(t, ok, "reviewRequests must be flattened to []map[string]any")
	require.Len(t, rr, 4)
	assert.Equal(t, "octocat", rr[0]["login"])
	assert.Equal(t, "platform-reviewers", rr[1]["slug"])
	assert.Equal(t, "BOT_kgDOCnlnWA", rr[2]["id"])
	assert.Equal(t, "migrated-user", rr[3]["login"])

	rollup := pr["statusCheckRollup"].(map[string]any)
	assert.Equal(t, "SUCCESS", rollup["state"])
}

func TestProvider_Execute_GetPullRequest_OmittedReviewRequestsReturnsEmptySlice(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, graphqlHandler(t, nil, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"id":     "PR_minimal",
					"number": float64(1),
					"title":  "Legacy mock",
				},
			},
		},
	}))

	out, err := p.execute(context.Background(), map[string]any{
		"operation": "get_pull_request",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    1,
		"api_base":  baseURL,
	})
	require.NoError(t, err)

	pr := out.Data.(map[string]any)["result"].(map[string]any)
	rr, ok := pr["reviewRequests"].([]map[string]any)
	require.True(t, ok)
	assert.NotNil(t, rr)
	assert.Empty(t, rr)
}

// ─── update_pull_request Draft Toggle Tests ──────────────────────────────────

func TestProvider_Execute_UpdatePullRequest_DraftToggle(t *testing.T) {
	t.Parallel()

	t.Run("marks draft PR ready for review without updatePullRequest when no other fields set", func(t *testing.T) {
		t.Parallel()

		var calls []string
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			w.Header().Set("Content-Type", "application/json")

			switch {
			case strings.Contains(req.Query, "markPullRequestReadyForReview"):
				calls = append(calls, "markReady")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"markPullRequestReadyForReview": map[string]any{
							"pullRequest": map[string]any{
								"id":      "PR_10",
								"number":  float64(10),
								"isDraft": false,
								"state":   "OPEN",
							},
						},
					},
				})
			case strings.Contains(req.Query, "pullRequest(number:"):
				calls = append(calls, "query")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"id":          "PR_10",
								"number":      float64(10),
								"title":       "WIP",
								"state":       "OPEN",
								"isDraft":     true,
								"url":         "https://github.com/public-org/demo/pull/10",
								"headRefName": "feat/x",
								"baseRefName": "main",
							},
						},
					},
				})
			default:
				t.Fatalf("unexpected GraphQL query: %s", req.Query)
			}
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "update_pull_request",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    10,
			"draft":     false,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"query", "markReady"}, calls)
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, false, res["isDraft"])
	})

	t.Run("converts ready PR to draft alongside title update", func(t *testing.T) {
		t.Parallel()

		var calls []string
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			w.Header().Set("Content-Type", "application/json")

			switch {
			case strings.Contains(req.Query, "updatePullRequest"):
				calls = append(calls, "updateFields")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"updatePullRequest": map[string]any{
							"pullRequest": map[string]any{
								"id":      "PR_11",
								"number":  float64(11),
								"title":   "WIP: updated",
								"isDraft": false,
							},
						},
					},
				})
			case strings.Contains(req.Query, "convertPullRequestToDraft"):
				calls = append(calls, "convertToDraft")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"convertPullRequestToDraft": map[string]any{
							"pullRequest": map[string]any{
								"id":      "PR_11",
								"number":  float64(11),
								"title":   "WIP: updated",
								"isDraft": true,
							},
						},
					},
				})
			default:
				calls = append(calls, "query")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"id":      "PR_11",
								"number":  float64(11),
								"title":   "Old",
								"state":   "OPEN",
								"isDraft": false,
							},
						},
					},
				})
			}
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "update_pull_request",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    11,
			"title":     "WIP: updated",
			"draft":     true,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, []string{"query", "updateFields", "convertToDraft"}, calls)
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, true, res["isDraft"])
		assert.Equal(t, "WIP: updated", res["title"])
	})

	t.Run("idempotent no-op when draft matches current state and no other fields set", func(t *testing.T) {
		t.Parallel()

		var callCount atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			callCount.Add(1)
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			assert.False(t, strings.HasPrefix(strings.TrimSpace(req.Query), "mutation"))

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"id":          "PR_12",
							"number":      float64(12),
							"title":       "Already Ready",
							"state":       "OPEN",
							"isDraft":     false,
							"url":         "https://github.com/public-org/demo/pull/12",
							"headRefName": "feat/ready",
							"baseRefName": "main",
						},
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "update_pull_request",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    12,
			"draft":     false,
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), callCount.Load())
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, "Already Ready", res["title"])
	})

	t.Run("rejects non-boolean draft input", func(t *testing.T) {
		t.Parallel()
		p := newProvider()
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "update_pull_request",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    12,
			"draft":     "false",
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "'draft' must be a boolean")
	})
}

// ─── Paginated list_review_threads Tests ─────────────────────────────────────

func TestProvider_Execute_ListReviewThreads_PaginationAndUnresolvedOnly(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		w.Header().Set("Content-Type", "application/json")

		// Overflow comments query for PRT_page2_unresolved
		if strings.Contains(req.Query, "node(id: $threadId)") {
			assert.Equal(t, "PRT_page2_unresolved", req.Variables["threadId"])
			assert.Equal(t, "c_cursor_1", req.Variables["after"])
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"node": map[string]any{
						"comments": map[string]any{
							"nodes": []any{
								map[string]any{
									"id":         "PRRC_overflow_2",
									"databaseId": float64(9002),
									"body":       "Follow-up comment on overflow page",
									"url":        "https://github.com/public-org/demo/pull/42#discussion_r9002",
									"author":     map[string]any{"login": "reviewer2"},
									"createdAt":  "2026-01-02T00:00:00Z",
								},
							},
							"pageInfo": map[string]any{
								"hasNextPage": false,
								"endCursor":   "",
							},
						},
					},
				},
			})
			return
		}

		after, _ := req.Variables["after"].(string)
		switch after {
		case "":
			// Page 1 contains ONLY resolved threads -- unresolved_only must still advance cursor!
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"nodes": []any{
									map[string]any{
										"id":         "PRT_page1_resolved",
										"isResolved": true,
										"isOutdated": false,
										"path":       "a.go",
										"line":       float64(5),
										"comments":   map[string]any{"nodes": []any{}},
									},
								},
								"pageInfo": map[string]any{
									"hasNextPage": true,
									"endCursor":   "cursor_page_1",
								},
							},
						},
					},
				},
			})
		case "cursor_page_1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"nodes": []any{
									map[string]any{
										"id":         "PRT_page2_unresolved",
										"isResolved": false,
										"isOutdated": false,
										"path":       "b.go",
										"line":       float64(19),
										"comments": map[string]any{
											"nodes": []any{
												map[string]any{
													"id":         "PRRC_initial_1",
													"databaseId": float64(9001),
													"body":       "Initial thread comment",
													"url":        "https://github.com/public-org/demo/pull/42#discussion_r9001",
													"author":     map[string]any{"login": "reviewer1"},
													"createdAt":  "2026-01-01T00:00:00Z",
												},
											},
											"pageInfo": map[string]any{
												"hasNextPage": true,
												"endCursor":   "c_cursor_1",
											},
										},
									},
								},
								"pageInfo": map[string]any{
									"hasNextPage": false,
									"endCursor":   "",
								},
							},
						},
					},
				},
			})
		default:
			t.Fatalf("unexpected cursor: %s", after)
		}
	})

	out, err := p.execute(context.Background(), map[string]any{
		"operation":       "list_review_threads",
		"owner":           "public-org",
		"repo":            "demo",
		"number":          42,
		"unresolved_only": true,
		"api_base":        baseURL,
	})
	require.NoError(t, err)

	threads := out.Data.(map[string]any)["result"].([]any)
	require.Len(t, threads, 1)

	th := threads[0].(map[string]any)
	assert.Equal(t, "PRT_page2_unresolved", th["id"])
	comments := th["comments"].(map[string]any)["nodes"].([]any)
	require.Len(t, comments, 2)
	assert.Equal(t, float64(9001), comments[0].(map[string]any)["databaseId"])
	assert.Equal(t, float64(9002), comments[1].(map[string]any)["databaseId"])
	assert.Contains(t, comments[1].(map[string]any)["url"], "#discussion_r9002")
}

// ─── list_pr_reviews Tests ───────────────────────────────────────────────────

func TestProvider_Execute_ListPRReviews_PaginationAndNilSafety(t *testing.T) {
	t.Parallel()

	p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
		var req graphqlRequest
		require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
		w.Header().Set("Content-Type", "application/json")

		after, _ := req.Variables["after"].(string)
		if after == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviews": map[string]any{
								"nodes": []any{
									map[string]any{
										"id":          "PRR_1",
										"databaseId":  float64(501),
										"author":      map[string]any{"login": "copilot-pull-request-reviewer"},
										"state":       "COMMENTED",
										"body":        "No issues found",
										"submittedAt": "2026-01-01T10:00:00Z",
										"commit":      map[string]any{"oid": "aaaa1111"},
										"url":         "https://github.com/public-org/demo/pull/7#pullrequestreview-501",
									},
								},
								"pageInfo": map[string]any{
									"hasNextPage": true,
									"endCursor":   "rev_cursor_1",
								},
							},
						},
					},
				},
			})
			return
		}

		// Second page: ghost user (author: null) and GC'd commit (commit: null)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": map[string]any{
				"repository": map[string]any{
					"pullRequest": map[string]any{
						"reviews": map[string]any{
							"nodes": []any{
								map[string]any{
									"id":          "PRR_2",
									"databaseId":  float64(502),
									"author":      nil,
									"state":       "APPROVED",
									"body":        "LGTM",
									"submittedAt": "2026-01-01T11:00:00Z",
									"commit":      nil,
									"url":         "https://github.com/public-org/demo/pull/7#pullrequestreview-502",
								},
							},
							"pageInfo": map[string]any{
								"hasNextPage": false,
								"endCursor":   "",
							},
						},
					},
				},
			},
		})
	})

	out, err := p.execute(context.Background(), map[string]any{
		"operation": "list_pr_reviews",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    7,
		"per_page":  1,
		"api_base":  baseURL,
	})
	require.NoError(t, err)

	reviews, ok := out.Data.(map[string]any)["result"].([]map[string]any)
	require.True(t, ok)
	require.Len(t, reviews, 2)

	assert.Equal(t, "PRR_1", reviews[0]["id"])
	assert.Equal(t, map[string]any{"login": "copilot-pull-request-reviewer"}, reviews[0]["author"])
	assert.Equal(t, map[string]any{"oid": "aaaa1111"}, reviews[0]["commit"])

	assert.Equal(t, "PRR_2", reviews[1]["id"])
	assert.Nil(t, reviews[1]["author"])
	assert.Nil(t, reviews[1]["commit"])
}

func TestExecuteListPRReviews_MissingNumber(t *testing.T) {
	t.Parallel()
	p := newProvider()
	_, err := p.execute(context.Background(), map[string]any{
		"operation": "list_pr_reviews",
		"owner":     "public-org",
		"repo":      "demo",
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "'number' is required")
}

func TestProvider_PaginationSafetyLimitsReturnExplicitErrors(t *testing.T) {
	t.Parallel()

	t.Run("list_review_threads returns error when pages exceed safety limit", func(t *testing.T) {
		t.Parallel()

		var pageCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			n := pageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviewThreads": map[string]any{
								"nodes": []any{
									map[string]any{
										"id":         "PRT_1",
										"isResolved": true,
										"comments":   map[string]any{"nodes": []any{}},
									},
								},
								"pageInfo": map[string]any{
									"hasNextPage": true,
									"endCursor":   "cur_next",
								},
							},
						},
					},
				},
			})
			assert.LessOrEqual(t, int(n), maxGraphQLPaginationPages)
		})

		_, err := p.execute(context.Background(), map[string]any{
			"operation":       "list_review_threads",
			"owner":           "public-org",
			"repo":            "demo",
			"number":          42,
			"per_page":        1,
			"unresolved_only": true,
			"api_base":        baseURL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeded pagination safety limit")
		assert.Equal(t, int32(maxGraphQLPaginationPages), pageCalls.Load())
	})

	t.Run("thread comment overflow returns error when pages exceed safety limit", func(t *testing.T) {
		t.Parallel()

		var calls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			n := calls.Add(1)
			var req graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&req))
			w.Header().Set("Content-Type", "application/json")

			if n == 1 {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"repository": map[string]any{
							"pullRequest": map[string]any{
								"reviewThreads": map[string]any{
									"nodes": []any{
										map[string]any{
											"id":         "PRT_noisy",
											"isResolved": false,
											"comments": map[string]any{
												"nodes": []any{},
												"pageInfo": map[string]any{
													"hasNextPage": true,
													"endCursor":   "c_1",
												},
											},
										},
									},
									"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
								},
							},
						},
					},
				})
				return
			}

			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"node": map[string]any{
						"comments": map[string]any{
							"nodes": []any{},
							"pageInfo": map[string]any{
								"hasNextPage": true,
								"endCursor":   "c_more",
							},
						},
					},
				},
			})
		})

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_review_threads",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"api_base":  baseURL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeded safety limit")
		assert.Equal(t, int32(1+maxCommentPagesPerThread), calls.Load())
	})

	t.Run("list_pr_reviews returns error when pages exceed safety limit", func(t *testing.T) {
		t.Parallel()

		var pageCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			pageCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{
							"reviews": map[string]any{
								"nodes": []any{},
								"pageInfo": map[string]any{
									"hasNextPage": true,
									"endCursor":   "rev_next",
								},
							},
						},
					},
				},
			})
		})

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_pr_reviews",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    7,
			"per_page":  1,
			"api_base":  baseURL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "exceeded pagination safety limit")
		assert.Equal(t, int32(maxGraphQLPaginationPages), pageCalls.Load())
	})
}

// ─── request_reviewers & reviewers Schema Dual-Type Tests ────────────────────

func TestSchema_ReviewersSupportsBothStringAndObject(t *testing.T) {
	t.Parallel()

	p := newProvider()
	schema := p.Descriptor().Schema
	sv := sdkprovider.NewSchemaValidator()

	// 1. String array for request_reviewers
	err := sv.ValidateInputs(map[string]any{
		"operation":      "request_reviewers",
		"owner":          "public-org",
		"repo":           "demo",
		"number":         42,
		"reviewers":      []any{"octocat", "hubot"},
		"team_reviewers": []any{"core-maintainers"},
		"bot_ids":        []any{"BOT_kgDOCnlnWA"},
	}, schema)
	require.NoError(t, err)

	// 2. Object array for create_or_update_environment
	err = sv.ValidateInputs(map[string]any{
		"operation":        "create_or_update_environment",
		"owner":            "public-org",
		"repo":             "demo",
		"environment_name": "production",
		"reviewers": []any{
			map[string]any{"type": "User", "id": 1001},
		},
	}, schema)
	require.NoError(t, err)

	// 3. Invalid number item in reviewers rejected by schema
	err = sv.ValidateInputs(map[string]any{
		"operation": "request_reviewers",
		"owner":     "public-org",
		"repo":      "demo",
		"number":    42,
		"reviewers": []any{12345},
	}, schema)
	require.Error(t, err)
}

func TestProvider_Execute_RequestReviewers_And_EnvironmentTypeChecks(t *testing.T) {
	t.Parallel()

	t.Run("combined REST human/team reviewers and GraphQL bot_ids reuses REST node_id", func(t *testing.T) {
		t.Parallel()

		var restCalls, gqlCalls atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/repos/public-org/demo/pulls/42/requested_reviewers" {
				restCalls.Add(1)
				assert.Equal(t, http.MethodPost, r.Method)
				bodyBytes, _ := io.ReadAll(r.Body)
				var payload map[string]any
				require.NoError(t, json.Unmarshal(bodyBytes, &payload))
				assert.Equal(t, []any{"octocat"}, payload["reviewers"])
				assert.Equal(t, []any{"platform-team"}, payload["team_reviewers"])

				_ = json.NewEncoder(w).Encode(map[string]any{
					"number":  float64(42),
					"node_id": "PR_reused_node_id",
				})
				return
			}

			gqlCalls.Add(1)
			var gqlReq graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
			assert.Contains(t, gqlReq.Query, "requestReviews")
			input := gqlReq.Variables["input"].(map[string]any)
			assert.Equal(t, "PR_reused_node_id", input["pullRequestId"])
			assert.Equal(t, []any{"BOT_kgDOCnlnWA"}, input["botIds"])
			assert.Equal(t, true, input["union"])

			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"requestReviews": map[string]any{
						"pullRequest": map[string]any{
							"id":     "PR_reused_node_id",
							"number": float64(42),
							"url":    "https://github.com/public-org/demo/pull/42",
							"reviewRequests": map[string]any{
								"nodes": []any{
									map[string]any{
										"requestedReviewer": map[string]any{
											"__typename": "Bot",
											"id":         "BOT_kgDOCnlnWA",
											"login":      "copilot-pull-request-reviewer",
										},
									},
								},
							},
						},
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation":      "request_reviewers",
			"owner":          "public-org",
			"repo":           "demo",
			"number":         42,
			"reviewers":      []any{"octocat"},
			"team_reviewers": []any{"platform-team"},
			"bot_ids":        []any{"BOT_kgDOCnlnWA"},
			"api_base":       baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), restCalls.Load())
		assert.Equal(t, int32(1), gqlCalls.Load(), "must reuse node_id from REST without extra resolvePullRequestID query")

		res := out.Data.(map[string]any)["result"].(map[string]any)
		rr := res["reviewRequests"].([]map[string]any)
		require.Len(t, rr, 1)
		assert.Equal(t, "BOT_kgDOCnlnWA", rr[0]["id"])
	})

	t.Run("bot_ids alone resolves PR ID and calls requestReviews", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			var gqlReq graphqlRequest
			require.NoError(t, json.NewDecoder(r.Body).Decode(&gqlReq))
			w.Header().Set("Content-Type", "application/json")

			if strings.Contains(gqlReq.Query, "requestReviews") {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"data": map[string]any{
						"requestReviews": map[string]any{
							"pullRequest": map[string]any{
								"id":     "PR_resolved",
								"number": float64(9),
								"url":    "https://github.com/public-org/demo/pull/9",
							},
						},
					},
				})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"data": map[string]any{
					"repository": map[string]any{
						"pullRequest": map[string]any{"id": "PR_resolved"},
					},
				},
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    9,
			"bot_ids":   []any{"BOT_kgDOCnlnWA"},
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, "PR_resolved", res["id"])
	})

	t.Run("REST-only human reviewers with []string slice returns REST payload directly", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodPost, r.Method)
			assert.Equal(t, "/repos/public-org/demo/pulls/15/requested_reviewers", r.URL.Path)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number":   float64(15),
				"html_url": "https://github.com/public-org/demo/pull/15",
			})
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    15,
			"reviewers": []string{"octocat", "hubot"},
			"api_base":  baseURL,
		})
		require.NoError(t, err)
		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, float64(15), res["number"])
	})

	t.Run("per-operation runtime validation rejects mismatched reviewer element types", func(t *testing.T) {
		t.Parallel()

		p := newProvider()

		// Object passed to request_reviewers
		_, err := p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
			"reviewers": []any{map[string]any{"type": "User", "id": 1}},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reviewers[0] must be a string")

		// Empty reviewers in request_reviewers
		_, err = p.execute(context.Background(), map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at least one of")

		// String passed to create_or_update_environment
		_, err = p.execute(context.Background(), map[string]any{
			"operation":        "create_or_update_environment",
			"owner":            "public-org",
			"repo":             "demo",
			"environment_name": "prod",
			"reviewers":        []any{"octocat"},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "reviewers[0] must be a non-null object")

		// Typed-nil map passed to create_or_update_environment
		_, err = p.execute(context.Background(), map[string]any{
			"operation":        "create_or_update_environment",
			"owner":            "public-org",
			"repo":             "demo",
			"environment_name": "prod",
			"reviewers":        []any{map[string]any(nil)},
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "non-null object")

		// >6 reviewers passed to create_or_update_environment
		tooMany := make([]any, 7)
		for i := range tooMany {
			tooMany[i] = map[string]any{"type": "User", "id": i + 1}
		}
		_, err = p.execute(context.Background(), map[string]any{
			"operation":        "create_or_update_environment",
			"owner":            "public-org",
			"repo":             "demo",
			"environment_name": "prod",
			"reviewers":        tooMany,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "at most 6 reviewers")
	})

	t.Run("DescribeWhatIf includes PR number for request_reviewers", func(t *testing.T) {
		t.Parallel()
		plug := NewPlugin()
		msg, err := plug.DescribeWhatIf(context.Background(), ProviderName, map[string]any{
			"operation": "request_reviewers",
			"owner":     "public-org",
			"repo":      "demo",
			"number":    42,
		})
		require.NoError(t, err)
		assert.Equal(t, "Would perform GitHub request_reviewers on public-org/demo#42", msg)
	})
}

func BenchmarkExecuteListPRReviews(b *testing.B) {
	p, baseURL := testProvider(b, graphqlHandler(b, nil, map[string]any{
		"data": map[string]any{
			"repository": map[string]any{
				"pullRequest": map[string]any{
					"reviews": map[string]any{
						"nodes": []any{
							map[string]any{
								"id":          "PRR_1",
								"databaseId":  float64(1),
								"author":      map[string]any{"login": "reviewer"},
								"state":       "APPROVED",
								"body":        "LGTM",
								"submittedAt": "2026-01-01T00:00:00Z",
								"commit":      map[string]any{"oid": "deadbeef"},
								"url":         "https://github.com/public-org/demo/pull/1#pullrequestreview-1",
							},
						},
						"pageInfo": map[string]any{"hasNextPage": false, "endCursor": ""},
					},
				},
			},
		},
	}))

	inputs := map[string]any{
		"operation": "list_pr_reviews",
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
