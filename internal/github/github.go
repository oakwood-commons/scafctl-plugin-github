// Copyright 2025-2026 Oakwood Commons
// SPDX-License-Identifier: Apache-2.0

// Package github implements the GitHub API provider plugin for scafctl.
//
// It supports read operations (repos, files, issues, PRs, releases, branches, tags)
// via the GitHub GraphQL API, write operations (issues, PRs, commits, branches, tags)
// via GraphQL mutations, and release writes via the REST API (no GraphQL mutation exists).
//
// Commit operations use the createCommitOnBranch GraphQL mutation which produces
// GPG-signed commits automatically -- no local key management required.
//
// Authentication is handled via the HostService auth RPC (GetAuthToken for "github" handler).
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"github.com/go-logr/logr"
	"github.com/google/jsonschema-go/jsonschema"

	"github.com/oakwood-commons/httpc"

	sdkplugin "github.com/oakwood-commons/scafctl-plugin-sdk/plugin"
	sdkprovider "github.com/oakwood-commons/scafctl-plugin-sdk/provider"
	sdkhelper "github.com/oakwood-commons/scafctl-plugin-sdk/provider/schemahelper"
)

// ProviderName is the registered name for this provider.
const ProviderName = "github"

// defaultAPIBase is the default GitHub API base URL.
const defaultAPIBase = "https://api.github.com"

// Default retry configuration values. These are production defaults that can
// be overridden via WithRetryConfig for testing.
const (
	defaultCommitMaxAttempts    = 5
	defaultCommitRetryBackoff   = 3 * time.Second
	defaultWaitMaxAttempts      = 15
	defaultWaitPollInterval     = 2 * time.Second
	defaultInitRepoMaxRetries   = 3
	defaultInitRepoRetryBackoff = 1 * time.Second
)

// allOperations lists every supported operation name for error messages.
var allOperations = []string{
	// Read operations
	"get_repo", "get_file",
	"list_releases", "get_latest_release",
	"list_pull_requests", "get_pull_request",
	"list_issues", "get_issue", "list_issue_comments",
	"list_branches", "get_branch", "list_tags",
	"list_pr_comments",
	// Review thread operations
	"list_review_threads", "reply_to_review_thread", "resolve_review_thread",
	// CI/CD check operations (REST)
	"list_check_runs", "get_workflow_run",
	// Commit lookup operations (REST)
	"list_commit_pulls",
	// Issue write operations
	"create_issue", "update_issue", "create_issue_comment",
	// PR write operations
	"create_pull_request", "update_pull_request", "merge_pull_request", "close_pull_request",
	// Commit & ref operations
	"create_commit", "get_head_oid",
	"create_branch", "delete_branch", "create_tag", "delete_tag",
	// Release write operations (REST)
	"create_release", "update_release", "delete_release",
	// Repository management operations (GraphQL + REST)
	"create_repo", "update_repo", "create_ruleset",
	"enable_vulnerability_alerts", "enable_automated_security_fixes",
	// Label operations (REST)
	"list_labels", "create_label", "update_label", "delete_label",
	"add_labels_to_issue", "remove_label_from_issue",
	// Milestone operations (REST)
	"list_milestones", "create_milestone", "update_milestone", "delete_milestone",
	// Reaction operations (REST)
	"add_reaction", "list_reactions", "delete_reaction",
	// Collaborator operations (REST)
	"list_collaborators", "add_collaborator", "remove_collaborator",
	// Webhook operations (REST)
	"list_webhooks", "create_webhook", "update_webhook", "delete_webhook",
	// GitHub Actions operations (REST)
	"dispatch_workflow", "list_workflow_runs", "cancel_workflow_run", "rerun_workflow",
	"list_repo_variables", "create_or_update_variable", "delete_variable",
	"list_environments", "create_or_update_environment", "delete_environment",
	// Repository settings (REST)
	"list_topics", "replace_topics",
	"fork_repo", "create_from_template",
	// Custom properties (REST)
	"list_custom_properties", "set_custom_properties",
	// Generic API call (REST)
	"api_call",
}

// readOperations are operations that return data (CapabilityFrom/Transform).
var readOperations = map[string]bool{
	"get_repo": true, "get_file": true,
	"list_releases": true, "get_latest_release": true,
	"list_pull_requests": true, "get_pull_request": true,
	"list_issues": true, "get_issue": true, "list_issue_comments": true,
	"list_branches": true, "get_branch": true, "list_tags": true,
	"get_head_oid":        true,
	"list_pr_comments":    true,
	"list_review_threads": true,
	"list_check_runs":     true, "get_workflow_run": true,
	"list_commit_pulls":      true,
	"list_labels":            true,
	"list_milestones":        true,
	"list_reactions":         true,
	"list_collaborators":     true,
	"list_webhooks":          true,
	"list_workflow_runs":     true,
	"list_repo_variables":    true,
	"list_environments":      true,
	"list_topics":            true,
	"list_custom_properties": true,
}

// Provider implements GitHub API operations as a provider.
type Provider struct {
	descriptor *sdkprovider.Descriptor
	// client can be overridden for testing via WithClient option.
	client *httpc.Client

	// Retry configuration -- overridable for testing.
	commitMaxAttempts    int
	commitRetryBackoff   time.Duration
	waitMaxAttempts      int
	waitPollInterval     time.Duration
	initRepoMaxRetries   int
	initRepoRetryBackoff time.Duration
}

// Option configures a Provider.
type Option func(*Provider)

// WithClient sets a custom httpc.Client (useful for testing).
func WithClient(c *httpc.Client) Option {
	return func(p *Provider) {
		p.client = c
	}
}

// WithRetryConfig overrides the default retry timing (useful for testing).
// Attempt counts are clamped to a minimum of 1; durations are clamped to >= 0.
func WithRetryConfig(commitMaxAttempts int, commitRetryBackoff time.Duration, waitMaxAttempts int, waitPollInterval time.Duration, initRepoMaxRetries int, initRepoRetryBackoff time.Duration) Option {
	return func(p *Provider) {
		p.commitMaxAttempts = max(1, commitMaxAttempts)
		p.commitRetryBackoff = max(0, commitRetryBackoff)
		p.waitMaxAttempts = max(1, waitMaxAttempts)
		p.waitPollInterval = max(0, waitPollInterval)
		p.initRepoMaxRetries = max(1, initRepoMaxRetries)
		p.initRepoRetryBackoff = max(0, initRepoRetryBackoff)
	}
}

// Plugin wraps Provider with the sdkplugin.ProviderPlugin interface.
type Plugin struct {
	provider *Provider
}

// NewPlugin creates a new GitHub provider plugin.
func NewPlugin(opts ...Option) *Plugin {
	return &Plugin{
		provider: newProvider(opts...),
	}
}

// newProvider creates a new GitHub API provider.
func newProvider(opts ...Option) *Provider {
	version, _ := semver.NewVersion("2.1.0")

	p := &Provider{
		commitMaxAttempts:    defaultCommitMaxAttempts,
		commitRetryBackoff:   defaultCommitRetryBackoff,
		waitMaxAttempts:      defaultWaitMaxAttempts,
		waitPollInterval:     defaultWaitPollInterval,
		initRepoMaxRetries:   defaultInitRepoMaxRetries,
		initRepoRetryBackoff: defaultInitRepoRetryBackoff,
		descriptor: &sdkprovider.Descriptor{
			Name:        ProviderName,
			DisplayName: "GitHub API",
			APIVersion:  "v1",
			Version:     version,
			Description: "Interact with GitHub via GraphQL (reads, issues, PRs, review threads, signed commits, branches, tags, " +
				"repos, branch protection) and REST (releases, CI check runs, workflow runs, labels, milestones, reactions, " +
				"collaborators, webhooks, Actions workflows, variables, environments, repo settings, tag protection, security settings). " +
				"Includes a generic api_call operation for arbitrary GitHub REST endpoints. " +
				"Uses the configured GitHub auth handler automatically. " +
				"Commit operations use createCommitOnBranch for GPG-signed multi-file atomic commits.",
			Category: "data",
			Capabilities: []sdkprovider.Capability{
				sdkprovider.CapabilityFrom,
				sdkprovider.CapabilityTransform,
				sdkprovider.CapabilityAction,
			},
			Schema: buildInputSchema(),
			OutputSchemas: map[sdkprovider.Capability]*jsonschema.Schema{
				sdkprovider.CapabilityFrom: sdkhelper.ObjectSchema(nil, map[string]*jsonschema.Schema{
					"result": sdkhelper.AnyProp("The API response data -- structure varies by operation"),
				}),
				sdkprovider.CapabilityTransform: sdkhelper.ObjectSchema(nil, map[string]*jsonschema.Schema{
					"result": sdkhelper.AnyProp("The API response data -- structure varies by operation"),
				}),
				sdkprovider.CapabilityAction: sdkhelper.ObjectSchema([]string{"success"}, map[string]*jsonschema.Schema{
					"success":   sdkhelper.BoolProp("Whether the operation succeeded"),
					"result":    sdkhelper.AnyProp("The API response data -- structure varies by operation"),
					"operation": sdkhelper.StringProp("The operation that was performed"),
				}),
			},
			Examples: buildExamples(),
			Tags:     []string{"github", "api", "data", "graphql", "git"},
			WriteOperations: []string{
				// Review thread mutations
				"reply_to_review_thread", "resolve_review_thread",
				// Issue mutations
				"create_issue", "update_issue", "create_issue_comment",
				// PR mutations
				"create_pull_request", "update_pull_request", "merge_pull_request", "close_pull_request",
				// Commit & ref mutations
				"create_commit",
				"create_branch", "delete_branch", "create_tag", "delete_tag",
				// Release mutations
				"create_release", "update_release", "delete_release",
				// Repository management
				"create_repo", "update_repo", "create_ruleset",
				"enable_vulnerability_alerts", "enable_automated_security_fixes",
				// Label mutations
				"create_label", "update_label", "delete_label",
				"add_labels_to_issue", "remove_label_from_issue",
				// Milestone mutations
				"create_milestone", "update_milestone", "delete_milestone",
				// Reaction mutations
				"add_reaction", "delete_reaction",
				// Collaborator mutations
				"add_collaborator", "remove_collaborator",
				// Webhook mutations
				"create_webhook", "update_webhook", "delete_webhook",
				// GitHub Actions mutations
				"dispatch_workflow", "cancel_workflow_run", "rerun_workflow",
				"create_or_update_variable", "delete_variable",
				"create_or_update_environment", "delete_environment",
				// Repository settings mutations
				"replace_topics",
				"fork_repo", "create_from_template",
				// Custom properties mutations
				"set_custom_properties",
			},
			SensitiveFields: []string{"webhook_secret"},
			Links: []sdkprovider.Link{
				{Name: "GitHub GraphQL API", URL: "https://docs.github.com/en/graphql"},
				{Name: "GitHub REST API", URL: "https://docs.github.com/en/rest"},
			},
		},
	}

	for _, opt := range opts {
		opt(p)
	}
	return p
}

// Descriptor returns the provider descriptor.
func (p *Provider) Descriptor() *sdkprovider.Descriptor {
	return p.descriptor
}

// GetProviders returns the list of providers exposed by this plugin.
func (*Plugin) GetProviders(_ context.Context) ([]string, error) {
	return []string{ProviderName}, nil
}

// GetProviderDescriptor returns the descriptor for the named provider.
func (p *Plugin) GetProviderDescriptor(_ context.Context, providerName string) (*sdkprovider.Descriptor, error) {
	if providerName != ProviderName {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	return p.provider.descriptor, nil
}

// ConfigureProvider accepts host configuration.
func (*Plugin) ConfigureProvider(_ context.Context, providerName string, _ sdkplugin.ProviderConfig) error {
	if providerName != ProviderName {
		return fmt.Errorf("unknown provider: %s", providerName)
	}
	return nil
}

// ExecuteProvider performs the GitHub API operation.
func (p *Plugin) ExecuteProvider(ctx context.Context, providerName string, inputs map[string]any) (*sdkprovider.Output, error) {
	if providerName != ProviderName {
		return nil, fmt.Errorf("unknown provider: %s", providerName)
	}
	return p.provider.execute(ctx, inputs)
}

// ExecuteProviderStream returns an error because streaming is not supported.
func (*Plugin) ExecuteProviderStream(_ context.Context, _ string, _ map[string]any, _ func(sdkplugin.StreamChunk)) error {
	return sdkplugin.ErrStreamingNotSupported
}

// DescribeWhatIf returns a human-readable description of what the operation would do.
func (*Plugin) DescribeWhatIf(_ context.Context, providerName string, inputs map[string]any) (string, error) {
	if providerName != ProviderName {
		return "", fmt.Errorf("unknown provider: %s", providerName)
	}
	operation, _ := inputs["operation"].(string)
	owner, _ := inputs["owner"].(string)
	repo, _ := inputs["repo"].(string)
	target := owner + "/" + repo
	return fmt.Sprintf("Would perform GitHub %s on %s", operation, target), nil
}

// ExtractDependencies returns nil (no external dependencies).
func (*Plugin) ExtractDependencies(_ context.Context, _ string, _ map[string]any) ([]string, error) {
	return nil, nil
}

// StopProvider is a no-op.
func (*Plugin) StopProvider(_ context.Context, _ string) error {
	return nil
}

// getClient returns the provider's httpc client, creating a default one if needed.
// In plugin mode, auth is injected via HostService RPC (GetAuthToken for "github" handler).
func (p *Provider) getClient(ctx context.Context) *httpc.Client {
	if p.client != nil {
		return p.client
	}
	cfg := httpc.DefaultConfig()
	cfg.EnableCache = false
	cfg.OnUnauthorized = func(innerCtx context.Context) (string, error) {
		hostClient := sdkplugin.HostClientFromContext(innerCtx)
		if hostClient == nil {
			return "", fmt.Errorf("host service client not available for token refresh")
		}
		resp, err := hostClient.GetAuthToken(innerCtx, "github", "", 0, true)
		if err != nil {
			return "", fmt.Errorf("refreshing github token: %w", err)
		}
		return "Bearer " + resp.AccessToken, nil
	}
	cfg.RequestHooks = append(cfg.RequestHooks, func(req *http.Request) error {
		hostClient := sdkplugin.HostClientFromContext(ctx)
		if hostClient == nil {
			lgr := logr.FromContextOrDiscard(ctx)
			lgr.V(1).Info("host service client not available, making unauthenticated request")
			return nil
		}
		resp, err := hostClient.GetAuthToken(ctx, "github", "", 0, false)
		if err != nil {
			lgr := logr.FromContextOrDiscard(ctx)
			lgr.V(1).Info("GitHub auth token unavailable, making unauthenticated request", "error", err)
			return nil
		}
		req.Header.Set("Authorization", "Bearer "+resp.AccessToken)
		return nil
	})
	return httpc.NewClient(cfg)
}

// execute runs the requested GitHub API operation.
func (p *Provider) execute(ctx context.Context, inputs map[string]any) (*sdkprovider.Output, error) {
	lgr := logr.FromContextOrDiscard(ctx)

	operation, _ := inputs["operation"].(string)
	lgr.V(1).Info("executing provider", "provider", ProviderName, "operation", operation)

	if operation == "" {
		return nil, fmt.Errorf("%s: 'operation' is required", ProviderName)
	}

	// Dry-run support: return mock data for write operations
	if dryRun := sdkprovider.DryRunFromContext(ctx); dryRun {
		return executeDryRun(operation)
	}

	owner, _ := inputs["owner"].(string)
	repo, _ := inputs["repo"].(string)
	apiBase, _ := inputs["api_base"].(string)
	if apiBase == "" {
		apiBase = defaultAPIBase
	}
	apiBase = strings.TrimRight(apiBase, "/")

	// Most operations require owner and repo. Only create_repo and api_call handle
	// these fields internally (owner is optional, repo validated inside).
	if operation != "create_repo" && operation != "api_call" && (owner == "" || repo == "") {
		return nil, fmt.Errorf("%s: 'owner' and 'repo' are required for %s operation", ProviderName, operation)
	}

	client := p.getClient(ctx)

	var result *sdkprovider.Output
	var err error

	switch operation {
	// --- Read operations (GraphQL) ---
	case "get_repo":
		result, err = p.executeGetRepo(ctx, client, apiBase, owner, repo)
	case "get_file":
		result, err = p.executeGetFile(ctx, client, apiBase, owner, repo, inputs)
	case "list_releases":
		result, err = p.executeListReleases(ctx, client, apiBase, owner, repo, inputs)
	case "get_latest_release":
		result, err = p.executeGetLatestRelease(ctx, client, apiBase, owner, repo)
	case "list_pull_requests":
		result, err = p.executeListPullRequests(ctx, client, apiBase, owner, repo, inputs)
	case "get_pull_request":
		result, err = p.executeGetPullRequest(ctx, client, apiBase, owner, repo, inputs)
	case "list_issues":
		result, err = p.executeListIssues(ctx, client, apiBase, owner, repo, inputs)
	case "get_issue":
		result, err = p.executeGetIssue(ctx, client, apiBase, owner, repo, inputs)
	case "list_issue_comments":
		result, err = p.executeListIssueComments(ctx, client, apiBase, owner, repo, inputs)
	case "list_pr_comments":
		result, err = p.executeListPRComments(ctx, client, apiBase, owner, repo, inputs)
	case "list_branches":
		result, err = p.executeListBranches(ctx, client, apiBase, owner, repo, inputs)
	case "get_branch":
		result, err = p.executeGetBranch(ctx, client, apiBase, owner, repo, inputs)
	case "list_tags":
		result, err = p.executeListTags(ctx, client, apiBase, owner, repo, inputs)
	case "get_head_oid":
		result, err = p.executeGetHeadOID(ctx, client, apiBase, owner, repo, inputs)

	// --- Review thread operations (GraphQL) ---
	case "list_review_threads":
		result, err = p.executeListReviewThreads(ctx, client, apiBase, owner, repo, inputs)
	case "reply_to_review_thread":
		result, err = p.executeReplyToReviewThread(ctx, client, apiBase, owner, repo, inputs)
	case "resolve_review_thread":
		result, err = p.executeResolveReviewThread(ctx, client, apiBase, owner, repo, inputs)

	// --- CI/CD check operations (REST) ---
	case "list_check_runs":
		result, err = p.executeListCheckRuns(ctx, client, apiBase, owner, repo, inputs)
	case "get_workflow_run":
		result, err = p.executeGetWorkflowRun(ctx, client, apiBase, owner, repo, inputs)

	// --- Commit lookup operations (REST) ---
	case "list_commit_pulls":
		result, err = p.executeListCommitPulls(ctx, client, apiBase, owner, repo, inputs)

	// --- Issue write operations (GraphQL mutations) ---
	case "create_issue":
		result, err = p.executeCreateIssue(ctx, client, apiBase, owner, repo, inputs)
	case "update_issue":
		result, err = p.executeUpdateIssue(ctx, client, apiBase, owner, repo, inputs)
	case "create_issue_comment":
		result, err = p.executeCreateIssueComment(ctx, client, apiBase, owner, repo, inputs)

	// --- PR write operations (GraphQL mutations) ---
	case "create_pull_request":
		result, err = p.executeCreatePullRequest(ctx, client, apiBase, owner, repo, inputs)
	case "update_pull_request":
		result, err = p.executeUpdatePullRequest(ctx, client, apiBase, owner, repo, inputs)
	case "merge_pull_request":
		result, err = p.executeMergePullRequest(ctx, client, apiBase, owner, repo, inputs)
	case "close_pull_request":
		result, err = p.executeClosePullRequest(ctx, client, apiBase, owner, repo, inputs)

	// --- Commit & ref operations (GraphQL mutations) ---
	case "create_commit":
		result, err = p.executeCreateCommit(ctx, client, apiBase, owner, repo, inputs)
	case "create_branch":
		result, err = p.executeCreateBranch(ctx, client, apiBase, owner, repo, inputs)
	case "delete_branch":
		result, err = p.executeDeleteBranch(ctx, client, apiBase, owner, repo, inputs)
	case "create_tag":
		result, err = p.executeCreateTag(ctx, client, apiBase, owner, repo, inputs)
	case "delete_tag":
		result, err = p.executeDeleteTag(ctx, client, apiBase, owner, repo, inputs)

	// --- Release write operations (REST) ---
	case "create_release":
		result, err = p.executeCreateRelease(ctx, client, apiBase, owner, repo, inputs)
	case "update_release":
		result, err = p.executeUpdateRelease(ctx, client, apiBase, owner, repo, inputs)
	case "delete_release":
		result, err = p.executeDeleteRelease(ctx, client, apiBase, owner, repo, inputs)

	// --- Repository management operations (GraphQL + REST) ---
	case "create_repo":
		result, err = p.executeCreateRepo(ctx, client, apiBase, inputs)
	case "update_repo":
		result, err = p.executeUpdateRepo(ctx, client, apiBase, owner, repo, inputs)
	case "create_ruleset":
		result, err = p.executeCreateRuleset(ctx, client, apiBase, owner, repo, inputs)
	case "enable_vulnerability_alerts":
		result, err = p.executeEnableVulnerabilityAlerts(ctx, client, apiBase, owner, repo)
	case "enable_automated_security_fixes":
		result, err = p.executeEnableAutomatedSecurityFixes(ctx, client, apiBase, owner, repo)

	// --- Label operations (REST) ---
	case "list_labels":
		result, err = p.executeListLabels(ctx, client, apiBase, owner, repo, inputs)
	case "create_label":
		result, err = p.executeCreateLabel(ctx, client, apiBase, owner, repo, inputs)
	case "update_label":
		result, err = p.executeUpdateLabel(ctx, client, apiBase, owner, repo, inputs)
	case "delete_label":
		result, err = p.executeDeleteLabel(ctx, client, apiBase, owner, repo, inputs)
	case "add_labels_to_issue":
		result, err = p.executeAddLabelsToIssue(ctx, client, apiBase, owner, repo, inputs)
	case "remove_label_from_issue":
		result, err = p.executeRemoveLabelFromIssue(ctx, client, apiBase, owner, repo, inputs)

	// --- Milestone operations (REST) ---
	case "list_milestones":
		result, err = p.executeListMilestones(ctx, client, apiBase, owner, repo, inputs)
	case "create_milestone":
		result, err = p.executeCreateMilestone(ctx, client, apiBase, owner, repo, inputs)
	case "update_milestone":
		result, err = p.executeUpdateMilestone(ctx, client, apiBase, owner, repo, inputs)
	case "delete_milestone":
		result, err = p.executeDeleteMilestone(ctx, client, apiBase, owner, repo, inputs)

	// --- Reaction operations (REST) ---
	case "add_reaction":
		result, err = p.executeAddReaction(ctx, client, apiBase, owner, repo, inputs)
	case "list_reactions":
		result, err = p.executeListReactions(ctx, client, apiBase, owner, repo, inputs)
	case "delete_reaction":
		result, err = p.executeDeleteReaction(ctx, client, apiBase, owner, repo, inputs)

	// --- Collaborator operations (REST) ---
	case "list_collaborators":
		result, err = p.executeListCollaborators(ctx, client, apiBase, owner, repo, inputs)
	case "add_collaborator":
		result, err = p.executeAddCollaborator(ctx, client, apiBase, owner, repo, inputs)
	case "remove_collaborator":
		result, err = p.executeRemoveCollaborator(ctx, client, apiBase, owner, repo, inputs)

	// --- Webhook operations (REST) ---
	case "list_webhooks":
		result, err = p.executeListWebhooks(ctx, client, apiBase, owner, repo, inputs)
	case "create_webhook":
		result, err = p.executeCreateWebhook(ctx, client, apiBase, owner, repo, inputs)
	case "update_webhook":
		result, err = p.executeUpdateWebhook(ctx, client, apiBase, owner, repo, inputs)
	case "delete_webhook":
		result, err = p.executeDeleteWebhook(ctx, client, apiBase, owner, repo, inputs)

	// --- GitHub Actions operations (REST) ---
	case "dispatch_workflow":
		result, err = p.executeDispatchWorkflow(ctx, client, apiBase, owner, repo, inputs)
	case "list_workflow_runs":
		result, err = p.executeListWorkflowRuns(ctx, client, apiBase, owner, repo, inputs)
	case "cancel_workflow_run":
		result, err = p.executeCancelWorkflowRun(ctx, client, apiBase, owner, repo, inputs)
	case "rerun_workflow":
		result, err = p.executeRerunWorkflow(ctx, client, apiBase, owner, repo, inputs)
	case "list_repo_variables":
		result, err = p.executeListRepoVariables(ctx, client, apiBase, owner, repo, inputs)
	case "create_or_update_variable":
		result, err = p.executeCreateOrUpdateVariable(ctx, client, apiBase, owner, repo, inputs)
	case "delete_variable":
		result, err = p.executeDeleteVariable(ctx, client, apiBase, owner, repo, inputs)
	case "list_environments":
		result, err = p.executeListEnvironments(ctx, client, apiBase, owner, repo, inputs)
	case "create_or_update_environment":
		result, err = p.executeCreateOrUpdateEnvironment(ctx, client, apiBase, owner, repo, inputs)
	case "delete_environment":
		result, err = p.executeDeleteEnvironment(ctx, client, apiBase, owner, repo, inputs)

	// --- Repo settings (REST) ---
	case "list_topics":
		result, err = p.executeListTopics(ctx, client, apiBase, owner, repo)
	case "replace_topics":
		result, err = p.executeReplaceTopics(ctx, client, apiBase, owner, repo, inputs)
	case "fork_repo":
		result, err = p.executeForkRepo(ctx, client, apiBase, owner, repo, inputs)
	case "create_from_template":
		result, err = p.executeCreateFromTemplate(ctx, client, apiBase, owner, repo, inputs)

	// --- Custom properties (REST) ---
	case "list_custom_properties":
		result, err = p.executeListCustomProperties(ctx, client, apiBase, owner, repo)
	case "set_custom_properties":
		result, err = p.executeSetCustomProperties(ctx, client, apiBase, owner, repo, inputs)

	// --- Generic API call (REST) ---
	case "api_call":
		result, err = p.executeAPICall(ctx, client, apiBase, inputs)

	default:
		return nil, fmt.Errorf("%s: unknown operation %q -- supported: %s", ProviderName, operation, strings.Join(allOperations, ", "))
	}

	if err != nil {
		return nil, fmt.Errorf("%s: %w", ProviderName, err)
	}

	lgr.V(1).Info("provider completed", "provider", ProviderName, "operation", operation)
	return result, nil
}

// executeDryRun returns mock output without making API calls.
func executeDryRun(operation string) (*sdkprovider.Output, error) {
	if readOperations[operation] {
		return &sdkprovider.Output{
			Data: map[string]any{
				"result": map[string]any{
					"dry_run":   true,
					"operation": operation,
				},
			},
		}, nil
	}
	return &sdkprovider.Output{
		Data: map[string]any{
			"success":   true,
			"operation": operation,
			"result": map[string]any{
				"dry_run": true,
			},
		},
	}, nil
}

// readOutput wraps a result in the standard read output shape.
func readOutput(result any) *sdkprovider.Output {
	return &sdkprovider.Output{
		Data: map[string]any{
			"result": result,
		},
	}
}

// actionOutput wraps a result in the standard action output shape.
func actionOutput(operation string, result any) *sdkprovider.Output {
	return &sdkprovider.Output{
		Data: map[string]any{
			"success":   true,
			"operation": operation,
			"result":    result,
		},
	}
}

// getPerPage extracts the per_page value from inputs, defaulting to 30.
func getPerPage(inputs map[string]any) int {
	if v, ok := getIntInput(inputs, "per_page"); ok && v > 0 {
		return v
	}
	return 30
}

// getStringInput extracts a string from the input map.
func getStringInput(inputs map[string]any, key string) string {
	v, _ := inputs[key].(string)
	return v
}

// getStringInputWithAliases extracts a string from the input map, trying the
// primary key first then falling back to aliases in order.
func getStringInputWithAliases(inputs map[string]any, key string, aliases ...string) string {
	if v := getStringInput(inputs, key); v != "" {
		return v
	}
	for _, alias := range aliases {
		if v := getStringInput(inputs, alias); v != "" {
			return v
		}
	}
	return ""
}

// commonInputKeys are top-level inputs handled by execute before dispatch.
// They are excluded from the "received inputs" list in error messages.
var commonInputKeys = []string{"operation", "owner", "repo", "api_base", "token"}

// requiredInputError builds an error for a missing required input, listing the
// operation-specific keys the caller provided so the user can spot typos.
func requiredInputError(operation, field string, inputs map[string]any, hint string) error {
	var userKeys []string
	for k := range inputs {
		if !slices.Contains(commonInputKeys, k) {
			userKeys = append(userKeys, k)
		}
	}
	sort.Strings(userKeys)

	msg := fmt.Sprintf("'%s' is required for %s operation", field, operation)
	if len(userKeys) > 0 {
		msg += fmt.Sprintf(" (received inputs: %s)", strings.Join(userKeys, ", "))
	}
	if hint != "" {
		msg += "; " + hint
	}
	return fmt.Errorf("%s", msg)
}

// getMapInput extracts a map[string]any from the input map.
func getMapInput(inputs map[string]any, key string) map[string]any {
	v, ok := inputs[key].(map[string]any)
	if !ok {
		return nil
	}
	return v
}

// getStringSliceInput extracts a string slice from the input map.
func getStringSliceInput(inputs map[string]any, key string) []string {
	v, ok := inputs[key].([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(v))
	for _, item := range v {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	return result
}

// getBoolInput extracts a bool from the input map.
func getBoolInput(inputs map[string]any, key string) (bool, bool) {
	v, ok := inputs[key].(bool)
	return v, ok
}

// getIntInput extracts an integer from the input map, handling both float64 (from JSON) and int.
func getIntInput(inputs map[string]any, key string) (int, bool) {
	v, ok := inputs[key]
	if !ok {
		return 0, false
	}
	switch n := v.(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case int64:
		return int(n), true
	default:
		return 0, false
	}
}

// restError is returned when the GitHub REST API responds with an HTTP error status.
type restError struct {
	StatusCode int
	Message    string
}

// Error implements the error interface.
func (e *restError) Error() string {
	return fmt.Sprintf("GitHub API error (HTTP %d): %s", e.StatusCode, e.Message)
}

// doRESTRequest performs an authenticated REST API request and returns the parsed JSON.
func (p *Provider) doRESTRequest(ctx context.Context, client *httpc.Client, method, url string, body any) (any, error) {
	var bodyReader io.Reader
	if body != nil {
		bodyBytes, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshaling request body: %w", err)
		}
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("creating REST request: %w", err)
	}

	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("REST request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // best-effort close

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading REST response: %w", err)
	}

	if resp.StatusCode >= 400 {
		msg := string(respBody)
		var ghErr map[string]any
		if json.Unmarshal(respBody, &ghErr) == nil {
			if m, ok := ghErr["message"].(string); ok {
				msg = m
			}
		}
		return nil, &restError{StatusCode: resp.StatusCode, Message: msg}
	}

	// DELETE with 204 No Content
	if resp.StatusCode == http.StatusNoContent {
		return map[string]any{}, nil
	}

	var result any
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parsing REST response JSON: %w", err)
	}

	return result, nil
}

// buildInputSchema constructs the JSON Schema for all GitHub provider operations.
func buildInputSchema() *jsonschema.Schema {
	operationEnums := make([]any, len(allOperations))
	for i, op := range allOperations {
		operationEnums[i] = op
	}

	return sdkhelper.ObjectSchema(
		[]string{"operation"},
		map[string]*jsonschema.Schema{
			// --- Common fields ---
			"operation": sdkhelper.StringProp("GitHub API operation to perform",
				sdkhelper.WithEnum(operationEnums...),
				sdkhelper.WithExample("get_repo"),
			),
			"owner": sdkhelper.StringProp("Repository owner (user or organization)",
				sdkhelper.WithExample("octocat"),
				sdkhelper.WithMaxLength(200),
			),
			"repo": sdkhelper.StringProp("Repository name",
				sdkhelper.WithExample("hello-world"),
				sdkhelper.WithMaxLength(200),
			),
			"api_base": sdkhelper.StringProp("GitHub API base URL. Defaults to https://api.github.com. Set for GitHub Enterprise.",
				sdkhelper.WithDefault(defaultAPIBase),
				sdkhelper.WithFormat("uri"),
			),

			// --- Read operation fields ---
			"path": sdkhelper.StringProp("File path within the repository (for get_file)",
				sdkhelper.WithExample("README.md"),
				sdkhelper.WithMaxLength(1000),
			),
			"ref": sdkhelper.StringProp("Git reference (branch, tag, or commit SHA). Defaults to the repo's default branch.",
				sdkhelper.WithExample("main"),
				sdkhelper.WithMaxLength(200),
			),
			"commit_sha": sdkhelper.StringProp("Commit SHA for list_commit_pulls operation (alias: sha)",
				sdkhelper.WithExample("abc123def456"),
				sdkhelper.WithMaxLength(40),
			),
			"number": sdkhelper.IntProp("Issue or pull request number",
				sdkhelper.WithMinimum(1),
				sdkhelper.WithExample(42),
			),
			"state": sdkhelper.StringProp("Filter by state for list operations",
				sdkhelper.WithEnum("open", "closed", "all", "OPEN", "CLOSED", "MERGED"),
				sdkhelper.WithDefault("open"),
			),
			"per_page": sdkhelper.IntProp("Number of results per page (max 100)",
				sdkhelper.WithMinimum(1),
				sdkhelper.WithMaximum(100),
				sdkhelper.WithDefault(30),
			),

			// --- Issue fields ---
			"title": sdkhelper.StringProp("Title for issue or pull request create/update",
				sdkhelper.WithMaxLength(1000),
			),
			"body": sdkhelper.StringProp("Body text for issue, pull request, release, or comment",
				sdkhelper.WithMaxLength(65536),
			),
			"labels": sdkhelper.ArrayProp("Labels to apply (names for issues, IDs resolved internally)",
				sdkhelper.WithItems(sdkhelper.StringProp("Label name")),
				sdkhelper.WithMaxItems(100),
			),
			"assignees": sdkhelper.ArrayProp("Assignee login usernames",
				sdkhelper.WithItems(sdkhelper.StringProp("Username")),
				sdkhelper.WithMaxItems(10),
			),
			"state_reason": sdkhelper.StringProp("Reason for closing an issue (for update_issue with state=closed)",
				sdkhelper.WithEnum("completed", "not_planned", "reopened"),
			),

			// --- Review thread fields ---
			"thread_id": sdkhelper.StringProp("Review thread node ID (for reply_to_review_thread and resolve_review_thread operations)"),

			// --- CI/CD fields ---
			"run_id": sdkhelper.IntProp("Workflow run ID for get_workflow_run",
				sdkhelper.WithMinimum(1),
			),

			// --- PR fields ---
			"head": sdkhelper.StringProp("Head branch name for creating a pull request",
				sdkhelper.WithMaxLength(200),
			),
			"base": sdkhelper.StringProp("Base branch name for creating/updating a pull request",
				sdkhelper.WithMaxLength(200),
			),
			"draft": sdkhelper.BoolProp("Whether to create the pull request as a draft"),
			"merge_method": sdkhelper.StringProp("Merge method for merge_pull_request",
				sdkhelper.WithEnum("MERGE", "SQUASH", "REBASE"),
				sdkhelper.WithDefault("MERGE"),
			),
			"commit_title": sdkhelper.StringProp("Commit title for merge_pull_request",
				sdkhelper.WithMaxLength(500),
			),
			"commit_message": sdkhelper.StringProp("Commit message body for merge_pull_request",
				sdkhelper.WithMaxLength(65536),
			),

			// --- Commit fields ---
			"branch": sdkhelper.StringProp("Branch name for commit, branch, or tag operations",
				sdkhelper.WithMaxLength(200),
			),
			"message": sdkhelper.StringProp("Commit message headline",
				sdkhelper.WithMaxLength(500),
			),
			"message_body": sdkhelper.StringProp("Commit message body (optional extended description)",
				sdkhelper.WithMaxLength(65536),
			),
			"expected_head_oid": sdkhelper.StringProp("Expected HEAD OID for optimistic locking in create_commit",
				sdkhelper.WithPattern("^[0-9a-f]{40}$"),
			),
			"additions": sdkhelper.ArrayProp("Files to add/update in a commit. Each item: {path, content}",
				sdkhelper.WithItems(sdkhelper.ObjectProp(
					"File addition",
					[]string{"path", "content"},
					map[string]*jsonschema.Schema{
						"path":    sdkhelper.StringProp("File path relative to repository root"),
						"content": sdkhelper.StringProp("File content (plain text, auto-encoded to base64)"),
					},
				)),
				sdkhelper.WithMaxItems(500),
			),
			"deletions": sdkhelper.ArrayProp("File paths to delete in a commit",
				sdkhelper.WithItems(sdkhelper.ObjectProp(
					"File deletion",
					[]string{"path"},
					map[string]*jsonschema.Schema{
						"path": sdkhelper.StringProp("File path relative to repository root"),
					},
				)),
				sdkhelper.WithMaxItems(500),
			),

			// --- Ref fields ---
			"oid": sdkhelper.StringProp("Git object ID (commit SHA) for branch/tag creation",
				sdkhelper.WithPattern("^[0-9a-f]{40}$"),
			),
			"tag": sdkhelper.StringProp("Tag name for create_tag/delete_tag",
				sdkhelper.WithMaxLength(200),
			),

			// --- Release fields (REST) ---
			"tag_name": sdkhelper.StringProp("Tag name for the release",
				sdkhelper.WithMaxLength(200),
			),
			"target_commitish": sdkhelper.StringProp("Commitish value for the release tag (branch or SHA)",
				sdkhelper.WithMaxLength(200),
			),
			"name": sdkhelper.StringProp("Release name/title",
				sdkhelper.WithMaxLength(500),
			),
			"prerelease": sdkhelper.BoolProp("Whether this is a prerelease"),
			"release_id": sdkhelper.IntProp("Release ID for update_release/delete_release",
				sdkhelper.WithMinimum(1),
			),

			// --- Repo management fields ---
			"description": sdkhelper.StringProp("Repository or resource description",
				sdkhelper.WithMaxLength(1000),
			),
			"visibility": sdkhelper.StringProp("Repository visibility for create_repo",
				sdkhelper.WithEnum("public", "private"),
				sdkhelper.WithDefault("public"),
			),
			"auto_init":    sdkhelper.BoolProp("Initialize repo with a README (uses REST API; GraphQL lacks auto_init support)"),
			"ruleset_name": sdkhelper.StringProp("Name for the repository ruleset", sdkhelper.WithMaxLength(200)),
			"target": sdkhelper.StringProp("Ruleset target type",
				sdkhelper.WithEnum("branch", "tag"),
				sdkhelper.WithDefault("branch"),
			),
			"enforcement": sdkhelper.StringProp("Ruleset enforcement level",
				sdkhelper.WithEnum("active", "disabled", "evaluate"),
				sdkhelper.WithDefault("active"),
			),
			"include_refs": sdkhelper.ArrayProp("Ref patterns to include",
				sdkhelper.WithItems(sdkhelper.StringProp("Ref pattern")),
				sdkhelper.WithMaxItems(100),
			),
			"exclude_refs": sdkhelper.ArrayProp("Ref patterns to exclude",
				sdkhelper.WithItems(sdkhelper.StringProp("Ref pattern")),
				sdkhelper.WithMaxItems(100),
			),
			"required_status_checks_contexts": sdkhelper.ArrayProp("Required status check context names",
				sdkhelper.WithItems(sdkhelper.StringProp("Context name")),
				sdkhelper.WithMaxItems(50),
			),
			"required_approving_review_count": sdkhelper.IntProp("Minimum approving reviews required",
				sdkhelper.WithMinimum(0),
				sdkhelper.WithMaximum(10),
			),
			"required_linear_history":    sdkhelper.BoolProp("Require linear commit history"),
			"allow_force_pushes":         sdkhelper.BoolProp("Allow force pushes to matching refs"),
			"allow_deletions":            sdkhelper.BoolProp("Allow deletion of matching refs"),
			"requires_commit_signatures": sdkhelper.BoolProp("Require signed commits"),

			// --- Label fields ---
			"label_name":        sdkhelper.StringProp("Label name for create/update/delete label operations", sdkhelper.WithMaxLength(200)),
			"new_label_name":    sdkhelper.StringProp("New name when renaming a label (update_label)", sdkhelper.WithMaxLength(200)),
			"color":             sdkhelper.StringProp("Hex color code for labels (without #)", sdkhelper.WithPattern("^[0-9a-fA-F]{6}$"), sdkhelper.WithExample("00ff00")),
			"label_description": sdkhelper.StringProp("Description for a label", sdkhelper.WithMaxLength(1000)),

			// --- Milestone fields ---
			"milestone_number": sdkhelper.IntProp("Milestone number for update/delete operations", sdkhelper.WithMinimum(1)),
			"due_on":           sdkhelper.StringProp("Due date for milestone (ISO 8601: YYYY-MM-DDTHH:MM:SSZ)", sdkhelper.WithFormat("date-time")),

			// --- Reaction fields ---
			"reaction_content": sdkhelper.StringProp("Reaction emoji to add",
				sdkhelper.WithEnum("+1", "-1", "laugh", "confused", "heart", "hooray", "rocket", "eyes"),
			),
			"reaction_subject": sdkhelper.StringProp("Subject type for the reaction",
				sdkhelper.WithEnum("issue", "pull_request", "issue_comment"),
				sdkhelper.WithDefault("issue"),
			),
			"reaction_id": sdkhelper.IntProp("Reaction ID for delete_reaction", sdkhelper.WithMinimum(1)),
			"comment_id":  sdkhelper.IntProp("Comment ID for reaction/comment operations", sdkhelper.WithMinimum(1)),

			// --- Collaborator fields ---
			"username":   sdkhelper.StringProp("GitHub username for collaborator operations", sdkhelper.WithMaxLength(200)),
			"permission": sdkhelper.StringProp("Permission level for collaborators", sdkhelper.WithEnum("pull", "triage", "push", "maintain", "admin")),

			// --- Webhook fields ---
			"webhook_url": sdkhelper.StringProp("Payload URL for the webhook", sdkhelper.WithFormat("uri")),
			"webhook_events": sdkhelper.ArrayProp("Events that trigger the webhook",
				sdkhelper.WithItems(sdkhelper.StringProp("Event name")),
				sdkhelper.WithMaxItems(50),
			),
			"webhook_content_type": sdkhelper.StringProp("Content type for webhook payloads",
				sdkhelper.WithEnum("json", "form"),
				sdkhelper.WithDefault("json"),
			),
			"webhook_secret": sdkhelper.StringProp("Secret for webhook signature verification"),
			"webhook_active": sdkhelper.BoolProp("Whether the webhook is active"),
			"hook_id":        sdkhelper.IntProp("Webhook ID for update/delete operations", sdkhelper.WithMinimum(1)),

			// --- GitHub Actions fields ---
			"workflow_id":     sdkhelper.StringProp("Workflow file name or ID (e.g. ci.yml or numeric ID)", sdkhelper.WithExample("ci.yml")),
			"workflow_inputs": sdkhelper.ObjectProp("Input parameters for workflow dispatch", nil, nil),
			"workflow_status": sdkhelper.StringProp("Filter workflow runs by status",
				sdkhelper.WithEnum("completed", "action_required", "cancelled", "failure", "neutral",
					"skipped", "stale", "success", "timed_out", "in_progress", "queued", "requested", "waiting"),
			),

			// --- Variable fields ---
			"variable_name":  sdkhelper.StringProp("Repository variable name", sdkhelper.WithMaxLength(200)),
			"variable_value": sdkhelper.StringProp("Repository variable value", sdkhelper.WithMaxLength(65536)),

			// --- Environment fields ---
			"environment_name": sdkhelper.StringProp("Deployment environment name", sdkhelper.WithMaxLength(200)),
			"wait_timer":       sdkhelper.IntProp("Wait timer in minutes before deployments proceed", sdkhelper.WithMinimum(0), sdkhelper.WithMaximum(43200)),
			"reviewers": sdkhelper.ArrayProp("Required reviewers for environment",
				sdkhelper.WithItems(sdkhelper.ObjectProp("Reviewer object", nil, nil)),
				sdkhelper.WithMaxItems(6),
			),

			// --- Repo settings fields ---
			"homepage":               sdkhelper.StringProp("Repository homepage URL", sdkhelper.WithFormat("uri")),
			"default_branch":         sdkhelper.StringProp("Default branch name", sdkhelper.WithMaxLength(200)),
			"has_issues":             sdkhelper.BoolProp("Enable issues feature"),
			"has_projects":           sdkhelper.BoolProp("Enable projects feature"),
			"has_wiki":               sdkhelper.BoolProp("Enable wiki feature"),
			"allow_squash_merge":     sdkhelper.BoolProp("Allow squash merging"),
			"allow_merge_commit":     sdkhelper.BoolProp("Allow merge commits"),
			"allow_rebase_merge":     sdkhelper.BoolProp("Allow rebase merging"),
			"delete_branch_on_merge": sdkhelper.BoolProp("Automatically delete head branches after merge"),
			"archived":               sdkhelper.BoolProp("Archive the repository"),
			"topics": sdkhelper.ArrayProp("Repository topics/tags",
				sdkhelper.WithItems(sdkhelper.StringProp("Topic name")),
				sdkhelper.WithMaxItems(20),
			),
			"organization":         sdkhelper.StringProp("Organization to fork into (fork_repo)", sdkhelper.WithMaxLength(200)),
			"default_branch_only":  sdkhelper.BoolProp("Fork only the default branch"),
			"new_repo_name":        sdkhelper.StringProp("Name for new repository (create_from_template)", sdkhelper.WithMaxLength(200)),
			"new_owner":            sdkhelper.StringProp("Owner for new repository (create_from_template)", sdkhelper.WithMaxLength(200)),
			"include_all_branches": sdkhelper.BoolProp("Include all branches when creating from template"),

			// --- Custom properties fields ---
			"properties": sdkhelper.ObjectProp("Custom properties to set (key-value map)", nil, nil),

			// --- Generic API call fields ---
			"endpoint": sdkhelper.StringProp("Relative API path for api_call. Must start with /.",
				sdkhelper.WithExample("/repos/octocat/hello-world/labels"),
				sdkhelper.WithMaxLength(2000),
			),
			"method": sdkhelper.StringProp("HTTP method for api_call",
				sdkhelper.WithEnum("GET", "POST", "PUT", "PATCH", "DELETE"),
				sdkhelper.WithDefault("GET"),
			),
			"query_params": sdkhelper.ObjectProp("Query parameters for api_call", nil, nil),
			"request_body": sdkhelper.ObjectProp("Request body (JSON object) for api_call POST/PUT/PATCH", nil, nil),
		},
	)
}

// buildExamples provides configuration examples.
func buildExamples() []sdkprovider.Example {
	return []sdkprovider.Example{
		{
			Name:        "Get repository info",
			Description: "Fetch metadata for a GitHub repository",
			YAML: `operation: get_repo
owner: octocat
repo: hello-world`,
		},
		{
			Name:        "Get file content",
			Description: "Retrieve a file from a repository at a specific branch",
			YAML: `operation: get_file
owner: octocat
repo: hello-world
path: README.md
ref: main`,
		},
		{
			Name:        "Create a signed commit",
			Description: "Atomically commit multiple files with GPG signing",
			YAML: `operation: create_commit
owner: my-org
repo: my-repo
branch: feature-branch
message: "feat: add scaffolded files"
expected_head_oid: abc123def456
additions:
  - path: src/main.go
    content: "package main\n\nfunc main() {}\n"`,
		},
		{
			Name:        "Create an issue",
			Description: "Create a new issue in a repository",
			YAML: `operation: create_issue
owner: my-org
repo: my-repo
title: "Bug: something is broken"
body: "Steps to reproduce..."
labels:
  - bug`,
		},
		{
			Name:        "List pull requests",
			Description: "List open pull requests",
			YAML: `operation: list_pull_requests
owner: my-org
repo: my-repo
state: open`,
		},
		{
			Name:        "Generic API call",
			Description: "Call any GitHub REST endpoint",
			YAML: `operation: api_call
endpoint: /repos/my-org/my-repo/labels
method: GET`,
		},
	}
}
