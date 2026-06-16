// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"testing"

	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
)

func BenchmarkProvider_Execute_DryRun(b *testing.B) {
	p := newProvider()

	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "get_repo",
		"owner":     "example-org",
		"repo":      "example-repo",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

func BenchmarkProvider_Execute_CreateForkPR_DryRun(b *testing.B) {
	p := newProvider()

	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "create_fork_pr",
		"owner":     "example-org",
		"repo":      "example-repo",
		"fork_org":  "my-fork-org",
		"branch":    "feature-branch",
		"message":   "feat: add scaffolded files",
		"additions": []any{
			map[string]any{"path": "src/main.go", "content": "package main"},
		},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

func BenchmarkProvider_Execute_StateLoad_DryRun(b *testing.B) {
	p := newProvider()

	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "state_load",
		"owner":     "example-org",
		"repo":      "example-repo",
		"path":      "state/app.json",
		"ref":       "main",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

func BenchmarkProvider_Execute_StateSave_DryRun(b *testing.B) {
	p := newProvider()

	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "state_save",
		"owner":     "example-org",
		"repo":      "example-repo",
		"path":      "state/app.json",
		"branch":    "main",
		"data":      map[string]any{"key": "value"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}

func BenchmarkProvider_Execute_StateDelete_DryRun(b *testing.B) {
	p := newProvider()

	ctx := sdkprovider.WithDryRun(context.Background(), true)
	inputs := map[string]any{
		"operation": "state_delete",
		"owner":     "example-org",
		"repo":      "example-repo",
		"path":      "state/app.json",
		"branch":    "main",
	}

	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		_, _ = p.execute(ctx, inputs)
	}
}
