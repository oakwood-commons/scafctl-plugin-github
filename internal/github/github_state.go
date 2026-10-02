// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

// ─── State Load ──────────────────────────────────────────────────────────────

// executeStateLoad reads a JSON state file from a repository. If the file does
// not exist, it returns an empty state (first-run scenario).
func (p *Provider) executeStateLoad(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	path := getStringInput(inputs, "path")
	if path == "" {
		return nil, requiredInputError("state_load", "path", inputs, "")
	}
	ref := getStringInput(inputs, "ref")
	if ref == "" {
		return nil, requiredInputError("state_load", "ref", inputs, "the git ref to read state from (e.g. main)")
	}

	content, err := p.getFileContentRaw(ctx, client, apiBase, owner, repo, path, ref)
	if err != nil {
		if isFileNotFound(err) {
			// First run -- no state file yet.
			return stateOutput(true, map[string]any{}), nil
		}
		return nil, fmt.Errorf("state_load: read file: %w", err)
	}

	var data any
	if err := json.Unmarshal([]byte(content), &data); err != nil {
		return nil, fmt.Errorf("state_load: parse state JSON at %s: %w", path, err)
	}

	return stateOutput(true, data), nil
}

// ─── State Save ──────────────────────────────────────────────────────────────

// executeStateSave persists state data as a JSON file by committing it to the
// target branch. The normal path creates a GitHub-signed commit via the GraphQL
// createCommitOnBranch mutation and uses optimistic locking via the branch HEAD
// OID.
//
// On the first save the target branch may not exist yet. In that case the
// branch is bootstrapped from a base ref (the optional "base_ref" input, or the
// repository's default branch) before committing. If the repository has no
// commits at all (a brand-new empty repo) the initial commit is seeded through
// the REST Contents API, which is the only way to create the first commit.
//
// When the optional "pull_request" input is set, a pull request for the state
// branch is opened after the commit (or reused if one is already open). The
// block is validated up front, before anything is committed.
func (p *Provider) executeStateSave(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	path := getStringInput(inputs, "path")
	if path == "" {
		return nil, requiredInputError("state_save", "path", inputs, "")
	}
	branch := getStringInput(inputs, "branch")
	if branch == "" {
		return nil, requiredInputError("state_save", "branch", inputs, "")
	}
	baseRef := getStringInput(inputs, "base_ref")

	data := inputs["data"]
	if data == nil {
		return nil, requiredInputError("state_save", "data", inputs, "the state data to persist")
	}

	message := getStringInput(inputs, "message")
	if message == "" {
		message = "chore(state): update state"
	}

	stateJSON, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("state_save: marshal state: %w", err)
	}

	// Validate the pull_request block before any commit so bad input has no
	// side effects.
	pr, err := p.resolveStatePullRequest(ctx, client, apiBase, owner, repo, branch, baseRef, inputs)
	if err != nil {
		return nil, err
	}

	// Fetch current HEAD OID for optimistic locking.
	headOID, err := p.getHeadOID(ctx, client, apiBase, owner, repo, branch)
	if err != nil {
		if isBranchNotFound(err) {
			// First save -- the target branch does not exist yet. Bootstrap it
			// rather than failing with "branch not found".
			return p.bootstrapStateSave(ctx, client, apiBase, owner, repo, path, branch, baseRef, message, stateJSON, pr)
		}
		return nil, fmt.Errorf("state_save: get branch HEAD: %w", err)
	}

	return p.commitStateFile(ctx, client, apiBase, owner, repo, path, branch, message, headOID, stateJSON, pr)
}

// statePullRequest is the validated "pull_request" input of state_save.
type statePullRequest struct {
	title string
	body  string
	base  string
	draft *bool // nil when not set, so GitHub applies its default (not a draft)
}

// resolveStatePullRequest parses and validates the optional "pull_request"
// input of state_save. It returns nil when the block is absent. The base branch
// defaults to base_ref, then to the repository's default branch (one API call,
// made only when neither is set).
func (p *Provider) resolveStatePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo, branch, baseRef string, inputs map[string]any) (*statePullRequest, error) {
	raw, ok := inputs["pull_request"]
	if !ok || raw == nil {
		return nil, nil
	}
	prInput, ok := raw.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("state_save: 'pull_request' must be an object, got %T", raw)
	}

	title := getStringInput(prInput, "title")
	if strings.TrimSpace(title) == "" {
		return nil, requiredInputError("state_save", "pull_request.title", inputs, "")
	}

	base := getStringInput(prInput, "base")
	if base == "" {
		// base_ref may be a tag or commit SHA because it only seeds the branch,
		// but a pull request needs a branch. A SHA is recognizable up front; a
		// tag is not without an API call, so that case is left to the README.
		if isFullCommitSHA(baseRef) {
			return nil, fmt.Errorf("state_save: base_ref %q is a commit SHA, which cannot be the pull request base -- set pull_request.base to a branch", baseRef)
		}
		base = baseRef
	}
	if base == "" {
		name, err := p.getRepoDefaultBranchName(ctx, client, apiBase, owner, repo)
		if err != nil {
			return nil, fmt.Errorf("state_save: resolve default branch for pull request base: %w", err)
		}
		base = name
	}
	if base == branch {
		return nil, fmt.Errorf("state_save: pull_request.base must differ from branch (both are %q)", branch)
	}

	pr := &statePullRequest{title: title, body: getStringInput(prInput, "body"), base: base}
	if draft, ok := getBoolInput(prInput, "draft"); ok {
		pr.draft = &draft
	}
	return pr, nil
}

// ensureStatePullRequest returns the open pull request from branch to pr.base,
// opening one when none exists. The result is the state_save "pull_request"
// output: {number, url, created}.
func (p *Provider) ensureStatePullRequest(ctx context.Context, client *httpc.Client, apiBase, owner, repo, branch string, pr *statePullRequest) (map[string]any, error) {
	existing, err := p.findOpenPullRequest(ctx, client, apiBase, owner, repo, branch, pr.base)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return statePullRequestOutput(existing, false), nil
	}

	created, err := p.createPullRequest(ctx, client, apiBase, owner, repo, pr.title, branch, pr.base, pr.body, pr.draft)
	if err != nil {
		return nil, err
	}
	return statePullRequestOutput(created, true), nil
}

// statePullRequestOutput builds the state_save "pull_request" output from a
// GraphQL pull request node. The node's number arrives as a float64 (JSON); it
// is normalized to an int.
func statePullRequestOutput(node map[string]any, created bool) map[string]any {
	number, _ := getIntInput(node, "number")
	prURL, _ := node["url"].(string)
	return map[string]any{"number": number, "url": prURL, "created": created}
}

// commitStateFile creates a signed commit that writes the state file to an
// existing branch, using expectedOID for optimistic locking. When pr is set it
// then opens (or reuses) the pull request for the branch.
func (p *Provider) commitStateFile(ctx context.Context, client *httpc.Client, apiBase, owner, repo, path, branch, message, expectedOID string, stateJSON []byte, pr *statePullRequest) (*sdkprovider.Output, error) {
	// Create commit with the state file (with FORBIDDEN retry for permission propagation).
	commitOutput, err := p.commitWithRetry(
		ctx, client, apiBase, owner, repo, branch, message, expectedOID,
		[]fileAddition{{Path: path, Content: string(stateJSON)}},
		nil, // no deletions
		nil, // no extra inputs
	)
	if err != nil {
		if isOIDMismatch(err) {
			return nil, fmt.Errorf("state save conflict: concurrent commit on branch %q -- re-run to pick up latest state", branch)
		}
		return nil, fmt.Errorf("state_save: create commit: %w", err)
	}

	commitOID := commitOIDFromCommitOutput(commitOutput)
	if pr == nil {
		return stateSaveOutput(commitOID, nil), nil
	}

	prOutput, err := p.ensureStatePullRequest(ctx, client, apiBase, owner, repo, branch, pr)
	if err != nil {
		// The commit is already on the branch; say so, so the caller knows a
		// re-run only needs to retry the pull request.
		return nil, fmt.Errorf("state_save: committed %s to branch %q but failed to open pull request: %w", commitOID, branch, err)
	}
	return stateSaveOutput(commitOID, prOutput), nil
}

// bootstrapStateSave handles the first save against a branch that does not yet
// exist. It creates the branch from a base ref (or the repository default
// branch) and then commits the state file. When the repository has no commits
// at all, it seeds the initial commit through the REST Contents API, which
// cannot be combined with a pull request (there is no base branch to target).
func (p *Provider) bootstrapStateSave(ctx context.Context, client *httpc.Client, apiBase, owner, repo, path, branch, baseRef, message string, stateJSON []byte, pr *statePullRequest) (*sdkprovider.Output, error) {
	// Resolve the OID the new branch should point at.
	var baseOID string
	if baseRef != "" {
		oid, err := p.resolveBaseRefOID(ctx, client, apiBase, owner, repo, baseRef)
		if err != nil {
			if isBranchNotFound(err) {
				return nil, fmt.Errorf("state_save: base_ref %q not found on %s/%s -- specify an existing branch, tag, or commit SHA to seed state from", baseRef, owner, repo)
			}
			return nil, fmt.Errorf("state_save: resolve base_ref %q: %w", baseRef, err)
		}
		baseOID = oid
	} else {
		oid, err := p.getDefaultBranchHeadOID(ctx, client, apiBase, owner, repo)
		if err != nil {
			return nil, fmt.Errorf("state_save: resolve default branch: %w", err)
		}
		if oid == "" {
			// The repository has no commits yet -- there is no base to branch
			// from. Seed the first commit through the REST Contents API.
			if pr != nil {
				return nil, fmt.Errorf("state_save: pull_request is not supported when initializing an empty repository (%s/%s has no commits yet) -- add an initial commit first, or remove pull_request", owner, repo)
			}
			return p.seedStateOnEmptyRepo(ctx, client, apiBase, owner, repo, path, branch, message, stateJSON)
		}
		baseOID = oid
	}

	// Create the state branch from the base OID. createOrResetBranch tolerates a
	// concurrent create (branch already exists) by returning the existing HEAD.
	branchOID, err := p.createOrResetBranch(ctx, client, apiBase, owner, repo, branch, baseOID, false)
	if err != nil {
		return nil, fmt.Errorf("state_save: create state branch %q: %w", branch, err)
	}

	return p.commitStateFile(ctx, client, apiBase, owner, repo, path, branch, message, branchOID, stateJSON, pr)
}

// seedStateOnEmptyRepo creates the very first commit in an empty repository (one
// with no commits) via the REST Contents API. GraphQL createCommitOnBranch
// cannot bootstrap an empty repository because it requires an existing branch
// HEAD. When the requested state branch matches the repository's configured
// default branch, the branch parameter is omitted so GitHub creates the default
// branch and its first commit together.
func (p *Provider) seedStateOnEmptyRepo(ctx context.Context, client *httpc.Client, apiBase, owner, repo, path, branch, message string, stateJSON []byte) (*sdkprovider.Output, error) {
	defaultBranch, err := p.getRepoDefaultBranchName(ctx, client, apiBase, owner, repo)
	if err != nil {
		return nil, fmt.Errorf("state_save: determine default branch for empty repo %s/%s: %w", owner, repo, err)
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/contents/%s", apiBase, owner, repo, escapeContentsPath(path))
	reqBody := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(stateJSON),
	}
	// Only pin the branch when it differs from the default; on an empty repo the
	// first commit must land on the default branch, so omitting it is safest.
	if branch != "" && branch != defaultBranch {
		reqBody["branch"] = branch
	}

	resp, err := p.doRESTRequest(ctx, client, http.MethodPut, restURL, reqBody)
	if err != nil {
		return nil, fmt.Errorf(
			"state_save: initialize state on empty repository %s/%s (branch %q): %w -- "+
				"the repository has no commits yet; add an initial commit (e.g. a README) "+
				"or set the state branch to the default branch %q",
			owner, repo, branch, err, defaultBranch)
	}

	return stateSaveOutput(commitSHAFromContentsResponse(resp), nil), nil
}

// ─── State Delete ────────────────────────────────────────────────────────────

// executeStateDelete removes a state file by creating a signed commit with a
// file deletion. The operation is idempotent -- deleting an already-absent file
// succeeds silently.
func (p *Provider) executeStateDelete(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	path := getStringInput(inputs, "path")
	if path == "" {
		return nil, requiredInputError("state_delete", "path", inputs, "")
	}
	branch := getStringInput(inputs, "branch")
	if branch == "" {
		return nil, requiredInputError("state_delete", "branch", inputs, "")
	}

	message := getStringInput(inputs, "message")
	if message == "" {
		message = "chore(state): delete state"
	}

	// Check if file exists first (idempotent delete).
	_, err := p.getFileContentRaw(ctx, client, apiBase, owner, repo, path, branch)
	if err != nil {
		if isFileNotFound(err) {
			// Already gone -- success.
			return &sdkprovider.Output{Data: map[string]any{"success": true}}, nil
		}
		return nil, fmt.Errorf("state_delete: check file: %w", err)
	}

	headOID, err := p.getHeadOID(ctx, client, apiBase, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("state_delete: get branch HEAD: %w", err)
	}

	_, err = p.commitWithRetry(
		ctx, client, apiBase, owner, repo, branch, message, headOID,
		nil, // no additions
		[]fileDeletion{{Path: path}},
		nil, // no extra inputs
	)
	if err != nil {
		return nil, fmt.Errorf("state_delete: create commit: %w", err)
	}

	return &sdkprovider.Output{Data: map[string]any{"success": true}}, nil
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// getFileContentRaw reads a file's text content from a repository at a given
// ref. Returns *fileNotFoundError if the file does not exist.
func (p *Provider) getFileContentRaw(ctx context.Context, client *httpc.Client, apiBase, owner, repo, path, ref string) (string, error) {
	expression := ref + ":" + path

	query := `query($owner: String!, $name: String!, $expression: String!) {
  repository(owner: $owner, name: $name) {
    object(expression: $expression) {
      ... on Blob {
        text
      }
    }
  }
}`
	vars := map[string]any{
		"owner":      owner,
		"name":       repo,
		"expression": expression,
	}
	data, err := graphqlDo(ctx, client, apiBase, query, vars)
	if err != nil {
		// GraphQL NOT_FOUND means the repo itself doesn't exist -- propagate
		// as a distinct error so callers don't confuse it with a missing file.
		return "", err
	}

	obj, err := extractNode(data, "repository.object")
	if err != nil {
		return "", err
	}
	if obj == nil {
		return "", &fileNotFoundError{path: path, ref: ref}
	}

	blob, ok := obj.(map[string]any)
	if !ok {
		return "", fmt.Errorf("object at %s is not a file (blob)", path)
	}

	text, hasText := blob["text"].(string)
	if !hasText {
		return "", fmt.Errorf("file at %s is not a readable text blob", path)
	}
	return text, nil
}

// fileNotFoundError is returned when a file does not exist at the given ref.
type fileNotFoundError struct {
	path string
	ref  string
}

func (e *fileNotFoundError) Error() string {
	return fmt.Sprintf("file not found: %s (ref: %s)", e.path, e.ref)
}

// isFileNotFound checks whether an error indicates a missing file.
func isFileNotFound(err error) bool {
	var fnf *fileNotFoundError
	return errors.As(err, &fnf)
}

// getDefaultBranchHeadOID returns the HEAD OID of the repository's default
// branch. An empty string (with a nil error) indicates the repository has no
// commits yet (a brand-new empty repository).
func (p *Provider) getDefaultBranchHeadOID(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (string, error) {
	query := `query($owner: String!, $name: String!) {
  repository(owner: $owner, name: $name) {
    defaultBranchRef {
      target { oid }
    }
  }
}`
	vars := map[string]any{"owner": owner, "name": repo}
	data, err := graphqlDo(ctx, client, apiBase, query, vars)
	if err != nil {
		return "", err
	}

	ref, err := extractNode(data, "repository.defaultBranchRef")
	if err != nil {
		return "", err
	}
	if ref == nil {
		// Empty repository -- no default branch, no commits.
		return "", nil
	}
	refMap, ok := ref.(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected defaultBranchRef format")
	}
	target, ok := refMap["target"].(map[string]any)
	if !ok {
		return "", fmt.Errorf("unexpected defaultBranchRef target format")
	}
	oid, _ := target["oid"].(string)
	return oid, nil
}

// getRepoDefaultBranchName returns the repository's configured default branch
// name via the REST API. Unlike the GraphQL defaultBranchRef, this is populated
// even for empty repositories that have no commits yet. Falls back to "main"
// when the field is absent.
func (p *Provider) getRepoDefaultBranchName(ctx context.Context, client *httpc.Client, apiBase, owner, repo string) (string, error) {
	url := fmt.Sprintf("%s/repos/%s/%s", apiBase, owner, repo)
	resp, err := p.doRESTRequest(ctx, client, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	if info, ok := resp.(map[string]any); ok {
		if name, ok := info["default_branch"].(string); ok && name != "" {
			return name, nil
		}
	}
	return "main", nil
}

// isOIDMismatch checks whether a GraphQL error indicates an OID mismatch
// (concurrent commit conflict on the branch).
func isOIDMismatch(err error) bool {
	var gqlErr *GraphQLError
	if errors.As(err, &gqlErr) {
		for _, e := range gqlErr.Errors {
			if strings.Contains(e.Message, "expectedHeadOid") ||
				strings.Contains(e.Message, "head OID") {
				return true
			}
		}
	}
	return false
}

// stateOutput builds the standard state operation output.
func stateOutput(success bool, data any) *sdkprovider.Output {
	out := map[string]any{"success": success}
	if data != nil {
		out["data"] = data
	}
	return &sdkprovider.Output{Data: out}
}

// stateSaveOutput builds the output for a successful state_save, including the
// commit OID when one is known and the pull request when one was opened or
// reused.
func stateSaveOutput(commitOID string, pr map[string]any) *sdkprovider.Output {
	result := map[string]any{"success": true}
	if commitOID != "" {
		result["commit_oid"] = commitOID
	}
	if pr != nil {
		result["pull_request"] = pr
	}
	return &sdkprovider.Output{Data: result}
}

// commitOIDFromCommitOutput extracts the commit OID from a commitWithRetry
// action output ({"result": {"oid": ...}}).
func commitOIDFromCommitOutput(commitOutput *sdkprovider.Output) string {
	if commitOutput == nil || commitOutput.Data == nil {
		return ""
	}
	d, ok := commitOutput.Data.(map[string]any)
	if !ok {
		return ""
	}
	r, ok := d["result"].(map[string]any)
	if !ok {
		return ""
	}
	oid, _ := r["oid"].(string)
	return oid
}

// commitSHAFromContentsResponse extracts the commit SHA from a REST Contents API
// response ({"commit": {"sha": ...}}).
func commitSHAFromContentsResponse(resp any) string {
	m, ok := resp.(map[string]any)
	if !ok {
		return ""
	}
	commit, ok := m["commit"].(map[string]any)
	if !ok {
		return ""
	}
	sha, _ := commit["sha"].(string)
	return sha
}

// escapeContentsPath percent-encodes each segment of a repository file path
// while preserving the "/" separators, so a path with special characters is
// safe to interpolate into a REST Contents API URL.
func escapeContentsPath(path string) string {
	segments := strings.Split(path, "/")
	for i, seg := range segments {
		segments[i] = url.PathEscape(seg)
	}
	return strings.Join(segments, "/")
}
