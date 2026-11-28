// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

const (
	// maxGraphQLPaginationPages bounds cursor pagination loops on PR review connections.
	maxGraphQLPaginationPages = 50
	// maxCommentPagesPerThread bounds overflow comment pagination for a single review thread.
	maxCommentPagesPerThread = 10
	// defaultGraphQLPageSize is the default GraphQL page size for paginated review queries.
	defaultGraphQLPageSize = 100
	// copilotRESTReviewerLogin is the GitHub REST login for the Copilot pull request reviewer app.
	copilotRESTReviewerLogin = "copilot-pull-request-reviewer[bot]"
	// defaultCopilotBotNodeID is GitHub's well-known GraphQL Bot node ID for copilot-pull-request-reviewer.
	defaultCopilotBotNodeID = "BOT_kgDOCnlnWA"
)

func getGraphQLPageSize(inputs map[string]any, operation string) (int, error) {
	raw, exists := inputs["per_page"]
	if !exists || raw == nil {
		return defaultGraphQLPageSize, nil
	}
	v, ok := getIntInput(inputs, "per_page")
	if !ok || v < 1 {
		return 0, fmt.Errorf("%s: 'per_page' must be an integer >= 1", operation)
	}
	return min(v, defaultGraphQLPageSize), nil
}

func extractPageInfo(conn map[string]any) (hasNextPage bool, endCursor string) {
	pi, ok := conn["pageInfo"].(map[string]any)
	if !ok {
		return false, ""
	}
	hasNextPage, _ = pi["hasNextPage"].(bool)
	endCursor, _ = pi["endCursor"].(string)
	return hasNextPage, endCursor
}

// ─── List Review Threads ─────────────────────────────────────────────────────

func (p *Provider) executeListReviewThreads(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, err := requirePositiveIntInput(inputs, "list_review_threads", "number")
	if err != nil {
		return nil, err
	}

	unresolvedOnly := false
	if rawUnresolved, has := inputs["unresolved_only"]; has && rawUnresolved != nil {
		b, isBool := rawUnresolved.(bool)
		if !isBool {
			return nil, fmt.Errorf("list_review_threads: 'unresolved_only' must be a boolean, got %T", rawUnresolved)
		}
		unresolvedOnly = b
	}

	pageSize, err := getGraphQLPageSize(inputs, "list_review_threads")
	if err != nil {
		return nil, err
	}

	query := `query($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: $first, after: $after) {
        nodes {
          id
          isResolved
          isOutdated
          path
          line
          comments(first: 100) {
            nodes {
              id
              databaseId
              body
              url
              author { login }
              createdAt
            }
            pageInfo {
              hasNextPage
              endCursor
            }
          }
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

	allThreads := make([]any, 0)
	var after string

	for page := 0; page < maxGraphQLPaginationPages; page++ {
		vars := map[string]any{
			"owner":  owner,
			"name":   repo,
			"number": num,
			"first":  pageSize,
		}
		if after != "" {
			vars["after"] = after
		}

		data, err := graphqlDo(ctx, client, apiBase, query, vars)
		if err != nil {
			return nil, err
		}

		conn, err := extractNodeMap(data, "repository.pullRequest.reviewThreads")
		if err != nil {
			return nil, err
		}
		rawNodes, ok := conn["nodes"]
		if !ok {
			return nil, fmt.Errorf("key %q has no 'nodes' field", "repository.pullRequest.reviewThreads")
		}
		pageNodes, ok := rawNodes.([]any)
		if !ok {
			return nil, fmt.Errorf("'nodes' at %q is not an array", "repository.pullRequest.reviewThreads")
		}

		for _, n := range pageNodes {
			threadMap, isMap := n.(map[string]any)
			if !isMap {
				continue
			}
			if unresolvedOnly {
				if resolved, _ := threadMap["isResolved"].(bool); resolved {
					continue
				}
			}
			if err := p.paginateThreadComments(ctx, client, apiBase, threadMap); err != nil {
				return nil, err
			}
			allThreads = append(allThreads, threadMap)
		}

		hasNext, endCursor := extractPageInfo(conn)
		if !hasNext {
			break
		}
		if endCursor == "" {
			return nil, fmt.Errorf("list_review_threads pagination: hasNextPage is true with empty endCursor")
		}
		if page == maxGraphQLPaginationPages-1 {
			if pageSize < defaultGraphQLPageSize {
				return nil, fmt.Errorf("list_review_threads: exceeded pagination safety limit (%d pages); increase per_page (up to %d)", maxGraphQLPaginationPages, defaultGraphQLPageSize)
			}
			return nil, fmt.Errorf("list_review_threads: exceeded pagination safety limit (%d pages)", maxGraphQLPaginationPages)
		}
		after = endCursor
	}

	return readOutput(allThreads), nil
}

func (p *Provider) paginateThreadComments(ctx context.Context, client *httpc.Client, apiBase string, threadMap map[string]any) error {
	commentsMap, ok := threadMap["comments"].(map[string]any)
	if !ok {
		return nil
	}
	hasNext, endCursor := extractPageInfo(commentsMap)
	if !hasNext {
		return nil
	}
	threadID, _ := threadMap["id"].(string)
	if threadID == "" {
		return fmt.Errorf("review thread comment pagination: thread id is missing")
	}
	existingNodes, _ := commentsMap["nodes"].([]any)

	overflowQuery := `query($threadId: ID!, $first: Int!, $after: String!) {
  node(id: $threadId) {
    ... on PullRequestReviewThread {
      comments(first: $first, after: $after) {
        nodes {
          id
          databaseId
          body
          url
          author { login }
          createdAt
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

	for page := 0; page < maxCommentPagesPerThread; page++ {
		if endCursor == "" {
			return fmt.Errorf("review thread %q comment pagination: hasNextPage is true with empty endCursor", threadID)
		}
		data, err := graphqlDo(ctx, client, apiBase, overflowQuery, map[string]any{
			"threadId": threadID,
			"first":    defaultGraphQLPageSize,
			"after":    endCursor,
		})
		if err != nil {
			return fmt.Errorf("paginating comments for thread %s: %w", threadID, err)
		}
		overflowConn, err := extractNodeMap(data, "node.comments")
		if err != nil {
			return fmt.Errorf("paginating comments for thread %s: %w", threadID, err)
		}
		moreNodes, _ := overflowConn["nodes"].([]any)
		existingNodes = append(existingNodes, moreNodes...)
		commentsMap["nodes"] = existingNodes
		if pi, ok := overflowConn["pageInfo"].(map[string]any); ok {
			commentsMap["pageInfo"] = pi
		}

		hasNext, endCursor = extractPageInfo(overflowConn)
		if !hasNext {
			break
		}
		if page == maxCommentPagesPerThread-1 {
			return fmt.Errorf("review thread %q comment pagination: exceeded safety limit (%d pages)", threadID, maxCommentPagesPerThread)
		}
	}

	return nil
}

// ─── List PR Reviews ─────────────────────────────────────────────────────────

func (p *Provider) executeListPRReviews(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, err := requirePositiveIntInput(inputs, "list_pr_reviews", "number")
	if err != nil {
		return nil, err
	}

	pageSize, err := getGraphQLPageSize(inputs, "list_pr_reviews")
	if err != nil {
		return nil, err
	}

	query := `query($owner: String!, $name: String!, $number: Int!, $first: Int!, $after: String) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviews(first: $first, after: $after) {
        nodes {
          id
          databaseId
          author { login }
          state
          body
          submittedAt
          commit { oid }
          url
        }
        pageInfo {
          hasNextPage
          endCursor
        }
      }
    }
  }
}`

	reviews := make([]map[string]any, 0)
	var after string

	for page := 0; page < maxGraphQLPaginationPages; page++ {
		vars := map[string]any{
			"owner":  owner,
			"name":   repo,
			"number": num,
			"first":  pageSize,
		}
		if after != "" {
			vars["after"] = after
		}

		data, err := graphqlDo(ctx, client, apiBase, query, vars)
		if err != nil {
			return nil, err
		}

		conn, err := extractNodeMap(data, "repository.pullRequest.reviews")
		if err != nil {
			return nil, err
		}
		rawNodes, ok := conn["nodes"]
		if !ok {
			return nil, fmt.Errorf("key %q has no 'nodes' field", "repository.pullRequest.reviews")
		}
		pageNodes, ok := rawNodes.([]any)
		if !ok {
			return nil, fmt.Errorf("'nodes' at %q is not an array", "repository.pullRequest.reviews")
		}

		for _, n := range pageNodes {
			nodeMap, isMap := n.(map[string]any)
			if !isMap {
				continue
			}
			var authorVal any
			if authorMap, ok := nodeMap["author"].(map[string]any); ok && authorMap != nil {
				authorVal = map[string]any{"login": authorMap["login"]}
			}
			var commitVal any
			if commitMap, ok := nodeMap["commit"].(map[string]any); ok && commitMap != nil {
				commitVal = map[string]any{"oid": commitMap["oid"]}
			}
			reviews = append(reviews, map[string]any{
				"id":          nodeMap["id"],
				"databaseId":  nodeMap["databaseId"],
				"author":      authorVal,
				"state":       nodeMap["state"],
				"body":        nodeMap["body"],
				"submittedAt": nodeMap["submittedAt"],
				"commit":      commitVal,
				"url":         nodeMap["url"],
			})
		}

		hasNext, endCursor := extractPageInfo(conn)
		if !hasNext {
			break
		}
		if endCursor == "" {
			return nil, fmt.Errorf("list_pr_reviews pagination: hasNextPage is true with empty endCursor")
		}
		if page == maxGraphQLPaginationPages-1 {
			if pageSize < defaultGraphQLPageSize {
				return nil, fmt.Errorf("list_pr_reviews: exceeded pagination safety limit (%d pages); increase per_page (up to %d)", maxGraphQLPaginationPages, defaultGraphQLPageSize)
			}
			return nil, fmt.Errorf("list_pr_reviews: exceeded pagination safety limit (%d pages)", maxGraphQLPaginationPages)
		}
		after = endCursor
	}

	return readOutput(reviews), nil
}

// ─── List Issue Timeline ─────────────────────────────────────────────────────

func (p *Provider) executeListIssueTimeline(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, err := requirePositiveIntInput(inputs, "list_issue_timeline", "number")
	if err != nil {
		return nil, err
	}

	pageSize, err := getGraphQLPageSize(inputs, "list_issue_timeline")
	if err != nil {
		return nil, err
	}

	events := make([]map[string]any, 0)
	escapedOwner := url.PathEscape(owner)
	escapedRepo := url.PathEscape(repo)

	for page := 1; page <= maxGraphQLPaginationPages; page++ {
		timelineURL := fmt.Sprintf("%s/repos/%s/%s/issues/%d/timeline?per_page=%d&page=%d", apiBase, escapedOwner, escapedRepo, num, pageSize, page)
		res, headers, err := p.doRESTRequestWithHeaders(ctx, client, http.MethodGet, timelineURL, nil)
		if err != nil {
			return nil, fmt.Errorf("listing issue timeline: %w", err)
		}

		pageSlice, ok := res.([]any)
		if !ok {
			return nil, fmt.Errorf("unexpected issue timeline response format: expected array, got %T", res)
		}
		if len(pageSlice) == 0 {
			break
		}

		for _, raw := range pageSlice {
			evMap, isMap := raw.(map[string]any)
			if !isMap {
				continue
			}
			events = append(events, normalizeTimelineEvent(evMap))
		}

		var linkHeaders []string
		if headers != nil {
			linkHeaders = headers.Values("Link")
		}
		hasNext := false
		if len(linkHeaders) > 0 {
			hasNext = restLinkHasNextPage(linkHeaders)
		} else {
			hasNext = len(pageSlice) == pageSize
		}

		if !hasNext {
			break
		}
		if page == maxGraphQLPaginationPages {
			if pageSize < defaultGraphQLPageSize {
				return nil, fmt.Errorf("list_issue_timeline: exceeded pagination safety limit (%d pages); increase per_page (up to %d)", maxGraphQLPaginationPages, defaultGraphQLPageSize)
			}
			return nil, fmt.Errorf("list_issue_timeline: exceeded pagination safety limit (%d pages)", maxGraphQLPaginationPages)
		}
	}

	return readOutput(events), nil
}

// normalizeTimelineEvent ensures every timeline event map exposes consistent
// top-level keys (event, created_at, actor, requested_reviewer, commit_id, state)
// while preserving all additional event-specific fields from the GitHub REST API.
func normalizeTimelineEvent(evMap map[string]any) map[string]any {
	if evMap["created_at"] == nil {
		if submittedAt, ok := evMap["submitted_at"]; ok && submittedAt != nil {
			evMap["created_at"] = submittedAt
		} else if authorMap, ok := evMap["author"].(map[string]any); ok && authorMap["date"] != nil {
			evMap["created_at"] = authorMap["date"]
		} else if committerMap, ok := evMap["committer"].(map[string]any); ok && committerMap["date"] != nil {
			evMap["created_at"] = committerMap["date"]
		}
	}
	if evMap["actor"] == nil {
		if userVal, ok := evMap["user"]; ok && userVal != nil {
			evMap["actor"] = userVal
		}
	}
	if evMap["commit_id"] == nil {
		if shaVal, ok := evMap["sha"]; ok && shaVal != nil {
			evMap["commit_id"] = shaVal
		}
	}
	for _, key := range []string{"event", "created_at", "actor", "requested_reviewer", "commit_id", "state"} {
		if _, exists := evMap[key]; !exists {
			evMap[key] = nil
		}
	}
	return evMap
}

func restLinkHasNextPage(linkHeaders []string) bool {
	for _, linkHeader := range linkHeaders {
		for _, part := range strings.Split(linkHeader, ",") {
			segments := strings.Split(part, ";")
			for _, seg := range segments[1:] {
				key, val, ok := strings.Cut(seg, "=")
				if !ok || !strings.EqualFold(strings.TrimSpace(key), "rel") {
					continue
				}
				unquoted := strings.Trim(strings.TrimSpace(val), `"`)
				if slices.Contains(strings.Fields(strings.ToLower(unquoted)), "next") {
					return true
				}
			}
		}
	}
	return false
}

// ─── Request Reviewers ───────────────────────────────────────────────────────

func (p *Provider) executeRequestReviewers(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, err := requirePositiveIntInput(inputs, "request_reviewers", "number")
	if err != nil {
		return nil, err
	}

	reviewers, err := extractStrictStringSlice(inputs, "request_reviewers", "reviewers")
	if err != nil {
		return nil, err
	}
	teamReviewers, err := extractStrictStringSlice(inputs, "request_reviewers", "team_reviewers")
	if err != nil {
		return nil, err
	}
	botIDs, err := extractStrictStringSlice(inputs, "request_reviewers", "bot_ids")
	if err != nil {
		return nil, err
	}

	if len(reviewers) == 0 && len(teamReviewers) == 0 && len(botIDs) == 0 {
		return nil, fmt.Errorf("request_reviewers requires at least one of 'reviewers', 'team_reviewers', or 'bot_ids'")
	}

	humanReviewers, reviewersWantCopilot := splitCopilotAliases(reviewers)
	cleanBotIDs, botsWantCopilot := splitCopilotAliases(botIDs)
	wantsCopilot := reviewersWantCopilot || botsWantCopilot

	var prNodeID string
	var restResult map[string]any
	copilotQueuedViaREST := false

	restURL := fmt.Sprintf("%s/repos/%s/%s/pulls/%d/requested_reviewers", apiBase, url.PathEscape(owner), url.PathEscape(repo), num)

	if len(humanReviewers) > 0 || len(teamReviewers) > 0 || wantsCopilot {
		restReviewers := humanReviewers
		if wantsCopilot {
			restReviewers = slices.Concat(humanReviewers, []string{copilotRESTReviewerLogin})
		}

		reqBody := make(map[string]any)
		if len(restReviewers) > 0 {
			reqBody["reviewers"] = restReviewers
		}
		if len(teamReviewers) > 0 {
			reqBody["team_reviewers"] = teamReviewers
		}

		res, restErr := p.doRESTRequest(ctx, client, http.MethodPost, restURL, reqBody)
		if restErr != nil {
			var re *restError
			if wantsCopilot && errors.As(restErr, &re) && re.StatusCode == http.StatusUnprocessableEntity {
				// Retry human and team reviewers alone so a 422 on the bot login
				// does not block requesting human or team reviewers.
				if len(humanReviewers) > 0 || len(teamReviewers) > 0 {
					humanBody := make(map[string]any)
					if len(humanReviewers) > 0 {
						humanBody["reviewers"] = humanReviewers
					}
					if len(teamReviewers) > 0 {
						humanBody["team_reviewers"] = teamReviewers
					}
					humanRes, humanErr := p.doRESTRequest(ctx, client, http.MethodPost, restURL, humanBody)
					if humanErr != nil {
						return nil, fmt.Errorf("requesting reviewers: %w", humanErr)
					}
					if m, isMap := humanRes.(map[string]any); isMap {
						restResult = m
						prNodeID, _ = m["node_id"].(string)
					}
				}
			} else {
				return nil, fmt.Errorf("requesting reviewers: %w", restErr)
			}
		} else if m, isMap := res.(map[string]any); isMap {
			restResult = m
			prNodeID, _ = m["node_id"].(string)
			if wantsCopilot {
				if matched, copilotNodeID := restResponseCopilotReviewer(m); matched {
					copilotQueuedViaREST = true
					isGitHubCom := isGitHubComAPIBase(apiBase)
					cleanBotIDs = slices.DeleteFunc(cleanBotIDs, func(id string) bool {
						return (isGitHubCom && id == defaultCopilotBotNodeID) || (copilotNodeID != "" && id == copilotNodeID)
					})
				}
			}
		}
	}

	if wantsCopilot && !copilotQueuedViaREST {
		copilotBotID, resolvedPRID, resolveErr := p.resolveCopilotBotID(ctx, client, apiBase, owner, repo, num)
		if resolveErr != nil {
			return nil, resolveErr
		}
		if prNodeID == "" && resolvedPRID != "" {
			prNodeID = resolvedPRID
		}
		if !slices.Contains(cleanBotIDs, copilotBotID) {
			cleanBotIDs = append(cleanBotIDs, copilotBotID)
		}
	}

	if len(cleanBotIDs) > 0 {
		if prNodeID == "" {
			prNodeID, err = p.resolvePullRequestID(ctx, client, apiBase, owner, repo, num)
			if err != nil {
				return nil, fmt.Errorf("resolving pull request ID: %w", err)
			}
		}

		mutation := `mutation($input: RequestReviewsInput!) {
  requestReviews(input: $input) {
    pullRequest {
      id
      number
      url
      reviewRequests(first: 20) {
        nodes {
          requestedReviewer {
            __typename
            ... on User { login }
            ... on Team { name slug }
            ... on Bot { id login }
            ... on Mannequin { login }
          }
        }
      }
    }
  }
}`
		data, gqlErr := graphqlDo(ctx, client, apiBase, mutation, map[string]any{
			"input": map[string]any{
				"pullRequestId": prNodeID,
				"botIds":        cleanBotIDs,
				"union":         true,
			},
		})
		if gqlErr != nil {
			return nil, gqlErr
		}
		gqlPR, extractErr := extractNodeMap(data, "requestReviews.pullRequest")
		if extractErr != nil {
			return nil, extractErr
		}
		flattenReviewRequests(gqlPR)
		return actionOutput("request_reviewers", gqlPR), nil
	}

	if restResult == nil {
		restResult = map[string]any{"number": num}
	}
	return actionOutput("request_reviewers", restResult), nil
}

func splitCopilotAliases(items []string) ([]string, bool) {
	if len(items) == 0 {
		return nil, false
	}
	remaining := make([]string, 0, len(items))
	found := false
	for _, item := range items {
		if isCopilotReviewerAlias(item) {
			found = true
			continue
		}
		remaining = append(remaining, item)
	}
	return remaining, found
}

func isCopilotReviewerAlias(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "@copilot", "copilot", "copilot-pull-request-reviewer", "copilot-pull-request-reviewer[bot]":
		return true
	default:
		return false
	}
}

func isCopilotReviewerBotLogin(login string) bool {
	trimmed := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
	return trimmed == "copilot-pull-request-reviewer"
}

func restResponseCopilotReviewer(restMap map[string]any) (bool, string) {
	rrList, ok := restMap["requested_reviewers"].([]any)
	if !ok {
		return false, ""
	}
	for _, item := range rrList {
		m, isMap := item.(map[string]any)
		if !isMap {
			continue
		}
		typ, _ := m["type"].(string)
		if typ != "" && !strings.EqualFold(typ, "Bot") {
			continue
		}
		isExplicitBot := strings.EqualFold(typ, "Bot")
		nodeID, _ := m["node_id"].(string)
		htmlURL, _ := m["html_url"].(string)
		if htmlURL != "" && isCopilotAppHTMLURL(htmlURL) {
			return true, nodeID
		}
		login, _ := m["login"].(string)
		if strings.EqualFold(strings.TrimSpace(login), copilotRESTReviewerLogin) {
			return true, nodeID
		}
		if (isExplicitBot || htmlURL == "") && (isCopilotReviewerBotLogin(login) || strings.EqualFold(strings.TrimSpace(login), "copilot")) {
			return true, nodeID
		}
	}
	return false, ""
}

func isCopilotAppHTMLURL(rawURL string) bool {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || parsed.Host == "" {
		return false
	}
	if !strings.EqualFold(parsed.Scheme, "http") && !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	cleanPath := strings.TrimRight(strings.ToLower(parsed.Path), "/")
	return cleanPath == "/apps/copilot-pull-request-reviewer"
}

func isGitHubComAPIBase(apiBase string) bool {
	trimmed := strings.TrimSpace(apiBase)
	if trimmed == "" {
		return true
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || !strings.EqualFold(parsed.Scheme, "https") {
		return false
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	if port := parsed.Port(); port != "" && port != "443" {
		return false
	}
	host := strings.TrimSuffix(parsed.Hostname(), ".")
	if !strings.EqualFold(host, "api.github.com") {
		return false
	}
	return strings.Trim(parsed.Path, "/") == ""
}

func sanitizeAPIBaseForError(apiBase string) string {
	parsed, err := url.Parse(strings.TrimSpace(apiBase))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "configured api_base"
	}
	parsed.User = nil
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return strings.TrimRight(parsed.String(), "/")
}

func fallbackCopilotBotID(apiBase, prNodeID string) (string, string, error) {
	if isGitHubComAPIBase(apiBase) {
		return defaultCopilotBotNodeID, prNodeID, nil
	}
	return "", prNodeID, fmt.Errorf("could not resolve Copilot reviewer bot ID from %s; pass the instance-local bot GraphQL node ID in 'bot_ids'", sanitizeAPIBaseForError(apiBase))
}

func (p *Provider) resolveCopilotBotID(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, number int) (botID, prNodeID string, err error) {
	prQuery := `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      id
      suggestedReviewerActors(first: 50, query: "copilot") {
        nodes {
          reviewer {
            __typename
            ... on Bot {
              id
              login
            }
          }
        }
      }
    }
  }
}`
	prData, prErr := graphqlDo(ctx, client, apiBase, prQuery, map[string]any{
		"owner":  owner,
		"name":   repo,
		"number": number,
	})
	if prErr != nil {
		var gqlErr *GraphQLError
		if !errors.As(prErr, &gqlErr) {
			return "", "", fmt.Errorf("resolving Copilot reviewer bot ID: %w", prErr)
		}
	}
	if prData != nil {
		if prMap, extractErr := extractNodeMap(prData, "repository.pullRequest"); extractErr == nil {
			prNodeID, _ = prMap["id"].(string)
			if conn, ok := prMap["suggestedReviewerActors"].(map[string]any); ok {
				if nodes, ok := conn["nodes"].([]any); ok {
					for _, n := range nodes {
						nm, isMap := n.(map[string]any)
						if !isMap {
							continue
						}
						rev, isMap := nm["reviewer"].(map[string]any)
						if !isMap {
							continue
						}
						if tn, _ := rev["__typename"].(string); tn != "" && tn != "Bot" {
							continue
						}
						id, _ := rev["id"].(string)
						login, _ := rev["login"].(string)
						if id != "" && isCopilotReviewerBotLogin(login) {
							return id, prNodeID, nil
						}
					}
				}
			}
		}
	}

	repoQuery := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    suggestedActors(first: 100, capabilities: [CAN_BE_ASSIGNED]) {
      nodes {
        __typename
        ... on Bot {
          id
          login
        }
      }
    }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, repoQuery, map[string]any{
		"owner": owner,
		"name":  repo,
	})
	if err != nil {
		var gqlErr *GraphQLError
		if !errors.As(err, &gqlErr) {
			return "", prNodeID, fmt.Errorf("resolving Copilot reviewer bot ID: %w", err)
		}
	}
	if data != nil {
		if nodes, extractErr := extractNodes(data, "repository.suggestedActors"); extractErr == nil {
			for _, n := range nodes {
				m, isMap := n.(map[string]any)
				if !isMap {
					continue
				}
				if tn, _ := m["__typename"].(string); tn != "" && tn != "Bot" {
					continue
				}
				id, _ := m["id"].(string)
				login, _ := m["login"].(string)
				if id != "" && isCopilotReviewerBotLogin(login) {
					return id, prNodeID, nil
				}
			}
		}
	}
	return fallbackCopilotBotID(apiBase, prNodeID)
}

func extractStrictStringSlice(inputs map[string]any, operation, key string) ([]string, error) {
	raw, exists := inputs[key]
	if !exists || raw == nil {
		return nil, nil
	}
	switch typed := raw.(type) {
	case []string:
		out := make([]string, 0, len(typed))
		for i, s := range typed {
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				return nil, fmt.Errorf("%s: %s[%d] must be a non-empty string", operation, key, i)
			}
			out = append(out, trimmed)
		}
		return out, nil
	case []any:
		out := make([]string, 0, len(typed))
		for i, item := range typed {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s: %s[%d] must be a string, got %T", operation, key, i, item)
			}
			trimmed := strings.TrimSpace(s)
			if trimmed == "" {
				return nil, fmt.Errorf("%s: %s[%d] must be a non-empty string", operation, key, i)
			}
			out = append(out, trimmed)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%s: '%s' must be an array of strings, got %T", operation, key, raw)
	}
}

// ─── Reply to Review Thread ──────────────────────────────────────────────────

func (p *Provider) executeReplyToReviewThread(ctx context.Context, client *httpc.Client, apiBase, _, _ string, inputs map[string]any) (*sdkprovider.Output, error) {
	threadID := getStringInput(inputs, "thread_id")
	if threadID == "" {
		return nil, requiredInputError("reply_to_review_thread", "thread_id", inputs, "")
	}
	body := getStringInput(inputs, "body")
	if body == "" {
		return nil, requiredInputError("reply_to_review_thread", "body", inputs, "")
	}

	mutation := `mutation($id: ID!, $body: String!) {
  addPullRequestReviewThreadReply(input: {pullRequestReviewThreadId: $id, body: $body}) {
    comment {
      id
      body
      createdAt
      author { login }
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"id": threadID, "body": body})
	if err != nil {
		return nil, err
	}

	comment, err := extractNodeMap(data, "addPullRequestReviewThreadReply.comment")
	if err != nil {
		return nil, err
	}

	return actionOutput("reply_to_review_thread", comment), nil
}

// ─── Resolve Review Thread ───────────────────────────────────────────────────

func (p *Provider) executeResolveReviewThread(ctx context.Context, client *httpc.Client, apiBase, _, _ string, inputs map[string]any) (*sdkprovider.Output, error) {
	threadID := getStringInput(inputs, "thread_id")
	if threadID == "" {
		return nil, requiredInputError("resolve_review_thread", "thread_id", inputs, "")
	}

	mutation := `mutation($threadId: ID!) {
  resolveReviewThread(input: {threadId: $threadId}) {
    thread {
      id
      isResolved
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"threadId": threadID})
	if err != nil {
		return nil, err
	}

	thread, err := extractNodeMap(data, "resolveReviewThread.thread")
	if err != nil {
		return nil, err
	}

	return actionOutput("resolve_review_thread", thread), nil
}
