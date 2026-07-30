// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

// Repository management operations use the GraphQL API where mutations are
// available (create_repo) and the REST API for repository rulesets and
// security features (no GraphQL mutations).

// ─── Create Repository (GraphQL) ─────────────────────────────────────────────

func (p *Provider) executeCreateRepo(ctx context.Context, client *httpc.Client, apiBase string, inputs map[string]any) (*sdkprovider.Output, error) {
	name := getStringInput(inputs, "repo")
	if name == "" {
		return nil, requiredInputError("create_repo", "repo", inputs, "")
	}

	owner := getStringInput(inputs, "owner")
	autoInit, _ := getBoolInput(inputs, "auto_init")

	// Try GraphQL first (lower permission requirement for most accounts).
	// Enterprise Managed Users (EMU) cannot use GraphQL mutations, so fall
	// back to REST if we get a FORBIDDEN error.
	output, err := p.executeCreateRepoGraphQL(ctx, client, apiBase, inputs, name)
	if err != nil && isGraphQLForbidden(err) {
		output, err = p.executeCreateRepoREST(ctx, client, apiBase, inputs, name, autoInit)
		if err != nil {
			return nil, err
		}
		// Derive the canonical owner from the REST response — /user/repos
		// creates the repo under the authenticated user, which may differ
		// from the caller-provided owner.
		restOwner := extractRESTOwner(output)
		if restOwner == "" {
			restOwner = owner
		}
		// Wait for GraphQL write access before returning — downstream
		// operations (create_commit) need it.
		if waitErr := p.waitForWriteAccess(ctx, client, apiBase, restOwner, name); waitErr != nil {
			return nil, waitErr
		}
		return output, nil
	}
	if err != nil {
		return nil, err
	}

	// Extract the resolved nameWithOwner from the GraphQL response — this
	// is the canonical "owner/repo" regardless of what the caller passed.
	nwo := extractNameWithOwner(output)
	if nwo == "" {
		return nil, fmt.Errorf("GraphQL createRepository response missing nameWithOwner")
	}
	resolvedOwner, _, _ := strings.Cut(nwo, "/")

	// GraphQL createRepository doesn't support auto_init, so create an
	// initial README via the Contents API to establish the default branch.
	if autoInit {
		if initErr := p.initRepoWithReadme(ctx, client, apiBase, nwo); initErr != nil {
			return nil, fmt.Errorf("initializing repository with README: %w", initErr)
		}
	}

	// Wait for GraphQL write access before returning — downstream
	// operations (create_commit) need it.
	if waitErr := p.waitForWriteAccess(ctx, client, apiBase, resolvedOwner, name); waitErr != nil {
		return nil, waitErr
	}

	return output, nil
}

// extractRESTOwner extracts the repository owner from a REST API create_repo
// response by parsing the "full_name" field ("owner/repo").
func extractRESTOwner(output *sdkprovider.Output) string {
	if output == nil {
		return ""
	}
	dataMap, ok := output.Data.(map[string]any)
	if !ok {
		return ""
	}
	resultData, ok := dataMap["result"].(map[string]any)
	if !ok {
		return ""
	}
	fullName, _ := resultData["full_name"].(string)
	if owner, _, ok := strings.Cut(fullName, "/"); ok {
		return owner
	}
	return ""
}

// extractNameWithOwner extracts the "owner/repo" string from a create_repo output.
func extractNameWithOwner(output *sdkprovider.Output) string {
	if output == nil {
		return ""
	}
	dataMap, ok := output.Data.(map[string]any)
	if !ok {
		return ""
	}
	resultData, ok := dataMap["result"].(map[string]any)
	if !ok {
		return ""
	}
	v, _ := resultData["nameWithOwner"].(string)
	return v
}

// isGraphQLForbidden checks whether an error is a GraphQL FORBIDDEN error
// (e.g., Enterprise Managed User restrictions).
func isGraphQLForbidden(err error) bool {
	var gqlErr *GraphQLError
	if errors.As(err, &gqlErr) {
		for _, e := range gqlErr.Errors {
			if e.Type == "FORBIDDEN" {
				return true
			}
		}
	}
	return false
}

// waitForWriteAccess polls the repository's viewerPermission via GraphQL until
// the authenticated user has WRITE, MAINTAIN, or ADMIN access. This handles the
// eventual consistency window after repo creation (especially for EMU/org repos)
// where GraphQL read access propagates before write access.
func (p *Provider) waitForWriteAccess(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) error {
	lgr := logr.FromContextOrDiscard(ctx)

	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    viewerPermission
  }
}`
	vars := map[string]any{"owner": owner, "name": repo}

	for attempt := range p.waitMaxAttempts {
		if attempt > 0 {
			timer := time.NewTimer(p.waitPollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}

		data, err := graphqlDo(ctx, client, apiBase, query, vars)
		if err != nil {
			lgr.V(1).Info("waitForWriteAccess: GraphQL query failed", "attempt", attempt+1, "error", err)
			continue
		}

		permNode, _ := extractNode(data, "repository.viewerPermission")
		perm, _ := permNode.(string)
		lgr.V(1).Info("waitForWriteAccess: checked permission", "attempt", attempt+1, "viewerPermission", perm)
		if perm == "ADMIN" || perm == "WRITE" || perm == "MAINTAIN" {
			return nil
		}
	}
	return fmt.Errorf("timed out waiting for write access to %s/%s via GraphQL after %d attempts", owner, repo, p.waitMaxAttempts)
}

// executeCreateRepoGraphQL creates a repository using the GraphQL createRepository mutation.
func (p *Provider) executeCreateRepoGraphQL(ctx context.Context, client *httpc.Client, apiBase string, inputs map[string]any, name string) (*sdkprovider.Output, error) {
	mutInput := map[string]any{
		"name":       name,
		"visibility": "PRIVATE",
	}

	if desc := getStringInput(inputs, "description"); desc != "" {
		mutInput["description"] = desc
	}

	switch strings.ToLower(getStringInput(inputs, "visibility")) {
	case "public":
		mutInput["visibility"] = "PUBLIC"
	case "internal":
		mutInput["visibility"] = "INTERNAL"
	}

	// If owner is specified, resolve its node ID and set ownerId on the mutation input.
	// The repositoryOwner GraphQL field resolves both users and organizations.
	owner := getStringInput(inputs, "owner")
	if owner != "" {
		orgID, err := p.resolveOwnerID(ctx, client, apiBase, owner)
		if err != nil {
			return nil, fmt.Errorf("resolving owner ID for %q: %w", owner, err)
		}
		mutInput["ownerId"] = orgID
	}

	mutation := `mutation($input: CreateRepositoryInput!) {
  createRepository(input: $input) {
    repository {
      id
      name
      nameWithOwner
      url
      isPrivate
      defaultBranchRef { name }
      createdAt
    }
  }
}`

	data, err := graphqlDo(ctx, client, apiBase, mutation, map[string]any{"input": mutInput})
	if err != nil {
		return nil, err
	}

	repoNode, err := extractNodeMap(data, "createRepository.repository")
	if err != nil {
		return nil, err
	}

	return actionOutput("create_repo", repoNode), nil
}

// initRepoWithReadme creates an initial README.md via the Contents API to
// establish the default branch on an empty repository. This only requires
// `repo` scope, unlike POST /orgs/{org}/repos which requires org admin.
// nameWithOwner is the full "owner/repo" path (e.g. "oakwood-commons/my-repo").
func (p *Provider) initRepoWithReadme(ctx context.Context, client *httpc.Client, apiBase, nameWithOwner string) error {
	repoName := nameWithOwner
	if idx := strings.LastIndex(nameWithOwner, "/"); idx >= 0 {
		repoName = nameWithOwner[idx+1:]
	}

	content := base64.StdEncoding.EncodeToString([]byte("# " + repoName + "\n"))
	url := fmt.Sprintf("%s/repos/%s/contents/%s", apiBase, nameWithOwner, "README.md")
	reqBody := map[string]any{
		"message": "Initial commit",
		"content": content,
	}

	// The repository was just created — the REST API may not see it
	// immediately due to eventual consistency. Retry on 404 only.
	var lastErr error
	for attempt := range p.initRepoMaxRetries {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * p.initRepoRetryBackoff)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
		_, lastErr = p.doRESTRequest(ctx, client, http.MethodPut, url, reqBody)
		if lastErr == nil {
			return nil
		}
		// Only retry on 404 (eventual consistency); fail immediately on other errors
		var restErr *restError
		if !errors.As(lastErr, &restErr) || restErr.StatusCode != http.StatusNotFound {
			return lastErr
		}
	}
	return lastErr
}

// executeCreateRepoREST creates a repository using the REST API.
// Used as fallback when GraphQL is forbidden (e.g., Enterprise Managed Users).
// Tries the org endpoint first; falls back to POST /user/repos on 404 (when
// the owner is a user account, not an organization).
func (p *Provider) executeCreateRepoREST(ctx context.Context, client *httpc.Client, apiBase string, inputs map[string]any, name string, autoInit bool) (*sdkprovider.Output, error) {
	owner := getStringInput(inputs, "owner")
	if owner == "" {
		return nil, requiredInputError("create_repo", "owner", inputs, "GraphQL createRepository was forbidden, falling back to REST")
	}

	reqBody := map[string]any{
		"name":      name,
		"auto_init": autoInit,
	}

	if desc := getStringInput(inputs, "description"); desc != "" {
		reqBody["description"] = desc
	}

	visibility := strings.ToLower(getStringInput(inputs, "visibility"))
	if visibility == "" {
		visibility = "private"
	}
	reqBody["visibility"] = visibility

	// Try org endpoint first.
	orgURL := fmt.Sprintf("%s/orgs/%s/repos", apiBase, owner)
	result, err := p.doRESTRequest(ctx, client, http.MethodPost, orgURL, reqBody)
	if err != nil {
		// If 404, the owner is a user account — fall back to /user/repos.
		var restErr *restError
		if errors.As(err, &restErr) && restErr.StatusCode == http.StatusNotFound {
			userURL := fmt.Sprintf("%s/user/repos", apiBase)
			result, err = p.doRESTRequest(ctx, client, http.MethodPost, userURL, reqBody)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}

	return actionOutput("create_repo", result), nil
}

// resolveOwnerID fetches the GraphQL node ID for a user or organization by login.
func (p *Provider) resolveOwnerID(ctx context.Context, client *httpc.Client, apiBase, login string) (string, error) {
	query := `query($login: String!) {
  repositoryOwner(login: $login) { id }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"login": login})
	if err != nil {
		return "", err
	}
	ownerNode, err := extractNodeMap(data, "repositoryOwner")
	if err != nil {
		return "", fmt.Errorf("owner %q not found: %w", login, err)
	}
	id, ok := ownerNode["id"].(string)
	if !ok {
		return "", fmt.Errorf("owner ID not found for %q", login)
	}
	return id, nil
}

// ─── Create Ruleset (REST) ───────────────────────────────────────────────────
//
// Repository rulesets replace legacy branch protection rules and tag protection.
// They support branch and tag targets with flexible rule composition.
// The REST API is used because the GraphQL mutations for rulesets have complex
// nested union input types that don't map cleanly to a flat input schema.

func (p *Provider) executeCreateRuleset(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	rulesetName := getStringInput(inputs, "ruleset_name")
	if rulesetName == "" {
		return nil, requiredInputError("create_ruleset", "ruleset_name", inputs, "")
	}

	target := getStringInput(inputs, "target")
	if target == "" {
		target = "branch"
	}

	enforcement := getStringInput(inputs, "enforcement")
	if enforcement == "" {
		enforcement = "active"
	}

	// Build conditions from include/exclude ref patterns
	includeRefs := getStringSliceInput(inputs, "include_refs")
	excludeRefs := getStringSliceInput(inputs, "exclude_refs")
	if len(includeRefs) == 0 {
		return nil, requiredInputError("create_ruleset", "include_refs", inputs, `e.g. ["refs/heads/main"]`)
	}

	// Ensure exclude is never null (API requires an array)
	if excludeRefs == nil {
		excludeRefs = []string{}
	}

	conditions := map[string]any{
		"ref_name": map[string]any{
			"include": includeRefs,
			"exclude": excludeRefs,
		},
	}

	// Build rules from individual boolean/parameter fields
	rules := buildRulesetRules(inputs)

	reqBody := map[string]any{
		"name":        rulesetName,
		"target":      target,
		"enforcement": enforcement,
		"conditions":  conditions,
		"rules":       rules,
	}

	url := fmt.Sprintf("%s/repos/%s/%s/rulesets", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPost, url, reqBody)
	if err != nil {
		return nil, err
	}

	return actionOutput("create_ruleset", result), nil
}

// buildRulesetRules converts flat input fields into the GitHub Rulesets rules array.
func buildRulesetRules(inputs map[string]any) []map[string]any {
	rules := make([]map[string]any, 0)

	// Required status checks
	if contexts := getStringSliceInput(inputs, "required_status_checks_contexts"); len(contexts) > 0 {
		checks := make([]map[string]any, 0, len(contexts))
		for _, name := range contexts {
			checks = append(checks, map[string]any{"context": name})
		}
		rules = append(rules, map[string]any{
			"type": "required_status_checks",
			"parameters": map[string]any{
				"required_status_checks":               checks,
				"strict_required_status_checks_policy": true,
			},
		})
	}

	// Pull request reviews
	if approvals, ok := getIntInput(inputs, "required_approving_review_count"); ok && approvals > 0 {
		rules = append(rules, map[string]any{
			"type": "pull_request",
			"parameters": map[string]any{
				"required_approving_review_count":   approvals,
				"dismiss_stale_reviews_on_push":     true,
				"require_code_owner_review":         false,
				"require_last_push_approval":        false,
				"required_review_thread_resolution": true,
			},
		})
	}

	// Required signatures
	if v, ok := getBoolInput(inputs, "requires_commit_signatures"); ok && v {
		rules = append(rules, map[string]any{
			"type": "required_signatures",
		})
	}

	// Required linear history
	if v, ok := getBoolInput(inputs, "required_linear_history"); ok && v {
		rules = append(rules, map[string]any{
			"type": "required_linear_history",
		})
	}

	// Prevent force pushes (non_fast_forward)
	if v, ok := getBoolInput(inputs, "allow_force_pushes"); ok && !v {
		rules = append(rules, map[string]any{
			"type": "non_fast_forward",
		})
	}

	// Prevent deletion
	if v, ok := getBoolInput(inputs, "allow_deletions"); ok && !v {
		rules = append(rules, map[string]any{
			"type": "deletion",
		})
	}

	return rules
}

// ─── Enable Vulnerability Alerts (REST) ──────────────────────────────────────
//
// Vulnerability alerts have no GraphQL mutation — REST only.

func (p *Provider) executeEnableVulnerabilityAlerts(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (*sdkprovider.Output, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/vulnerability-alerts", apiBase, owner, repo)
	_, err := p.doRESTRequest(ctx, client, http.MethodPut, url, nil)
	if err != nil {
		return nil, err
	}

	return actionOutput("enable_vulnerability_alerts", map[string]any{
		"enabled": true,
	}), nil
}

// ─── Enable Automated Security Fixes (REST) ──────────────────────────────────
//
// Automated security fixes have no GraphQL mutation — REST only.

func (p *Provider) executeEnableAutomatedSecurityFixes(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (*sdkprovider.Output, error) {
	url := fmt.Sprintf("%s/repos/%s/%s/automated-security-fixes", apiBase, owner, repo)
	_, err := p.doRESTRequest(ctx, client, http.MethodPut, url, nil)
	if err != nil {
		return nil, err
	}

	return actionOutput("enable_automated_security_fixes", map[string]any{
		"enabled": true,
	}), nil
}

// ─── Update Repository ───────────────────────────────────────────────────────

func (p *Provider) executeUpdateRepo(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	reqBody := map[string]any{}

	if desc := getStringInput(inputs, "description"); desc != "" {
		reqBody["description"] = desc
	}
	if homepage := getStringInput(inputs, "homepage"); homepage != "" {
		reqBody["homepage"] = homepage
	}
	if visibility := getStringInput(inputs, "visibility"); visibility != "" {
		reqBody["visibility"] = visibility
	}
	if defaultBranch := getStringInput(inputs, "default_branch"); defaultBranch != "" {
		reqBody["default_branch"] = defaultBranch
	}
	if v, ok := getBoolInput(inputs, "has_issues"); ok {
		reqBody["has_issues"] = v
	}
	if v, ok := getBoolInput(inputs, "has_projects"); ok {
		reqBody["has_projects"] = v
	}
	if v, ok := getBoolInput(inputs, "has_wiki"); ok {
		reqBody["has_wiki"] = v
	}
	if v, ok := getBoolInput(inputs, "allow_squash_merge"); ok {
		reqBody["allow_squash_merge"] = v
	}
	if v, ok := getBoolInput(inputs, "allow_merge_commit"); ok {
		reqBody["allow_merge_commit"] = v
	}
	if v, ok := getBoolInput(inputs, "allow_rebase_merge"); ok {
		reqBody["allow_rebase_merge"] = v
	}
	if v, ok := getBoolInput(inputs, "delete_branch_on_merge"); ok {
		reqBody["delete_branch_on_merge"] = v
	}
	if v, ok := getBoolInput(inputs, "archived"); ok {
		reqBody["archived"] = v
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPatch, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("updating repository: %w", err)
	}
	return actionOutput("update_repo", result), nil
}

// ─── List Topics ─────────────────────────────────────────────────────────────

func (p *Provider) executeListTopics(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (*sdkprovider.Output, error) {
	restURL := fmt.Sprintf("%s/repos/%s/%s/topics", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodGet, restURL, nil)
	if err != nil {
		return nil, fmt.Errorf("listing topics: %w", err)
	}
	return readOutput(result), nil
}

// ─── Replace Topics ──────────────────────────────────────────────────────────

func (p *Provider) executeReplaceTopics(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	topics := getStringSliceInput(inputs, "topics")
	if topics == nil {
		topics = []string{}
	}

	reqBody := map[string]any{
		"names": topics,
	}
	restURL := fmt.Sprintf("%s/repos/%s/%s/topics", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPut, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("replacing topics: %w", err)
	}
	return actionOutput("replace_topics", result), nil
}

// ─── Fork Repository ─────────────────────────────────────────────────────────

func (p *Provider) executeForkRepo(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	reqBody := map[string]any{}
	if org := getStringInput(inputs, "organization"); org != "" {
		reqBody["organization"] = org
	}
	if name := getStringInput(inputs, "name"); name != "" {
		reqBody["name"] = name
	}
	if v, ok := getBoolInput(inputs, "default_branch_only"); ok {
		reqBody["default_branch_only"] = v
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/forks", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPost, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("forking repository: %w", err)
	}
	return actionOutput("fork_repo", result), nil
}

// ─── Create from Template ────────────────────────────────────────────────────

func (p *Provider) executeCreateFromTemplate(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	newOwner := getStringInput(inputs, "new_owner")
	if newOwner == "" {
		newOwner = owner
	}
	newName := getStringInput(inputs, "new_repo_name")
	if newName == "" {
		return nil, requiredInputError("create_from_template", "new_repo_name", inputs, "")
	}

	reqBody := map[string]any{
		"owner": newOwner,
		"name":  newName,
	}
	if desc := getStringInput(inputs, "description"); desc != "" {
		reqBody["description"] = desc
	}
	if v, ok := getBoolInput(inputs, "include_all_branches"); ok {
		reqBody["include_all_branches"] = v
	}
	if visibility := getStringInput(inputs, "visibility"); visibility != "" {
		reqBody["visibility"] = strings.ToLower(visibility)
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/generate", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPost, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("creating from template: %w", err)
	}
	return actionOutput("create_from_template", result), nil
}

// ─── List Custom Properties ──────────────────────────────────────────────────

func (p *Provider) executeListCustomProperties(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (*sdkprovider.Output, error) {
	restURL := fmt.Sprintf("%s/repos/%s/%s/properties/values", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodGet, restURL, nil)
	if err != nil {
		return nil, fmt.Errorf("listing custom properties: %w", err)
	}
	return readOutput(result), nil
}

// ─── Set Custom Properties ───────────────────────────────────────────────────

func (p *Provider) executeSetCustomProperties(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	props := getMapInput(inputs, "properties")
	if len(props) == 0 {
		return nil, requiredInputError("set_custom_properties", "properties", inputs, "map of property_name -> value")
	}

	// Convert flat map to GitHub API format: [{"property_name": "k", "value": "v"}, ...]
	propList := make([]map[string]any, 0, len(props))
	for k, v := range props {
		propList = append(propList, map[string]any{
			"property_name": k,
			"value":         v,
		})
	}
	reqBody := map[string]any{"properties": propList}

	restURL := fmt.Sprintf("%s/repos/%s/%s/properties/values", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPatch, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf("setting custom properties: %w", err)
	}
	return actionOutput("set_custom_properties", result), nil
}

// ─── Create Fork PR (compound) ──────────────────────────────────────────────

// executeCreateForkPR performs a compound fork → sync → branch → commit → PR workflow.
// This replaces the error-prone 4-action chain previously required in solutions.
func (p *Provider) executeCreateForkPR(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	lgr := logr.FromContextOrDiscard(ctx)

	// --- Validate required inputs ---
	forkOrg := getStringInput(inputs, "fork_org")
	if forkOrg == "" {
		return nil, requiredInputError("create_fork_pr", "fork_org", inputs, "organization to fork into")
	}
	branch := getStringInput(inputs, "branch")
	if branch == "" {
		return nil, requiredInputError("create_fork_pr", "branch", inputs, "branch name for the PR")
	}
	message := getStringInput(inputs, "message")
	if message == "" {
		return nil, requiredInputError("create_fork_pr", "message", inputs, "commit message headline")
	}

	additions, deletions, err := parseFileChanges(inputs)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: %w", err)
	}
	if len(additions) == 0 && len(deletions) == 0 {
		return nil, fmt.Errorf("create_fork_pr requires at least one 'additions' or 'deletions' entry")
	}

	// Optional inputs with defaults.
	title := getStringInput(inputs, "title")
	if title == "" {
		title = message
	}
	body := getStringInput(inputs, "body")
	base := getStringInput(inputs, "base")
	draft, _ := getBoolInput(inputs, "draft")
	syncFork := true
	if v, ok := getBoolInput(inputs, "sync_fork"); ok {
		syncFork = v
	}
	force, _ := getBoolInput(inputs, "force")

	// --- Step 1: Fetch upstream default branch (for PR base) ---
	lgr.V(1).Info("create_fork_pr: fetching upstream repo info", "owner", owner, "repo", repo)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	upstreamDefaultBranch, upstreamRepoID, err := p.getRepoInfo(ctx, client, apiBase, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: fetching upstream repo: %w", err)
	}
	if base == "" {
		base = upstreamDefaultBranch
	}

	// --- Step 2: Fork the repository ---
	lgr.V(1).Info("create_fork_pr: forking repository", "owner", owner, "repo", repo, "fork_org", forkOrg)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	forkResult, err := p.forkOrGetExisting(ctx, client, apiBase, owner, repo, forkOrg, base == upstreamDefaultBranch)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: fork failed: %w", err)
	}
	forkNodeID, _ := forkResult["node_id"].(string)
	if forkNodeID == "" {
		return nil, fmt.Errorf("create_fork_pr: fork response missing node_id for %s/%s", forkOrg, repo)
	}

	// --- Step 3: Wait for fork readiness ---
	lgr.V(1).Info("create_fork_pr: waiting for fork readiness", "fork_org", forkOrg, "repo", repo)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	headOID, err := p.waitForForkReady(ctx, client, apiBase, forkOrg, repo, base)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: fork not ready: %w", err)
	}

	// --- Step 4: Sync fork with upstream (if enabled) ---
	if syncFork {
		lgr.V(1).Info("create_fork_pr: syncing fork with upstream", "fork_org", forkOrg, "repo", repo)
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if syncErr := p.syncForkWithUpstream(ctx, client, apiBase, forkOrg, repo, base); syncErr != nil {
			// Non-fatal: log and continue (fork may already be up to date or merge-upstream unsupported)
			lgr.V(1).Info("create_fork_pr: sync fork warning (continuing)", "error", syncErr)
		} else {
			// Re-fetch HEAD OID after sync.
			newOID, err := p.getHeadOID(ctx, client, apiBase, forkOrg, repo, base)
			if err == nil {
				headOID = newOID
			}
		}
	}

	// --- Step 5: Create or force-reset branch on fork ---
	lgr.V(1).Info("create_fork_pr: creating branch on fork", "fork_org", forkOrg, "repo", repo, "branch", branch, "force", force)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	branchOID, err := p.createOrResetBranch(ctx, client, apiBase, forkOrg, repo, branch, headOID, force)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: branch creation failed: %w", err)
	}

	// --- Step 6: Commit files ---
	lgr.V(1).Info("create_fork_pr: committing files", "fork_org", forkOrg, "repo", repo, "branch", branch)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	commitOutput, err := p.commitWithRetry(ctx, client, apiBase, forkOrg, repo, branch, message, branchOID, additions, deletions, inputs)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: commit failed (fork: %s/%s): %w", forkOrg, repo, err)
	}
	var commitResult any
	if dataMap, ok := commitOutput.Data.(map[string]any); ok {
		commitResult = dataMap["result"]
	}

	// --- Step 7: Create cross-repo pull request ---
	lgr.V(1).Info("create_fork_pr: creating pull request", "head", forkOrg+":"+branch, "base", base)
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	prOutput, err := p.createCrossRepoPR(ctx, client, apiBase, upstreamRepoID, forkNodeID, branch, base, title, body, draft)
	if err != nil {
		return nil, fmt.Errorf("create_fork_pr: pull request creation failed: %w", err)
	}

	return actionOutput("create_fork_pr", map[string]any{
		"fork":         forkResult,
		"commit":       commitResult,
		"pull_request": prOutput,
	}), nil
}

// getRepoInfo fetches the default branch and node ID for a repository.
func (p *Provider) getRepoInfo(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (defaultBranch, nodeID string, err error) {
	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    id
    defaultBranchRef { name }
  }
}`
	data, err := graphqlDo(ctx, client, apiBase, query, map[string]any{"owner": owner, "name": repo})
	if err != nil {
		return "", "", err
	}
	repoNode, err := extractNodeMap(data, "repository")
	if err != nil {
		return "", "", err
	}
	nodeID, _ = repoNode["id"].(string)
	if nodeID == "" {
		return "", "", fmt.Errorf("repository %s/%s: missing node ID in GraphQL response", owner, repo)
	}
	if branchRef, ok := repoNode["defaultBranchRef"].(map[string]any); ok {
		defaultBranch, _ = branchRef["name"].(string)
	}
	if defaultBranch == "" {
		return "", "", fmt.Errorf("repository %s/%s: could not determine default branch from API response", owner, repo)
	}
	return defaultBranch, nodeID, nil
}

// forkOrGetExisting forks a repo or returns existing fork details on 422.
func (p *Provider) forkOrGetExisting(ctx context.Context, client *httpc.Client, apiBase, owner, repo, forkOrg string, defaultBranchOnly bool) (map[string]any, error) {
	reqBody := map[string]any{
		"organization":        forkOrg,
		"default_branch_only": defaultBranchOnly,
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/forks", apiBase, owner, repo)
	result, err := p.doRESTRequest(ctx, client, http.MethodPost, restURL, reqBody)
	if err != nil {
		var re *restError
		if errors.As(err, &re) && re.StatusCode == http.StatusUnprocessableEntity {
			// Fork already exists -- fetch it.
			getURL := fmt.Sprintf("%s/repos/%s/%s", apiBase, forkOrg, repo)
			existing, getErr := p.doRESTRequest(ctx, client, http.MethodGet, getURL, nil)
			if getErr != nil {
				return nil, fmt.Errorf("fork already exists but failed to fetch: %w", getErr)
			}
			m, ok := existing.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("fork already exists but response for %s/%s is not a JSON object", forkOrg, repo)
			}
			// Validate that the existing repo is actually a fork of the upstream.
			if parent, hasParent := m["parent"].(map[string]any); hasParent {
				parentFullName, _ := parent["full_name"].(string)
				expected := owner + "/" + repo
				if parentFullName != expected {
					return nil, fmt.Errorf("repo %s/%s exists but is not a fork of %s (parent: %s)", forkOrg, repo, expected, parentFullName)
				}
			} else if isFork, _ := m["fork"].(bool); !isFork {
				return nil, fmt.Errorf("repo %s/%s exists but is not a fork", forkOrg, repo)
			}
			return m, nil
		}
		return nil, err
	}
	m, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("fork response for %s/%s is not a JSON object", owner, repo)
	}
	return m, nil
}

// waitForForkReady polls get_head_oid on the fork until the branch is accessible.
func (p *Provider) waitForForkReady(ctx context.Context, client *httpc.Client, apiBase, forkOrg, repo, branch string) (string, error) {
	lgr := logr.FromContextOrDiscard(ctx)

	var lastErr error
	for attempt := range p.forkReadyMaxAttempts {
		if attempt > 0 {
			delay := p.forkReadyBackoff
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return "", ctx.Err()
			case <-timer.C:
			}
		}

		oid, err := p.getHeadOID(ctx, client, apiBase, forkOrg, repo, branch)
		if err == nil {
			lgr.V(1).Info("fork ready", "attempt", attempt+1, "oid", oid)
			return oid, nil
		}
		lastErr = err
		lgr.V(1).Info("fork not ready yet", "attempt", attempt+1, "error", err)
	}
	return "", fmt.Errorf("fork not ready after %d attempts: %w", p.forkReadyMaxAttempts, lastErr)
}

// branchNotFoundError is returned when a branch (ref) does not exist on the
// repository. It is a typed error so callers can distinguish a missing branch
// (e.g. a first-time state save) from other failures via errors.As.
type branchNotFoundError struct {
	branch string
	owner  string
	repo   string
}

func (e *branchNotFoundError) Error() string {
	return fmt.Sprintf("branch %q not found on %s/%s", e.branch, e.owner, e.repo)
}

// isBranchNotFound reports whether err indicates a missing branch.
func isBranchNotFound(err error) bool {
	var bnf *branchNotFoundError
	return errors.As(err, &bnf)
}

// getHeadOID fetches the HEAD OID for a branch on a repository.
func (p *Provider) getHeadOID(ctx context.Context, client *httpc.Client, apiBase, owner, repo, branch string) (string, error) {
	query := `query($owner: String!, $name: String!, $qualifiedName: String!) {
  repository(owner: $owner, name: $name) {
    ref(qualifiedName: $qualifiedName) {
      target { oid }
    }
  }
}`
	vars := map[string]any{"owner": owner, "name": repo, "qualifiedName": "refs/heads/" + branch}
	data, err := graphqlDo(ctx, client, apiBase, query, vars)
	if err != nil {
		return "", err
	}
	ref, err := extractNode(data, "repository.ref")
	if err != nil {
		return "", err
	}
	if ref == nil {
		return "", &branchNotFoundError{branch: branch, owner: owner, repo: repo}
	}
	refMap, ok := ref.(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected ref format")
	}
	target, ok := refMap["target"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected ref target format")
	}
	oid, _ := target["oid"].(string)
	if oid == "" {
		return "", fmt.Errorf("empty OID for branch %q", branch)
	}
	return oid, nil
}

// syncForkWithUpstream syncs the fork's branch with the upstream using the merge-upstream API.
func (p *Provider) syncForkWithUpstream(ctx context.Context, client *httpc.Client, apiBase, forkOrg, repo, branch string) error {
	restURL := fmt.Sprintf("%s/repos/%s/%s/merge-upstream", apiBase, forkOrg, repo)
	reqBody := map[string]any{"branch": branch}
	_, err := p.doRESTRequest(ctx, client, http.MethodPost, restURL, reqBody)
	return err
}

// createOrResetBranch creates a branch on the fork, handling force-reset and already-exists scenarios.
func (p *Provider) createOrResetBranch(ctx context.Context, client *httpc.Client, apiBase, forkOrg, repo, branch, oid string, force bool) (string, error) {
	_, err := p.executeCreateRef(ctx, client, apiBase, forkOrg, repo, "create_branch", "refs/heads/"+branch, oid)
	if err == nil {
		return oid, nil
	}

	// Check if branch already exists (GraphQL returns UNPROCESSABLE or error containing "already exists").
	if !isBranchAlreadyExists(err) {
		return "", err
	}

	if force {
		// Delete and recreate.
		refID, delErr := p.resolveRefID(ctx, client, apiBase, forkOrg, repo, "refs/heads/"+branch)
		if delErr != nil {
			return "", fmt.Errorf("resolving branch for deletion: %w", delErr)
		}
		delMutation := `mutation($input: DeleteRefInput!) { deleteRef(input: $input) { clientMutationId } }`
		if _, delErr = graphqlDo(ctx, client, apiBase, delMutation, map[string]any{"input": map[string]any{"refId": refID}}); delErr != nil {
			return "", fmt.Errorf("deleting branch for force-reset: %w", delErr)
		}
		// Recreate.
		if _, err = p.executeCreateRef(ctx, client, apiBase, forkOrg, repo, "create_branch", "refs/heads/"+branch, oid); err != nil {
			return "", fmt.Errorf("recreating branch after force-delete: %w", err)
		}
		return oid, nil
	}

	// Not forcing -- use existing branch HEAD OID.
	existingOID, err := p.getHeadOID(ctx, client, apiBase, forkOrg, repo, branch)
	if err != nil {
		return "", fmt.Errorf("fetching existing branch OID: %w", err)
	}
	return existingOID, nil
}

// isBranchAlreadyExists checks if an error indicates the ref already exists.
func isBranchAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "reference already exists")
}

// commitWithRetry commits to the fork with FORBIDDEN retry logic for permission propagation.
func (p *Provider) commitWithRetry(ctx context.Context, client *httpc.Client, apiBase, forkOrg, repo, branch, message, expectedOID string, additions []fileAddition, deletions []fileDeletion, inputs map[string]any) (*sdkprovider.Output, error) {
	lgr := logr.FromContextOrDiscard(ctx)
	var lastErr error

	for attempt := range p.commitMaxAttempts {
		if attempt > 0 {
			delay := time.Duration(attempt) * p.commitRetryBackoff
			lgr.V(1).Info("retrying commit after FORBIDDEN", "attempt", attempt+1, "delay", delay)
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}

		output, err := p.executeCreateCommitGraphQL(ctx, client, apiBase, forkOrg, repo, branch, message, expectedOID, additions, deletions, inputs)
		if err == nil {
			return output, nil
		}
		lastErr = err

		if !isGraphQLForbidden(err) {
			return nil, err
		}
	}
	return nil, lastErr
}

// createCrossRepoPR creates a pull request from a fork branch to the upstream repo.
func (p *Provider) createCrossRepoPR(ctx context.Context, client *httpc.Client, apiBase, upstreamRepoID, forkRepoNodeID, headBranch, baseBranch, title, body string, draft bool) (map[string]any, error) {
	mutInput := map[string]any{
		"repositoryId": upstreamRepoID,
		"title":        title,
		"headRefName":  headBranch,
		"baseRefName":  baseBranch,
	}
	if forkRepoNodeID != "" {
		mutInput["headRepositoryId"] = forkRepoNodeID
	}
	if body != "" {
		mutInput["body"] = body
	}
	if draft {
		mutInput["draft"] = true
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

	pr, err := extractNodeMap(data, "createPullRequest.pullRequest")
	if err != nil {
		return nil, err
	}
	return pr, nil
}
