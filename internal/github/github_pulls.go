// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

// ─── Create Pull Request ─────────────────────────────────────────────────────

func (p *Provider) executeCreatePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	title := getStringInput(inputs, "title")
	if title == "" {
		return nil, requiredInputError("create_pull_request", "title", inputs, "")
	}
	head := getStringInput(inputs, "head")
	if head == "" {
		return nil, requiredInputError("create_pull_request", "head", inputs, "")
	}
	base := getStringInput(inputs, "base")
	if base == "" {
		return nil, requiredInputError("create_pull_request", "base", inputs, "")
	}

	var draft *bool
	if d, ok := getBoolInput(inputs, "draft"); ok {
		draft = &d
	}

	pr, err := p.createPullRequest(ctx, client, apiBase, owner, repo, title, head, base, getStringInput(inputs, "body"), draft)
	if err != nil {
		return nil, err
	}

	return actionOutput("create_pull_request", pr), nil
}

// createPullRequest opens a pull request from head to base through the GraphQL
// createPullRequest mutation and returns the created pull request node. body is
// sent only when non-empty and draft only when non-nil (GitHub then defaults to
// a regular, non-draft pull request).
func (p *Provider) createPullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo, title, head, base, body string, draft *bool) (map[string]any, error) {
	repoID, err := p.resolveRepoID(ctx, client, apiBase, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("resolving repository ID: %w", err)
	}

	mutInput := map[string]any{
		"repositoryId": repoID,
		"title":        title,
		"headRefName":  head,
		"baseRefName":  base,
	}
	if body != "" {
		mutInput["body"] = body
	}
	if draft != nil {
		mutInput["draft"] = *draft
	}

	mutation := `mutation($input: CreatePullRequestInput!) {
  createPullRequest(input: $input) {
    pullRequest {
      id
      number
      title
      url
      state
      headRefName
      baseRefName
      isDraft
      createdAt
      author { login }
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	return extractNodeMap(data, "createPullRequest.pullRequest")
}

// findOpenPullRequest returns the open pull request (number and url) from this
// repository's head branch to base, or nil when there is none. The headRefName
// filter matches on the branch name alone, so pull requests from forks whose
// branch happens to share that name come back too; they are skipped, otherwise
// a stranger's pull request could be mistaken for ours.
func (p *Provider) findOpenPullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo, head, base string) (map[string]any, error) {
	query := `query($owner: String!, $name: String!, $head: String!, $base: String!) {
  repository(owner: $owner, name: $name) {
    pullRequests(headRefName: $head, baseRefName: $base, states: [OPEN], first: 20) {
      nodes { number url isCrossRepository }
    }
  }
}`
	vars := map[string]any{"owner": owner, "name": repo, "head": head, "base": base}
	data, err := graphqlDo(ctx, client, apiBase, query, vars)
	if err != nil {
		return nil, err
	}

	nodes, err := extractNodes(data, "repository.pullRequests")
	if err != nil {
		return nil, err
	}
	for _, node := range nodes {
		pr, ok := node.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("unexpected pull request node format")
		}
		if cross, _ := pr["isCrossRepository"].(bool); cross {
			continue
		}
		return pr, nil
	}
	return nil, nil
}

// ─── Update Pull Request ─────────────────────────────────────────────────────

func (p *Provider) executeUpdatePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, ok := getIntInput(inputs, "number")
	if !ok || num == 0 {
		return nil, requiredInputError("update_pull_request", "number", inputs, "")
	}

	prID, err := p.resolvePullRequestID(ctx, client, apiBase, owner, repo, num)
	if err != nil {
		return nil, fmt.Errorf("resolving pull request ID: %w", err)
	}

	mutInput := map[string]any{
		"pullRequestId": prID,
	}
	if title := getStringInput(inputs, "title"); title != "" {
		mutInput["title"] = title
	}
	if body := getStringInput(inputs, "body"); body != "" {
		mutInput["body"] = body
	}
	if base := getStringInput(inputs, "base"); base != "" {
		mutInput["baseRefName"] = base
	}

	mutation := `mutation($input: UpdatePullRequestInput!) {
  updatePullRequest(input: $input) {
    pullRequest {
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

	pr, err := extractNodeMap(data, "updatePullRequest.pullRequest")
	if err != nil {
		return nil, err
	}

	return actionOutput("update_pull_request", pr), nil
}

// ─── Merge Pull Request ──────────────────────────────────────────────────────

func (p *Provider) executeMergePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, ok := getIntInput(inputs, "number")
	if !ok || num == 0 {
		return nil, requiredInputError("merge_pull_request", "number", inputs, "")
	}

	prID, err := p.resolvePullRequestID(ctx, client, apiBase, owner, repo, num)
	if err != nil {
		return nil, fmt.Errorf("resolving pull request ID: %w", err)
	}

	mutInput := map[string]any{
		"pullRequestId": prID,
	}

	mergeMethod := getStringInput(inputs, "merge_method")
	if mergeMethod == "" {
		mergeMethod = "MERGE"
	}
	mutInput["mergeMethod"] = mergeMethod

	if commitTitle := getStringInput(inputs, "commit_title"); commitTitle != "" {
		mutInput["commitHeadline"] = commitTitle
	}
	if commitMessage := getStringInput(inputs, "commit_message"); commitMessage != "" {
		mutInput["commitBody"] = commitMessage
	}

	mutation := `mutation($input: MergePullRequestInput!) {
  mergePullRequest(input: $input) {
    pullRequest {
      id
      number
      title
      url
      state
      merged
      mergedAt
      mergeCommit { oid }
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	pr, err := extractNodeMap(data, "mergePullRequest.pullRequest")
	if err != nil {
		return nil, err
	}

	return actionOutput("merge_pull_request", pr), nil
}

// ─── Close Pull Request ──────────────────────────────────────────────────────

func (p *Provider) executeClosePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	num, ok := getIntInput(inputs, "number")
	if !ok || num == 0 {
		return nil, requiredInputError("close_pull_request", "number", inputs, "")
	}

	prID, err := p.resolvePullRequestID(ctx, client, apiBase, owner, repo, num)
	if err != nil {
		return nil, fmt.Errorf("resolving pull request ID: %w", err)
	}

	mutation := `mutation($input: ClosePullRequestInput!) {
  closePullRequest(input: $input) {
    pullRequest {
      id
      number
      title
      url
      state
      closedAt
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": map[string]any{"pullRequestId": prID}})
	if err != nil {
		return nil, err
	}

	pr, err := extractNodeMap(data, "closePullRequest.pullRequest")
	if err != nil {
		return nil, err
	}

	return actionOutput("close_pull_request", pr), nil
}
