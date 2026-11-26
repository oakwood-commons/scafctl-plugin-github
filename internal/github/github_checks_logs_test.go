// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// smallChunkReader forces small Read slices so ring-buffer wraparound
// (writePos != 0, writePos == maxBytes, and rem > 0) is exercised directly.
type smallChunkReader struct {
	r         io.Reader
	chunkSize int
}

func (s *smallChunkReader) Read(p []byte) (int, error) {
	if len(p) > s.chunkSize {
		p = p[:s.chunkSize]
	}
	return s.r.Read(p)
}

func TestReadLogTail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		input         string
		maxBytes      int
		tailLines     int
		wantLogs      string
		wantTruncated bool
		wantTotal     int64
		wantLines     int
	}{
		{
			name:          "empty stream",
			input:         "",
			maxBytes:      1024,
			tailLines:     200,
			wantLogs:      "",
			wantTruncated: false,
			wantTotal:     0,
			wantLines:     0,
		},
		{
			name:          "fits within maxBytes and tailLines with trailing newline",
			input:         "line1\nline2\nline3\n",
			maxBytes:      1024,
			tailLines:     10,
			wantLogs:      "line1\nline2\nline3\n",
			wantTruncated: false,
			wantTotal:     18,
			wantLines:     3,
		},
		{
			name:          "fits within maxBytes without trailing newline",
			input:         "line1\nline2",
			maxBytes:      1024,
			tailLines:     10,
			wantLogs:      "line1\nline2",
			wantTruncated: false,
			wantTotal:     11,
			wantLines:     2,
		},
		{
			name:          "tailLines slices last N lines",
			input:         "l1\nl2\nl3\nl4\nl5\n",
			maxBytes:      1024,
			tailLines:     2,
			wantLogs:      "l4\nl5\n",
			wantTruncated: true,
			wantTotal:     15,
			wantLines:     2,
		},
		{
			name:          "tailLines zero returns all lines within maxBytes",
			input:         "l1\nl2\nl3\nl4\n",
			maxBytes:      1024,
			tailLines:     0,
			wantLogs:      "l1\nl2\nl3\nl4\n",
			wantTruncated: false,
			wantTotal:     12,
			wantLines:     4,
		},
		{
			name:          "exact maxBytes boundary is not truncated",
			input:         "12345\n",
			maxBytes:      6,
			tailLines:     10,
			wantLogs:      "12345\n",
			wantTruncated: false,
			wantTotal:     6,
			wantLines:     1,
		},
		{
			name:          "ring buffer wraps and preserves tail end of multi-chunk stream",
			input:         strings.Repeat("prefix-line-0123456789\n", 500) + "final-1\nfinal-2\n",
			maxBytes:      32,
			tailLines:     2,
			wantLogs:      "final-1\nfinal-2\n",
			wantTruncated: true,
			wantTotal:     int64(len(strings.Repeat("prefix-line-0123456789\n", 500) + "final-1\nfinal-2\n")),
			wantLines:     2,
		},
		{
			name:          "trailing continuation bytes only when maxBytes cuts inside final rune",
			input:         "hello 🔥",
			maxBytes:      2,
			tailLines:     10,
			wantLogs:      "",
			wantTruncated: true,
			wantTotal:     int64(len("hello 🔥")),
			wantLines:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			reader := &smallChunkReader{r: strings.NewReader(tt.input), chunkSize: 7}
			logs, truncated, totalBytes, returnedLines, err := readLogTail(reader, tt.maxBytes, tt.tailLines)
			require.NoError(t, err)
			assert.Equal(t, tt.wantLogs, logs)
			assert.Equal(t, tt.wantTruncated, truncated)
			assert.Equal(t, tt.wantTotal, totalBytes)
			assert.Equal(t, tt.wantLines, returnedLines)
		})
	}
}

func TestReadLogTail_UTF8WrapAlignment(t *testing.T) {
	t.Parallel()

	// Construct payload with 2-byte (é), 3-byte (─), and 4-byte (🔥) UTF-8 runes
	// and test every possible wrap offset modulo 4 so continuation bytes land at index 0.
	payload := strings.Repeat("🔥─é", 40) + "\nerror: assertion failed 🔥\n"
	for extra := range 8 {
		maxBytes := 40 + extra
		logs, truncated, total, lines, err := readLogTail(strings.NewReader(payload), maxBytes, 0)
		require.NoError(t, err)
		assert.True(t, truncated)
		assert.Equal(t, int64(len(payload)), total)
		assert.GreaterOrEqual(t, lines, 1)
		assert.True(t, utf8.ValidString(logs), "wrapped tail must always start on a valid UTF-8 rune boundary (maxBytes=%d)", maxBytes)
		assert.True(t, strings.HasSuffix(logs, "\nerror: assertion failed 🔥\n"))
	}
}

func TestReadLogTailWithLimit_StreamLimitExceeded(t *testing.T) {
	t.Parallel()

	big := bytes.Repeat([]byte("a"), 2048)
	_, _, _, _, err := readLogTailWithLimit(bytes.NewReader(big), 256, 50, 1024)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "exceeds maximum size")
}

func TestProvider_Execute_GetJobLogs(t *testing.T) {
	t.Parallel()

	t.Run("fetches and tails job logs via REST", func(t *testing.T) {
		t.Parallel()

		var body strings.Builder
		for i := 1; i <= 10; i++ {
			fmt.Fprintf(&body, "2026-01-01T00:00:%02dZ line %d\n", i, i)
		}
		rawLogs := body.String()

		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			assert.Equal(t, http.MethodGet, r.Method)
			assert.Equal(t, "/repos/public-org/demo/actions/jobs/778899/logs", r.URL.Path)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte(rawLogs))
		})

		out, err := p.execute(context.Background(), map[string]any{
			"operation":  "get_job_logs",
			"owner":      "public-org",
			"repo":       "demo",
			"job_id":     778899,
			"tail_lines": 3,
			"api_base":   baseURL,
		})
		require.NoError(t, err)

		res := out.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, 778899, res["job_id"])
		assert.Equal(t, true, res["truncated"])
		assert.Equal(t, int64(len(rawLogs)), res["total_bytes"])
		assert.Equal(t, 3, res["returned_lines"])
		logs := res["logs"].(string)
		assert.Contains(t, logs, "line 8\n")
		assert.Contains(t, logs, "line 10\n")
		assert.NotContains(t, logs, "line 7\n")
	})

	t.Run("validates inputs and surfaces HTTP errors", func(t *testing.T) {
		t.Parallel()

		p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
		})

		invalidCases := []struct {
			name    string
			inputs  map[string]any
			wantErr string
		}{
			{
				name:    "missing job_id",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r"},
				wantErr: "'job_id' is required",
			},
			{
				name:    "zero job_id",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 0},
				wantErr: "'job_id' must be an integer >= 1",
			},
			{
				name:    "fractional job_id rejected without truncation",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 42.9},
				wantErr: "'job_id' must be an integer >= 1",
			},
			{
				name:    "negative fractional tail_lines rejected without truncating to zero",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "tail_lines": -0.5},
				wantErr: "'tail_lines' must be an integer",
			},
			{
				name:    "positive fractional tail_lines rejected",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "tail_lines": 10.25},
				wantErr: "'tail_lines' must be an integer",
			},
			{
				name:    "NaN tail_lines rejected",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "tail_lines": math.NaN()},
				wantErr: "'tail_lines' must be an integer",
			},
			{
				name:    "Inf max_bytes rejected",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "max_bytes": math.Inf(1)},
				wantErr: "'max_bytes' must be an integer",
			},
			{
				name:    "fractional max_bytes rejected",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "max_bytes": 1024.5},
				wantErr: "'max_bytes' must be an integer",
			},
			{
				name:    "fractional per_page rejected on list_pr_reviews",
				inputs:  map[string]any{"operation": "list_pr_reviews", "owner": "o", "repo": "r", "number": 1, "per_page": 1.5},
				wantErr: "list_pr_reviews: 'per_page' must be an integer >= 1",
			},
			{
				name:    "fractional per_page rejected on list_review_threads",
				inputs:  map[string]any{"operation": "list_review_threads", "owner": "o", "repo": "r", "number": 1, "per_page": 1.5},
				wantErr: "list_review_threads: 'per_page' must be an integer >= 1",
			},
			{
				name:    "fractional number rejected on list_pr_reviews",
				inputs:  map[string]any{"operation": "list_pr_reviews", "owner": "o", "repo": "r", "number": 7.8},
				wantErr: "list_pr_reviews: 'number' must be an integer >= 1",
			},
			{
				name:    "fractional number rejected on list_review_threads",
				inputs:  map[string]any{"operation": "list_review_threads", "owner": "o", "repo": "r", "number": 7.8},
				wantErr: "list_review_threads: 'number' must be an integer >= 1",
			},
			{
				name:    "fractional number rejected on request_reviewers",
				inputs:  map[string]any{"operation": "request_reviewers", "owner": "o", "repo": "r", "number": 7.8, "reviewers": []any{"alice"}},
				wantErr: "request_reviewers: 'number' must be an integer >= 1",
			},
			{
				name:    "fractional number rejected on update_pull_request",
				inputs:  map[string]any{"operation": "update_pull_request", "owner": "o", "repo": "r", "number": 7.8, "draft": true},
				wantErr: "update_pull_request: 'number' must be an integer >= 1",
			},
			{
				name:    "negative tail_lines",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "tail_lines": -1},
				wantErr: "'tail_lines' must be between 0 and 10000",
			},
			{
				name:    "excessive tail_lines",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "tail_lines": 10001},
				wantErr: "'tail_lines' must be between 0 and 10000",
			},
			{
				name:    "zero max_bytes",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "max_bytes": 0},
				wantErr: "'max_bytes' must be between 1 and 10485760",
			},
			{
				name:    "excessive max_bytes",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 1, "max_bytes": 20000000},
				wantErr: "'max_bytes' must be between 1 and 10485760",
			},
			{
				name:    "HTTP 404 surfaces message",
				inputs:  map[string]any{"operation": "get_job_logs", "owner": "o", "repo": "r", "job_id": 999, "api_base": baseURL},
				wantErr: "HTTP 404",
			},
		}

		for _, tc := range invalidCases {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				_, err := p.execute(context.Background(), tc.inputs)
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			})
		}

		// Dry-run returns read-shaped output
		dryOut, err := p.execute(sdkprovider.WithDryRun(context.Background(), true), map[string]any{
			"operation": "get_job_logs",
			"owner":     "public-org",
			"repo":      "demo",
			"job_id":    123,
		})
		require.NoError(t, err)
		dryRes := dryOut.Data.(map[string]any)["result"].(map[string]any)
		assert.Equal(t, true, dryRes["dry_run"])
	})
}

func TestGetIntInput_StrictIntegralValidation(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		val    any
		want   int
		wantOK bool
	}{
		{name: "integral float64", val: float64(42), want: 42, wantOK: true},
		{name: "zero float64", val: float64(0), want: 0, wantOK: true},
		{name: "negative integral float64", val: float64(-7), want: -7, wantOK: true},
		{name: "positive fractional float64", val: float64(42.9), want: 0, wantOK: false},
		{name: "negative fractional float64", val: float64(-0.5), want: 0, wantOK: false},
		{name: "NaN float64", val: math.NaN(), want: 0, wantOK: false},
		{name: "positive Inf float64", val: math.Inf(1), want: 0, wantOK: false},
		{name: "negative Inf float64", val: math.Inf(-1), want: 0, wantOK: false},
		{name: "MinInt float64 exact", val: float64(math.MinInt), want: math.MinInt, wantOK: true},
		{name: "MaxInt+1 float64 overflow", val: -float64(math.MinInt), want: 0, wantOK: false},
		{name: "below MinInt float64 underflow", val: float64(math.MinInt) * 2, want: 0, wantOK: false},
		{name: "json.Number integer", val: json.Number("12345"), want: 12345, wantOK: true},
		{name: "json.Number fractional", val: json.Number("12.5"), want: 0, wantOK: false},
		{name: "json.Number overflow", val: json.Number("99999999999999999999999999999"), want: 0, wantOK: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := getIntInput(map[string]any{"k": tc.val}, "k")
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestOptionalIntInputs_RejectInvalidWithoutSendingRequest(t *testing.T) {
	t.Parallel()

	t.Run("create_ruleset rejects invalid required_approving_review_count before sending request", func(t *testing.T) {
		t.Parallel()

		var reqCount atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			reqCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": float64(1)})
		})

		invalidCounts := []any{1.5, -0.5, -1, 11, "2", math.NaN(), math.Inf(1)}
		for _, bad := range invalidCounts {
			_, err := p.execute(context.Background(), map[string]any{
				"operation":                       "create_ruleset",
				"owner":                           "public-org",
				"repo":                            "demo",
				"ruleset_name":                    "main-protection",
				"include_refs":                    []any{"refs/heads/main"},
				"required_approving_review_count": bad,
				"api_base":                        baseURL,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "create_ruleset: 'required_approving_review_count' must be an integer between 0 and 10")
		}
		assert.Zero(t, reqCount.Load(), "no HTTP request should be sent for invalid required_approving_review_count")
	})

	t.Run("create_or_update_environment rejects invalid wait_timer before sending request", func(t *testing.T) {
		t.Parallel()

		var reqCount atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, r *http.Request) {
			reqCount.Add(1)
			var body map[string]any
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			assert.Equal(t, float64(15), body["wait_timer"])
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": "prod"})
		})

		invalidTimers := []any{1.5, -0.5, -1, 43201, "30", math.NaN(), math.Inf(1)}
		for _, bad := range invalidTimers {
			_, err := p.execute(context.Background(), map[string]any{
				"operation":        "create_or_update_environment",
				"owner":            "public-org",
				"repo":             "demo",
				"environment_name": "prod",
				"wait_timer":       bad,
				"api_base":         baseURL,
			})
			require.Error(t, err)
			assert.Contains(t, err.Error(), "create_or_update_environment: 'wait_timer' must be an integer between 0 and 43200")
		}
		assert.Zero(t, reqCount.Load(), "no HTTP request should be sent for invalid wait_timer")

		// Valid integral wait_timer succeeds and sends the field.
		_, err := p.execute(context.Background(), map[string]any{
			"operation":        "create_or_update_environment",
			"owner":            "public-org",
			"repo":             "demo",
			"environment_name": "prod",
			"wait_timer":       15,
			"api_base":         baseURL,
		})
		require.NoError(t, err)
		assert.Equal(t, int32(1), reqCount.Load())
	})

	t.Run("list operations reject invalid per_page before sending request", func(t *testing.T) {
		t.Parallel()

		var reqCount atomic.Int32
		p, baseURL := testProvider(t, func(w http.ResponseWriter, _ *http.Request) {
			reqCount.Add(1)
			w.WriteHeader(http.StatusOK)
		})

		_, err := p.execute(context.Background(), map[string]any{
			"operation": "list_workflow_runs",
			"owner":     "public-org",
			"repo":      "demo",
			"per_page":  1.5,
			"api_base":  baseURL,
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list_workflow_runs: 'per_page' must be an integer >= 1")
		assert.Zero(t, reqCount.Load())
	})
}

func BenchmarkReadLogTail(b *testing.B) {
	// 1 MiB synthetic CI log streamed through default 256 KiB circular buffer.
	line := []byte("2026-01-01T00:00:00.0000000Z === RUN   TestSomething/subcase_alpha_bravo_charlie\n")
	repeats := (1024 * 1024) / len(line)
	payload := bytes.Repeat(line, repeats)

	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _, _, _, err := readLogTail(bytes.NewReader(payload), defaultJobLogsMaxBytes, defaultJobLogsTailLines)
		if err != nil {
			b.Fatal(err)
		}
	}
}
