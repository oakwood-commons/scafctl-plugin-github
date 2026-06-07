---
name: provider-authoring
description: "How to create new scafctl plugin providers. Plugin SDK interface, Descriptor, schema, testing patterns, and entry point. Use when adding operations or modifying the provider."
---

# Plugin Provider Authoring Guide

This repo is a **scafctl plugin** (not a built-in provider). Plugins run as
separate binaries and communicate with the scafctl host via gRPC using the
plugin SDK.

## Plugin SDK Interface

Every plugin implements `sdkplugin.ProviderPlugin`:

~~~go
type ProviderPlugin interface {
    GetProviders(ctx context.Context) ([]string, error)
    GetProviderDescriptor(ctx context.Context, name string) (*sdkprovider.Descriptor, error)
    ConfigureProvider(ctx context.Context, name string, cfg ProviderConfig) error
    ExecuteProvider(ctx context.Context, name string, inputs map[string]any) (*sdkprovider.Output, error)
    ExecuteProviderStream(ctx context.Context, name string, inputs map[string]any, cb func(StreamChunk)) error
    DescribeWhatIf(ctx context.Context, name string, inputs map[string]any) (string, error)
}
~~~

## Package Structure

~~~
cmd/scafctl-plugin-github/
    main.go                      # Entry point: sdkplugin.Serve(github.NewPlugin())
internal/github/
    github.go                    # Plugin + Provider structs, Descriptor, Execute dispatch
    github_<domain>.go           # Operation implementations (e.g., github_repo_mgmt.go)
    github_<domain>_test.go      # Tests per domain
    github_benchmark_test.go     # Benchmarks
    graphql.go                   # GraphQL helpers
~~~

## Entry Point

~~~go
package main

import (
    "github.com/oakwood-commons/scafctl-plugin-github/internal/github"
    sdkplugin "github.com/oakwood-commons/scafctl-plugin-sdk/plugin"
)

func main() {
    sdkplugin.Serve(github.NewPlugin())
}
~~~

## Plugin and Provider Pattern

The Plugin wraps a Provider for the SDK interface:

~~~go
type Provider struct {
    descriptor *sdkprovider.Descriptor
    client     *httpc.Client
    // ... config fields
}

type Plugin struct {
    provider *Provider
}

func NewPlugin(opts ...Option) *Plugin {
    return &Plugin{provider: newProvider(opts...)}
}
~~~

## Building the Descriptor

Use `sdkhelper` functions to build the input schema:

~~~go
func newProvider(opts ...Option) *Provider {
    version, _ := semver.NewVersion("2.1.0")
    p := &Provider{
        descriptor: &sdkprovider.Descriptor{
            Name:        ProviderName,
            DisplayName: "GitHub API",
            APIVersion:  "v1",
            Version:     version,
            Description: "...",
            Capabilities: []sdkprovider.Capability{
                sdkprovider.CapabilityFrom,
                sdkprovider.CapabilityAction,
            },
            ReadOperations:  readOperations,
            WriteOperations: writeOperations,
            InputSchema: sdkhelper.ObjectSchema("GitHub provider inputs",
                map[string]*jsonschema.Schema{
                    "operation": sdkhelper.StringProp("Operation to perform",
                        sdkhelper.WithEnum("get_repo", "create_repo", ...),
                    ),
                    "visibility": sdkhelper.StringProp("Repository visibility",
                        sdkhelper.WithEnum("public", "private", "internal"),
                        sdkhelper.WithDefault("private"),
                    ),
                },
                sdkhelper.WithRequired("operation"),
            ),
        },
    }
    for _, opt := range opts {
        opt(p)
    }
    return p
}
~~~

### Schema Helpers

| Helper | Purpose |
|--------|---------|
| `sdkhelper.StringProp(desc, opts...)` | String property |
| `sdkhelper.BoolProp(desc)` | Boolean property |
| `sdkhelper.IntProp(desc, opts...)` | Integer property |
| `sdkhelper.ArrayProp(desc, opts...)` | Array property |
| `sdkhelper.ObjectSchema(desc, props, opts...)` | Object with properties |
| `sdkhelper.WithEnum(values...)` | Restrict to enum values |
| `sdkhelper.WithDefault(value)` | Set default value |
| `sdkhelper.WithMaxLength(n)` | Max string length |
| `sdkhelper.WithRequired(fields...)` | Mark required fields |
| `sdkhelper.WithItems(schema)` | Array item schema |

## Authentication

Use the SDK's host service to get auth tokens:

~~~go
token, err := sdkplugin.GetAuthToken(ctx, "github")
if err != nil {
    return nil, fmt.Errorf("getting auth token: %w", err)
}
// Use token for API calls
~~~

## Dry Run Support

Every operation must check for dry run:

~~~go
func (p *Provider) execute(ctx context.Context, inputs map[string]any) (*sdkprovider.Output, error) {
    if sdkprovider.IsDryRun(ctx) {
        return executeDryRun(operation)
    }
    // ... actual execution
}
~~~

## Adding a New Operation

1. Add the operation name to `allOperations` slice in `github.go`
2. Add to `readOperations` or `WriteOperations` as appropriate
3. Add a `case` in the `execute` switch in `github.go`
4. Implement in the appropriate domain file (e.g., `github_repo_mgmt.go`)
5. Add any new input fields to the schema in the `Descriptor`
6. Add tests in the corresponding `_test.go` file
7. Add a dry-run description in `executeDryRun`

## Testing Patterns

### Test Infrastructure

~~~go
// testProvider creates a Provider wired to a test HTTP server.
func testProvider(t testing.TB, handler http.HandlerFunc) (*Provider, string) {
    t.Helper()
    server := httptest.NewServer(handler)
    t.Cleanup(server.Close)

    client := httpc.NewClient(&httpc.ClientConfig{
        EnableCache:     false,
        RetryMax:        0,
        AllowPrivateIPs: true,
    })
    p := newProvider(
        WithClient(client),
        WithRetryConfig(5, time.Millisecond, 15, time.Millisecond, 3, time.Millisecond),
    )
    return p, server.URL
}
~~~

### GraphQL Handler Helper

~~~go
// graphqlHandler creates a mock that validates GraphQL requests.
p, baseURL := testProvider(t, graphqlHandler(t,
    func(query string, vars map[string]any) {
        input := vars["input"].(map[string]any)
        assert.Equal(t, "PRIVATE", input["visibility"])
    },
    map[string]any{
        "data": map[string]any{
            "createRepository": map[string]any{...},
        },
    },
))
~~~

**Note**: `graphqlHandler` only handles a single GraphQL response. For
operations that make multiple GraphQL calls (e.g., `resolveOwnerID` +
`createRepository`), use a custom handler with `atomic.Int32` call counting.

### Benchmark Tests (Required)

~~~go
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
~~~

## Key SDK Packages

| Import | Alias | Purpose |
|--------|-------|---------|
| `scafctl-plugin-sdk/plugin` | `sdkplugin` | Plugin interface, Serve, auth, streaming |
| `scafctl-plugin-sdk/provider` | `sdkprovider` | Descriptor, Output, Capability, DryRun |
| `scafctl-plugin-sdk/provider/schemahelper` | `sdkhelper` | JSON schema builder functions |

## Build and Test

~~~bash
task build    # Build the plugin binary
task test     # Run tests with -race
task lint     # Run golangci-lint
task bench    # Run benchmarks
task ci       # Full CI pipeline (lint + test + build)
~~~
