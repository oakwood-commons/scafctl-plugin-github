// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"unicode/utf8"

	"github.com/oakwood-commons/httpc"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

const (
	defaultJobLogsTailLines = 200
	maxJobLogsTailLines     = 10000
	defaultJobLogsMaxBytes  = 262144           // 256 KiB
	maxJobLogsMaxBytes      = 10485760         // 10 MiB
	maxJobLogsStreamBytes   = 64 * 1024 * 1024 // 64 MiB
)

// ─── List Check Runs ─────────────────────────────────────────────────────────

// executeListCheckRuns lists check runs for a commit ref via the REST API.
// This replaces `gh pr checks <number>`.
func (p *Provider) executeListCheckRuns(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	ref := getStringInput(inputs, "ref")
	if ref == "" {
		return nil, requiredInputError("list_check_runs", "ref", inputs, "")
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/check-runs", apiBase, owner, repo, url.PathEscape(ref))
	result, err := p.doRESTRequest(ctx, client, "GET", restURL, nil)
	if err != nil {
		return nil, fmt.Errorf("listing check runs: %w", err)
	}

	resultMap, ok := result.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected check runs response format")
	}

	checkRuns, _ := resultMap["check_runs"].([]any)
	// Slim down each check run to the fields the agents need
	runs := make([]any, 0, len(checkRuns))
	for _, cr := range checkRuns {
		crMap, ok := cr.(map[string]any)
		if !ok {
			continue
		}
		run := map[string]any{
			"id":           crMap["id"],
			"name":         crMap["name"],
			"status":       crMap["status"],
			"conclusion":   crMap["conclusion"],
			"started_at":   crMap["started_at"],
			"completed_at": crMap["completed_at"],
			"html_url":     crMap["html_url"],
		}
		if output, ok := crMap["output"].(map[string]any); ok {
			run["output"] = map[string]any{
				"title":   output["title"],
				"summary": output["summary"],
			}
		}
		runs = append(runs, run)
	}

	return readOutput(map[string]any{
		"total_count": resultMap["total_count"],
		"check_runs":  runs,
	}), nil
}

// ─── Get Workflow Run ────────────────────────────────────────────────────────

// executeGetWorkflowRun fetches a workflow run and its jobs via the REST API.
// This replaces `gh run view <run_id> --log-failed`.
func (p *Provider) executeGetWorkflowRun(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	runID, ok := getIntInput(inputs, "run_id")
	if !ok || runID == 0 {
		return nil, requiredInputError("get_workflow_run", "run_id", inputs, "")
	}

	// Fetch the workflow run
	runURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d", apiBase, owner, repo, runID)
	runResult, err := p.doRESTRequest(ctx, client, "GET", runURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching workflow run: %w", err)
	}

	runMap, ok := runResult.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("unexpected workflow run response format")
	}

	// Fetch jobs for the run
	jobsURL := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%d/jobs", apiBase, owner, repo, runID)
	jobsResult, err := p.doRESTRequest(ctx, client, "GET", jobsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("fetching workflow run jobs: %w", err)
	}

	var jobs []any
	if jobsMap, ok := jobsResult.(map[string]any); ok {
		if jobsList, ok := jobsMap["jobs"].([]any); ok {
			jobs = make([]any, 0, len(jobsList))
			for _, j := range jobsList {
				jMap, ok := j.(map[string]any)
				if !ok {
					continue
				}
				job := map[string]any{
					"id":           jMap["id"],
					"name":         jMap["name"],
					"status":       jMap["status"],
					"conclusion":   jMap["conclusion"],
					"started_at":   jMap["started_at"],
					"completed_at": jMap["completed_at"],
					"html_url":     jMap["html_url"],
				}
				// Include steps for failed jobs
				if jMap["conclusion"] == "failure" {
					if steps, ok := jMap["steps"].([]any); ok {
						job["steps"] = steps
					}
				}
				jobs = append(jobs, job)
			}
		}
	}

	return readOutput(map[string]any{
		"id":            runMap["id"],
		"name":          runMap["name"],
		"status":        runMap["status"],
		"conclusion":    runMap["conclusion"],
		"html_url":      runMap["html_url"],
		"run_number":    runMap["run_number"],
		"event":         runMap["event"],
		"created_at":    runMap["created_at"],
		"updated_at":    runMap["updated_at"],
		"head_branch":   runMap["head_branch"],
		"head_sha":      runMap["head_sha"],
		"display_title": runMap["display_title"],
		"jobs":          jobs,
	}), nil
}

// ─── List Commit Pulls ───────────────────────────────────────────────────────

// executeListCommitPulls lists pull requests associated with a commit via the REST API.
// Uses: GET /repos/{owner}/{repo}/commits/{commit_sha}/pulls
func (p *Provider) executeListCommitPulls(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	commitSHA := getStringInputWithAliases(inputs, "commit_sha", "sha")
	if commitSHA == "" {
		return nil, requiredInputError("list_commit_pulls", "commit_sha", inputs, "")
	}

	restURL := fmt.Sprintf("%s/repos/%s/%s/commits/%s/pulls", apiBase, owner, repo, url.PathEscape(commitSHA))
	result, err := p.doRESTRequest(ctx, client, "GET", restURL, nil)
	if err != nil {
		return nil, fmt.Errorf("listing commit pulls: %w", err)
	}

	pulls, ok := result.([]any)
	if !ok {
		return readOutput(map[string]any{
			"total_count":   0,
			"pull_requests": []any{},
		}), nil
	}

	// Slim down each PR to essential fields.
	prs := make([]any, 0, len(pulls))
	for _, pr := range pulls {
		prMap, ok := pr.(map[string]any)
		if !ok {
			continue
		}
		slim := map[string]any{
			"number":     prMap["number"],
			"title":      prMap["title"],
			"state":      prMap["state"],
			"html_url":   prMap["html_url"],
			"created_at": prMap["created_at"],
			"merged_at":  prMap["merged_at"],
			"draft":      prMap["draft"],
		}
		if user, ok := prMap["user"].(map[string]any); ok {
			slim["user"] = user["login"]
		}
		if head, ok := prMap["head"].(map[string]any); ok {
			slim["head_ref"] = head["ref"]
		}
		if base, ok := prMap["base"].(map[string]any); ok {
			slim["base_ref"] = base["ref"]
		}
		prs = append(prs, slim)
	}

	return readOutput(map[string]any{
		"total_count":   len(prs),
		"pull_requests": prs,
	}), nil
}

// ─── Get Job Logs ────────────────────────────────────────────────────────────

// executeGetJobLogs downloads plain-text logs for a GitHub Actions job via the REST API
// and retains only the bounded tail in memory.
func (p *Provider) executeGetJobLogs(ctx context.Context, client *httpc.Client, apiBase, owner, repo string, inputs map[string]any) (*sdkprovider.Output, error) {
	if client == nil {
		return nil, fmt.Errorf("HTTP client is not initialized")
	}
	jobID, err := requirePositiveIntInput(inputs, "get_job_logs", "job_id")
	if err != nil {
		return nil, err
	}

	tailLines := defaultJobLogsTailLines
	if rawTail, exists := inputs["tail_lines"]; exists && rawTail != nil {
		tl, valid := getIntInput(inputs, "tail_lines")
		if !valid {
			return nil, fmt.Errorf("get_job_logs: 'tail_lines' must be an integer")
		}
		if tl < 0 || tl > maxJobLogsTailLines {
			return nil, fmt.Errorf("get_job_logs: 'tail_lines' must be between 0 and %d, got %d", maxJobLogsTailLines, tl)
		}
		tailLines = tl
	}

	maxBytes := defaultJobLogsMaxBytes
	if rawMax, exists := inputs["max_bytes"]; exists && rawMax != nil {
		mb, valid := getIntInput(inputs, "max_bytes")
		if !valid {
			return nil, fmt.Errorf("get_job_logs: 'max_bytes' must be an integer")
		}
		if mb < 1 || mb > maxJobLogsMaxBytes {
			return nil, fmt.Errorf("get_job_logs: 'max_bytes' must be between 1 and %d, got %d", maxJobLogsMaxBytes, mb)
		}
		maxBytes = mb
	}

	logsURL := fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%d/logs", apiBase, url.PathEscape(owner), url.PathEscape(repo), jobID)
	requestCtx, _ := withRequestAuthState(ctx)
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, logsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("creating job logs request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	resp, err := client.Do(req)
	if resp != nil && resp.Body != nil {
		defer resp.Body.Close() //nolint:errcheck // best-effort close
	}
	if err != nil {
		return nil, fmt.Errorf("fetching job logs: %w", redactURLErrorQuery(err))
	}

	if resp.StatusCode >= 400 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		msg := string(errBody)
		var ghErr map[string]any
		if json.Unmarshal(errBody, &ghErr) == nil {
			if m, isStr := ghErr["message"].(string); isStr {
				msg = m
			}
		}
		var apiErr error = &restError{StatusCode: resp.StatusCode, Message: msg}
		if shouldAddUnauthenticatedHint(requestCtx, req, resp) {
			apiErr = wrapUnauthenticatedHTTPError(resp.StatusCode, apiErr)
		}
		return nil, fmt.Errorf("fetching job logs: %w", apiErr)
	}

	logsText, truncated, totalBytes, returnedLines, err := readLogTail(resp.Body, maxBytes, tailLines)
	if err != nil {
		return nil, fmt.Errorf("reading job logs: %w", err)
	}

	return readOutput(map[string]any{
		"job_id":         jobID,
		"logs":           logsText,
		"truncated":      truncated,
		"total_bytes":    totalBytes,
		"returned_lines": returnedLines,
	}), nil
}

// readLogTail streams r through a fixed-capacity circular byte buffer of size maxBytes
// up to maxJobLogsStreamBytes and returns at most tailLines trailing lines (or all
// lines within maxBytes when tailLines == 0).
func readLogTail(r io.Reader, maxBytes, tailLines int) (logsText string, truncated bool, totalBytes int64, returnedLines int, err error) {
	return readLogTailWithLimit(r, maxBytes, tailLines, maxJobLogsStreamBytes)
}

func readLogTailWithLimit(r io.Reader, maxBytes, tailLines int, maxStreamBytes int64) (logsText string, truncated bool, totalBytes int64, returnedLines int, err error) {
	if maxBytes < 1 {
		return "", false, 0, 0, fmt.Errorf("maxBytes must be >= 1")
	}

	ring := make([]byte, maxBytes)
	var chunk [32 * 1024]byte
	writePos := 0

	for {
		n, readErr := r.Read(chunk[:])
		if n > 0 {
			totalBytes += int64(n)
			if maxStreamBytes > 0 && totalBytes > maxStreamBytes {
				return "", false, totalBytes, 0, fmt.Errorf("job log stream exceeds maximum size of %d bytes", maxStreamBytes)
			}

			p := chunk[:n]
			if len(p) >= maxBytes {
				copy(ring, p[len(p)-maxBytes:])
				writePos = 0
			} else {
				first := min(len(p), maxBytes-writePos)
				copy(ring[writePos:], p[:first])
				if rem := len(p) - first; rem > 0 {
					copy(ring, p[first:])
					writePos = rem
				} else {
					writePos += first
					if writePos == maxBytes {
						writePos = 0
					}
				}
			}
		}

		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return "", false, totalBytes, 0, readErr
		}
	}

	if totalBytes == 0 {
		return "", false, 0, 0, nil
	}

	byteTruncated := totalBytes > int64(maxBytes)
	var buf []byte
	if !byteTruncated {
		buf = ring[:totalBytes]
	} else if writePos == 0 {
		buf = ring
	} else {
		buf = make([]byte, maxBytes)
		n1 := copy(buf, ring[writePos:])
		copy(buf[n1:], ring[:writePos])
	}

	if byteTruncated {
		start := 0
		for start < len(buf) && !utf8.RuneStart(buf[start]) {
			start++
		}
		buf = buf[start:]
	}

	if len(buf) == 0 {
		return "", byteTruncated, totalBytes, 0, nil
	}

	searchEnd := len(buf)
	if buf[searchEnd-1] == '\n' {
		searchEnd--
	}

	lineTruncated := false
	if tailLines > 0 {
		linesFound := 0
		cutIdx := 0
		for i := searchEnd - 1; i >= 0; i-- {
			if buf[i] == '\n' {
				linesFound++
				if linesFound == tailLines {
					cutIdx = i + 1
					lineTruncated = cutIdx > 0
					break
				}
			}
		}
		if cutIdx > 0 {
			buf = buf[cutIdx:]
			returnedLines = tailLines
		} else {
			returnedLines = linesFound + 1
		}
	} else {
		returnedLines = bytes.Count(buf[:searchEnd], []byte{'\n'}) + 1
	}

	return string(buf), byteTruncated || lineTruncated, totalBytes, returnedLines, nil
}
