# scafctl-plugin-github

A [scafctl](https://github.com/oakwood-commons/scafctl) plugin that provides the **github** provider for interacting with GitHub via GraphQL and REST APIs.

## Capabilities

| Capability | Description |
|------------|-------------|
| **from** | Read data from GitHub (repos, files, issues, PRs, releases, branches, tags) |
| **transform** | Transform data using GitHub API responses |
| **action** | Write operations (create issues, PRs, commits, releases, repos, labels, etc.) |

## Supported Operations

### Read Operations
`get_repo`, `get_file`, `list_releases`, `get_latest_release`, `list_pull_requests`, `get_pull_request`, `list_issues`, `get_issue`, `list_issue_comments`, `list_branches`, `get_branch`, `list_tags`, `get_head_oid`, `list_pr_comments`, `list_review_threads`, `list_check_runs`, `get_workflow_run`, `list_commit_pulls`, `list_labels`, `list_milestones`, `list_reactions`, `list_collaborators`, `list_webhooks`, `list_workflow_runs`, `list_repo_variables`, `list_environments`, `list_topics`, `list_custom_properties`

### Write Operations
`create_issue`, `update_issue`, `create_issue_comment`, `create_pull_request`, `update_pull_request`, `merge_pull_request`, `close_pull_request`, `create_commit`, `create_branch`, `delete_branch`, `create_tag`, `delete_tag`, `create_release`, `update_release`, `delete_release`, `create_repo`, `update_repo`, `create_ruleset`, `enable_vulnerability_alerts`, `enable_automated_security_fixes`, `create_label`, `update_label`, `delete_label`, `add_labels_to_issue`, `remove_label_from_issue`, `create_milestone`, `update_milestone`, `delete_milestone`, `add_reaction`, `delete_reaction`, `add_collaborator`, `remove_collaborator`, `create_webhook`, `update_webhook`, `delete_webhook`, `dispatch_workflow`, `cancel_workflow_run`, `rerun_workflow`, `create_or_update_variable`, `delete_variable`, `create_or_update_environment`, `delete_environment`, `replace_topics`, `fork_repo`, `create_from_template`, `set_custom_properties`, `reply_to_review_thread`, `resolve_review_thread`

### Compound Operations
`create_fork_pr` -- fork a repository, sync it with upstream, create a branch, commit files, and open a cross-repo pull request in a single action

### Generic
`api_call` -- call any GitHub REST endpoint

## Installation

```bash
# Build from source
task build

# Or install from OCI registry
scafctl plugin install oci://ghcr.io/oakwood-commons/scafctl-plugin-github:latest
```

## Usage

Register this plugin in your scafctl configuration, then reference the **github** provider in your solutions:

```yaml
# Get repository info
resolvers:
  repo-info:
    resolve:
      with:
        - provider: github
          inputs:
            operation: get_repo
            owner: octocat
            repo: hello-world
```

```yaml
# Create a signed commit
actions:
  - provider: github
    inputs:
      operation: create_commit
      owner: my-org
      repo: my-repo
      branch: feature-branch
      message: "feat: add scaffolded files"
      expected_head_oid: abc123def456
      additions:
        - path: src/main.go
          content: "package main\n\nfunc main() {}\n"
```

```yaml
# Fork a repository, commit to it, and open a cross-repo pull request
actions:
  - provider: github
    inputs:
      operation: create_fork_pr
      owner: upstream-org
      repo: upstream-repo
      fork_org: my-fork-org
      # Optional: name the fork explicitly, so forks of different
      # upstreams that share a name (upstream-a/config and
      # upstream-b/config) can coexist in one destination org.
      # Defaults to the upstream repo name. If a fork of this upstream
      # already exists in fork_org under a different name, GitHub returns
      # that existing fork and its name is used instead.
      fork_repo_name: upstream-repo-my-app
      branch: feat/add-config
      message: "feat: add config"
      additions:
        - path: config.yaml
          content: "key: value\n"
```

```yaml
# Generic REST API call
actions:
  - provider: github
    inputs:
      operation: api_call
      endpoint: /repos/my-org/my-repo/labels
      method: POST
      request_body:
        name: help-wanted
        color: "00ff00"
```

## Authentication

Authentication is handled automatically via the configured `github` auth handler in scafctl. No manual token configuration is needed.

For GitHub Enterprise, set the `api_base` input to your instance URL.

## Development

```bash
# Run tests
task test

# Run linter
task lint

# Build
task build

# Full CI pipeline
task ci
```

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for guidelines.

## License

Apache-2.0 -- see [LICENSE](LICENSE) for details.
