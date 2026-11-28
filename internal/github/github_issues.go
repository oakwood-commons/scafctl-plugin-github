// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

// ─── Create Issue ────────────────────────────────────────────────────────────

func (p *Provider) executeCreateIssue(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	title := getStringInput(inputs, "title")
	if title == "" {
		return nil, requiredInputError("create_issue", "title", inputs, "")
	}

	// First, resolve the repository node ID
	repoID, err := p.resolveRepoID(ctx, client, apiBase, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("resolving repository ID: %w", err)
	}

	// Build mutation input
	mutInput := map[string]any{
		"repositoryId": repoID,
		"title":        title,
	}
	if body := getStringInput(inputs, "body"); body != "" {
		mutInput["body"] = body
	}

	// Resolve label IDs if provided
	labelNames := getStringSliceInput(inputs, "labels")
	if len(labelNames) > 0 {
		labelIDs, err := p.resolveLabelIDs(ctx, client, apiBase, owner, repo, labelNames)
		if err != nil {
			return nil, fmt.Errorf("resolving label IDs: %w", err)
		}
		if len(labelIDs) > 0 {
			mutInput["labelIds"] = labelIDs
		}
	}

	// Resolve assignee IDs if provided
	assigneeLogins := getStringSliceInput(inputs, "assignees")
	if len(assigneeLogins) > 0 {
		assigneeIDs, err := p.resolveUserIDs(ctx, client, apiBase, assigneeLogins)
		if err != nil {
			return nil, fmt.Errorf("resolving assignee IDs: %w", err)
		}
		if len(assigneeIDs) > 0 {
			mutInput["assigneeIds"] = assigneeIDs
		}
	}

	mutation := `mutation($input: CreateIssueInput!) {
  createIssue(input: $input) {
    issue {
      id
      number
      title
      url
      state
      createdAt
      author { login }
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	issue, err := extractNodeMap(data, "createIssue.issue")
	if err != nil {
		return nil, err
	}

	return actionOutput("create_issue", issue), nil
}

// ─── Update Issue ────────────────────────────────────────────────────────────

func (p *Provider) executeUpdateIssue(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, ok := getIntInput(inputs, "number")
	if !ok || num == 0 {
		return nil, requiredInputError("update_issue", "number", inputs, "")
	}

	// Resolve issue node ID
	issueID, err := p.resolveIssueID(ctx, client, apiBase, owner, repo, num)
	if err != nil {
		return nil, fmt.Errorf("resolving issue ID: %w", err)
	}

	mutInput := map[string]any{
		"id": issueID,
	}
	if title := getStringInput(inputs, "title"); title != "" {
		mutInput["title"] = title
	}
	if body := getStringInput(inputs, "body"); body != "" {
		mutInput["body"] = body
	}
	if state := getStringInput(inputs, "state"); state != "" {
		mutInput["state"] = mapIssueStateForMutation(state)
	}
	if stateReason := getStringInput(inputs, "state_reason"); stateReason != "" {
		mappedReason, err := mapIssueStateReason(stateReason)
		if err != nil {
			return nil, err
		}
		mutInput["stateReason"] = mappedReason
	}

	// Resolve label IDs if provided
	labelNames := getStringSliceInput(inputs, "labels")
	if len(labelNames) > 0 {
		labelIDs, err := p.resolveLabelIDs(ctx, client, apiBase, owner, repo, labelNames)
		if err != nil {
			return nil, fmt.Errorf("resolving label IDs: %w", err)
		}
		if len(labelIDs) > 0 {
			mutInput["labelIds"] = labelIDs
		}
	}

	// Resolve assignee IDs if provided
	assigneeLogins := getStringSliceInput(inputs, "assignees")
	if len(assigneeLogins) > 0 {
		assigneeIDs, err := p.resolveUserIDs(ctx, client, apiBase, assigneeLogins)
		if err != nil {
			return nil, fmt.Errorf("resolving assignee IDs: %w", err)
		}
		if len(assigneeIDs) > 0 {
			mutInput["assigneeIds"] = assigneeIDs
		}
	}

	mutation := `mutation($input: UpdateIssueInput!) {
  updateIssue(input: $input) {
    issue {
      id
      number
      title
      url
      state
      updatedAt
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	issue, err := extractNodeMap(data, "updateIssue.issue")
	if err != nil {
		return nil, err
	}

	return actionOutput("update_issue", issue), nil
}

// ─── Create Issue Comment ────────────────────────────────────────────────────

func (p *Provider) executeCreateIssueComment(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, ok := getIntInput(inputs, "number")
	if !ok || num == 0 {
		return nil, requiredInputError("create_issue_comment", "number", inputs, "")
	}
	body := getStringInput(inputs, "body")
	if body == "" {
		return nil, requiredInputError("create_issue_comment", "body", inputs, "")
	}

	// Resolve issue node ID
	issueID, err := p.resolveIssueID(ctx, client, apiBase, owner, repo, num)
	if err != nil {
		return nil, fmt.Errorf("resolving issue ID: %w", err)
	}

	mutation := `mutation($input: AddCommentInput!) {
  addComment(input: $input) {
    commentEdge {
      node {
        id
        body
        createdAt
        author { login }
        url
      }
    }
  }
}`

	mutInput := map[string]any{
		"subjectId": issueID,
		"body":      body,
	}

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	comment, err := extractNodeMap(data, "addComment.commentEdge.node")
	if err != nil {
		return nil, err
	}

	return actionOutput("create_issue_comment", comment), nil
}

// ─── Node ID Resolution Helpers ──────────────────────────────────────────────

// resolveRepoID fetches the GraphQL node ID for a repository.
func (p *Provider) resolveRepoID(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (string, error) {
	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) { id }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo})
	if err != nil {
		return "", err
	}
	repoNode, err := extractNodeMap(data, "repository")
	if err != nil {
		return "", err
	}
	id, ok := repoNode["id"].(string)
	if !ok {
		return "", fmt.Errorf("repository ID not found")
	}
	return id, nil
}

// resolveIssueID fetches the GraphQL node ID for an issue by number.
func (p *Provider) resolveIssueID(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, number int) (string, error) {
	query := `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    issue(number: $number) { id }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo, "number": number})
	if err != nil {
		return "", err
	}
	issue, err := extractNodeMap(data, "repository.issue")
	if err != nil {
		return "", err
	}
	id, ok := issue["id"].(string)
	if !ok {
		return "", fmt.Errorf("issue #%d ID not found", number)
	}
	return id, nil
}

// resolvePullRequestID fetches the GraphQL node ID for a pull request by number.
func (p *Provider) resolvePullRequestID(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, number int) (string, error) {
	query := `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) { id }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo, "number": number})
	if err != nil {
		return "", err
	}
	pr, err := extractNodeMap(data, "repository.pullRequest")
	if err != nil {
		return "", err
	}
	id, ok := pr["id"].(string)
	if !ok {
		return "", fmt.Errorf("pull request #%d ID not found", number)
	}
	return id, nil
}

// resolveLabelIDs fetches GraphQL node IDs for labels by name.
func (p *Provider) resolveLabelIDs(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, names []string) ([]string, error) {
	// Fetch all labels for the repo and filter by name
	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    labels(first: 100) {
      nodes { id name }
    }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo})
	if err != nil {
		return nil, err
	}
	nodes, err := extractNodes(data, "repository.labels")
	if err != nil {
		return nil, err
	}

	// Build a name→ID map
	nameToID := make(map[string]string)
	for _, node := range nodes {
		if m, ok := node.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				if id, ok := m["id"].(string); ok {
					nameToID[n] = id
				}
			}
		}
	}

	var ids []string
	for _, name := range names {
		if id, ok := nameToID[name]; ok {
			ids = append(ids, id)
		}
		// Silently skip labels that don't exist
	}
	return ids, nil
}

// resolveUserIDs fetches GraphQL node IDs for users by login.
func (p *Provider) resolveUserIDs(ctx context.Context, client *httpc.Client, apiBase string, logins []string) ([]string, error) {
	ids := make([]string, 0, len(logins))
	for _, login := range logins {
		query := `query($login: String!) {
  user(login: $login) { id }
}`
		data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"login": login})
		if err != nil {
			return nil, fmt.Errorf("resolving user %q: %w", login, err)
		}
		user, err := extractNodeMap(data, "user")
		if err != nil {
			return nil, fmt.Errorf("resolving user %q: %w", login, err)
		}
		if id, ok := user["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// resolveRefID fetches the GraphQL node ID for a ref (branch or tag).
func (p *Provider) resolveRefID(ctx context.Context, client *httpc.Client, apiBase, owner, repo, qualifiedName string) (string, error) {
	query := `query($owner: String!, $name: String!, $qualifiedName: String!) {
  repository(owner: $owner, name: $name) {
    ref(qualifiedName: $qualifiedName) { id }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo, "qualifiedName": qualifiedName})
	if err != nil {
		return "", err
	}
	ref, err := extractNode(data, "repository.ref")
	if err != nil {
		return "", err
	}
	if ref == nil {
		return "", fmt.Errorf("ref %q not found", qualifiedName)
	}
	refMap, ok := ref.(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected ref format")
	}
	id, ok := refMap["id"].(string)
	if !ok {
		return "", fmt.Errorf("ref %q ID not found", qualifiedName)
	}
	return id, nil
}

// mapIssueStateForMutation converts user state to GraphQL IssueState enum for mutations.
func mapIssueStateForMutation(state string) string {
	switch state {
	case "open", "OPEN":
		return "OPEN"
	case "closed", "CLOSED":
		return "CLOSED"
	default:
		return state
	}
}

// mapIssueStateReason converts user-friendly state reason to GraphQL IssueClosedStateReason enum.
// Returns an error for unrecognized values to fail fast rather than sending invalid data to the API.
func mapIssueStateReason(reason string) (string, error) {
	switch strings.ToUpper(reason) {
	case "COMPLETED":
		return "COMPLETED", nil
	case "NOT_PLANNED":
		return "NOT_PLANNED", nil
	case "REOPENED":
		return "REOPENED", nil
	default:
		return "", fmt.Errorf("invalid state_reason %q: must be one of completed, not_planned, reopened", reason)
	}
}

// extractOptionalTrimmedString validates that an optional input, if supplied and non-nil,
// is a string and returns its trimmed value.
func extractOptionalTrimmedString(inputs map[string]any, operation, key string) (string, error) {
	raw, exists := inputs[key]
	if !exists || raw == nil {
		return "", nil
	}
	s, ok := raw.(string)
	if !ok {
		return "", fmt.Errorf("%s: '%s' must be a string, got %T", operation, key, raw)
	}
	return strings.TrimSpace(s), nil
}

// mapIssueOrderField converts user-friendly sort names to GitHub GraphQL IssueOrderField values.
func mapIssueOrderField(sortField string) (string, error) {
	switch strings.ToLower(sortField) {
	case "", "created", "created_at":
		return "CREATED_AT", nil
	case "updated", "updated_at":
		return "UPDATED_AT", nil
	case "comments":
		return "COMMENTS", nil
	default:
		return "", fmt.Errorf("list_issues: invalid 'sort' %q: must be one of created, updated, comments", sortField)
	}
}

// mapOrderDirection converts user-friendly direction strings to GitHub GraphQL OrderDirection values.
func mapOrderDirection(direction string) (string, error) {
	switch strings.ToLower(direction) {
	case "", "desc":
		return "DESC", nil
	case "asc":
		return "ASC", nil
	default:
		return "", fmt.Errorf("list_issues: invalid 'direction' %q: must be one of asc, desc", direction)
	}
}

// buildIssueFilters constructs a GitHub GraphQL IssueFilters map from list_issues inputs.
// When a milestone title filter does not match any milestone in the repository, emptyMatch
// is returned as true so the caller can return an empty result slice without an extra query.
func (p *Provider) buildIssueFilters(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (map[string]any, bool, error) {
	filterBy := make(map[string]any)

	labels, err := extractStrictStringSlice(inputs, "list_issues", "labels")
	if err != nil {
		return nil, false, err
	}
	if len(labels) > 0 {
		filterBy["labels"] = labels
	}

	assignee, err := extractOptionalTrimmedString(inputs, "list_issues", "assignee")
	if err != nil {
		return nil, false, err
	}
	if assignee != "" {
		if strings.EqualFold(assignee, "none") {
			filterBy["assignee"] = nil
		} else {
			filterBy["assignee"] = assignee
		}
	}

	creator, err := extractOptionalTrimmedString(inputs, "list_issues", "creator")
	if err != nil {
		return nil, false, err
	}
	if creator != "" {
		filterBy["createdBy"] = creator
	}

	mentioned, err := extractOptionalTrimmedString(inputs, "list_issues", "mentioned")
	if err != nil {
		return nil, false, err
	}
	if mentioned != "" {
		filterBy["mentioned"] = mentioned
	}

	since, err := extractOptionalTrimmedString(inputs, "list_issues", "since")
	if err != nil {
		return nil, false, err
	}
	if since != "" {
		if _, parseErr := time.Parse(time.RFC3339Nano, since); parseErr != nil {
			return nil, false, fmt.Errorf("list_issues: 'since' must be a valid RFC 3339 timestamp: %w", parseErr)
		}
		filterBy["since"] = since
	}

	if rawMilestone, exists := inputs["milestone"]; exists && rawMilestone != nil {
		switch mVal := rawMilestone.(type) {
		case string:
			trimmed := strings.TrimSpace(mVal)
			if trimmed != "" {
				switch {
				case strings.EqualFold(trimmed, "none"):
					filterBy["milestoneNumber"] = nil
				case trimmed == "*":
					filterBy["milestoneNumber"] = "*"
				default:
					if parsedNum, parseErr := strconv.Atoi(trimmed); parseErr == nil {
						if parsedNum < 1 {
							return nil, false, fmt.Errorf("list_issues: 'milestone' number must be >= 1, got %q", trimmed)
						}
						filterBy["milestoneNumber"] = strconv.Itoa(parsedNum)
					} else {
						resolvedNum, found, resolveErr := p.resolveMilestoneNumberByTitle(ctx, client, apiBase, owner, repo, trimmed)
						if resolveErr != nil {
							return nil, false, resolveErr
						}
						if !found {
							return nil, true, nil
						}
						filterBy["milestoneNumber"] = strconv.Itoa(resolvedNum)
					}
				}
			}
		default:
			num, ok := getIntInput(inputs, "milestone")
			if !ok {
				return nil, false, fmt.Errorf("list_issues: 'milestone' must be a string or integer, got %T", rawMilestone)
			}
			if num < 1 {
				return nil, false, fmt.Errorf("list_issues: 'milestone' number must be >= 1, got %d", num)
			}
			filterBy["milestoneNumber"] = strconv.Itoa(num)
		}
	}

	return filterBy, false, nil
}

// resolveMilestoneNumberByTitle resolves a milestone's integer number from its title.
func (p *Provider) resolveMilestoneNumberByTitle(ctx context.Context, client *httpc.Client, apiBase, owner, repo, title string) (int, bool, error) {
	query := `query($owner: String!, $name: String!, $first: Int!, $after: String, $query: String!) {
  repository(owner: $owner, name: $name) {
    milestones(first: $first, after: $after, query: $query) {
      nodes {
        number
        title
      }
      pageInfo {
        hasNextPage
        endCursor
      }
    }
  }
}`
	var after string
	var fallbackNum int

	for page := 0; page < maxGraphQLPaginationPages; page++ {
		vars := map[string]any{
			"owner": owner,
			"name":  repo,
			"first": defaultGraphQLPageSize,
			"query": title,
		}
		if after != "" {
			vars["after"] = after
		}

		data, err := graphqlDo(ctx, client, apiBase, query, vars)
		if err != nil {
			return 0, false, fmt.Errorf("resolving milestone %q: %w", title, err)
		}

		conn, err := extractNodeMap(data, "repository.milestones")
		if err != nil {
			return 0, false, fmt.Errorf("resolving milestone %q: %w", title, err)
		}
		rawNodes, ok := conn["nodes"]
		if !ok {
			return 0, false, fmt.Errorf("resolving milestone %q: key %q has no 'nodes' field", title, "repository.milestones")
		}
		nodes, ok := rawNodes.([]any)
		if !ok {
			return 0, false, fmt.Errorf("resolving milestone %q: 'nodes' at %q is not an array", title, "repository.milestones")
		}

		for _, n := range nodes {
			m, isMap := n.(map[string]any)
			if !isMap {
				continue
			}
			num, ok := getIntInput(m, "number")
			if !ok || num < 1 {
				continue
			}
			nodeTitle, _ := m["title"].(string)
			if nodeTitle == title {
				return num, true, nil
			}
			if fallbackNum == 0 && strings.EqualFold(nodeTitle, title) {
				fallbackNum = num
			}
		}

		hasNext, endCursor := extractPageInfo(conn)
		if !hasNext {
			break
		}
		if endCursor == "" {
			return 0, false, fmt.Errorf("resolving milestone %q: hasNextPage is true with empty endCursor", title)
		}
		if page == maxGraphQLPaginationPages-1 {
			return 0, false, fmt.Errorf("resolving milestone %q: exceeded pagination safety limit (%d pages)", title, maxGraphQLPaginationPages)
		}
		after = endCursor
	}

	if fallbackNum > 0 {
		return fallbackNum, true, nil
	}
	return 0, false, nil
}
