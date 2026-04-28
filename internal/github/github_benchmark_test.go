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
