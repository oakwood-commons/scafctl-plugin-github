// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// executeStateSave persists state data as a JSON file by creating a signed
// commit on the target branch. Uses optimistic locking via the branch HEAD OID.
func (p *Provider) executeStateSave(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	path := getStringInput(inputs, "path")
	if path == "" {
		return nil, requiredInputError("state_save", "path", inputs, "")
	}
	branch := getStringInput(inputs, "branch")
	if branch == "" {
		return nil, requiredInputError("state_save", "branch", inputs, "")
	}

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

	// Fetch current HEAD OID for optimistic locking.
	headOID, err := p.getHeadOID(ctx, client, apiBase, owner, repo, branch)
	if err != nil {
		return nil, fmt.Errorf("state_save: get branch HEAD: %w", err)
	}

	// Create commit with the state file (with FORBIDDEN retry for permission propagation).
	commitOutput, err := p.commitWithRetry(
		ctx, client, apiBase, owner, repo, branch, message, headOID,
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

	// Extract commit OID from the commit output for the response.
	var commitOID string
	if commitOutput != nil && commitOutput.Data != nil {
		if d, ok := commitOutput.Data.(map[string]any); ok {
			if r, ok := d["result"].(map[string]any); ok {
				commitOID, _ = r["oid"].(string)
			}
		}
	}

	result := map[string]any{"success": true}
	if commitOID != "" {
		result["commit_oid"] = commitOID
	}
	return &sdkprovider.Output{Data: result}, nil
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
