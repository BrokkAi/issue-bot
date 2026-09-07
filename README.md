# Brokk Issue Bot

Walk GitHub issues, give a coding agent a focused attempt at each one, and open
pull requests for review. A Go daemon with the same no-config startup as
[release-bot](https://github.com/BrokkAi/release-bot). Both use the shared
[acp-go](https://github.com/BrokkAi/acp-go) protocol client and process runner.

## Install and run

```sh
go install github.com/BrokkAi/issue-bot/cmd/bib@latest
cd /path/to/your-repo
bib
```

Or install a prebuilt Linux/macOS binary (amd64 or arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/BrokkAi/issue-bot/master/install.sh | sh
```

The installer checks the archive's SHA-256 and installs `bib` to `~/.local/bin`.
Set `INSTALL_DIR` to use another directory. From source, `make build` produces
`bin/bib`. Source builds require Go 1.27.1.

Runtime requirements: Git, authenticated `gh` with repository read/push/PR access,
and an authenticated ACP agent. The default is `codex-acp`; if it is missing,
`bib` uses `npx --yes @agentclientprotocol/codex-acp` (requires Node.js and may
download the adapter). Explicit agent commands are used exactly as supplied.

No configuration file is needed. The bot discovers `origin` (or the only remote)
and the repository's default branch. It creates a managed clone and a separate
Git worktree for each issue. Your source checkout and uncommitted edits are left
alone. Starting it authorizes unattended edits, command execution, commits,
branch pushes and PR creation for the configured repository.

```sh
bib /path/to/repo
bib https://github.com/OWNER/REPO.git
bib --label bug --label ready
bib once --issue 123
bib --model YOUR_MODEL_ID --effort low
bib --agent your-acp-agent --agent-arg=--stdio
bib --draft=false
bib status
bib retry --issue 123 --once
```

Flags may appear before or after the repository argument. `--once` attempts at
most one eligible issue (or reconciles one completed job), then exits. `status`
prints saved JSON without starting an agent. `retry` resets pending/blocked
attempt budgets and resumes work; `--issue` restricts it to that issue. Run
`bib --help` for all flags.

## How it works

1. List open issues, oldest first, with pagination. PRs and locked issues are
   excluded. Labels `wontfix`, `duplicate` and `invalid` are excluded by default;
   repeated `--label` flags require all those labels. `--issue` still respects
   eligibility and label filters.
2. Create a persistent `issue-bot/<base-branch-hash>/<issue-number>` branch and
   worktree from the selected remote base branch. The agent reads repository
   instructions, examines the issue and discussion, reproduces the problem,
   implements a focused fix, runs checks, and commits locally.
3. Require a structured completion receipt containing the change explanation
   and validation evidence. Independently check that the worktree is clean, the
   branch is correct, it contains its starting commit, and it has a real diff.
   An optional operator `verify` command must also pass.
4. Recheck issue eligibility, push the issue branch without force, then create
   a draft PR with the explanation, check results and `Fixes #N`. Confirm its
   repository, branches, issue marker and head commit. `--draft=false` creates
   a regular PR. The bot does not merge PRs or close issues itself.

A PR is recorded as **submitted**, not as proof that the issue is solved. Agent
validation evidence is a report; the bot does not infer test correctness from
prose or wait for remote CI. Use repository branch protections and review, and
configure `verify` when you need an independent local gate.

Ambiguous, already-fixed, unsupported or oversized issues can be marked blocked
with a local explanation. They do not prevent attempts on later issues. The bot
posts no issue comments. Transient work failures retry after 15 minutes, up to
three attempts; exhausted jobs require `retry`. Failed work and the previous
failure are preserved. Agent setup failures (login, executable, model or effort)
stop the daemon without consuming an issue attempt.

The daemon polls every five minutes when no issue is ready; while work is
available it proceeds sequentially. Each attempt has a two-hour budget. Saved
pending jobs are checked for an existing PR before further agent work, including
at the attempt limit or after the issue closes. A lost PR-creation response is
reconciled by the persistent branch and issue marker. A closed, unmerged bot PR
blocks the job for inspection. Work is not automatically deleted or reset.

This is a single-daemon workflow. Local state/checkout locks prevent concurrent
local runs; there is no distributed claim across machines. Keep one instance per
repository/base branch. Existing human PRs on other branches are left for the
agent to discover while inspecting the issue; the bot deduplicates its own PRs.

## Optional configuration

`bib --config issue-bot.json` reads a strict JSON object. There is no implicitly
loaded or generated config. Paths are relative to that file. See
[issue-bot.example.json](issue-bot.example.json).

```json
{
  "remote": "https://github.com/OWNER/REPO.git",
  "branch": "main",
  "directory": "var/checkout",
  "state_directory": "var/state",
  "labels": ["bug"],
  "exclude_labels": ["wontfix", "duplicate", "invalid"],
  "draft": true,
  "poll": "5m",
  "timeout": "2h",
  "retry_delay": "15m",
  "attempts": 3,
  "agent": {"command": ["codex-acp"], "model": "YOUR_MODEL_ID", "effort": "low"},
  "verify": ["/opt/checks/verify-issue"]
}
```

`agent` also accepts `environment`, `auth_method` and `mode`. Model and effort
choices are selected and confirmed before work; unavailable choices fail
explicitly. `github.host` supports Enterprise; `github.repo` (`OWNER/REPO`)
identifies a repository when using a local mirror. `instruction_files` defaults
to `AGENTS.md`, `CONTRIBUTING.md`, and `README.md`. `issue` can pin one issue number.

The verifier runs as an argument array, without shell expansion, in the issue
worktree with `ISSUE_NUMBER` set. Keep it outside the agent's writable worktree.

## State and diagnostics

Default state lives below `$XDG_STATE_HOME/issue-bot` or
`~/.local/state/issue-bot`, keyed by remote and base branch. Startup logs the paths.
State writes use atomic replacement and fsync. Private JSONL transcripts live in
`state/sessions`; readable live output goes to stderr. Use `--json` for structured
logs. Retain the managed clone, issue worktrees and state together across restarts.

ACP permission requests are automatically approved. Client filesystem callbacks
are confined by `os.Root`; agents and terminal commands inherit the bot account's
OS rights. This is not a sandbox. Use an account/container appropriate for the
repository and its credentials. Transcripts may include code and command output;
manage their retention externally. Issue text is treated as untrusted problem
data in the agent instructions, not as permission to broaden the task.

## Development

```sh
make check build
./bin/bib --help
```

Tests use temporary Git remotes and simulated GitHub responses. They cover
walking multiple issues, PR reconciliation after a lost response, blocked-job
fairness, retries preserving edits, setup failures, independent verification,
configuration, discovery and locks. They do not run a paid agent or create PRs
in live repositories. The shared ACP library and release-bot additionally test
protocol and subprocess interoperability, model/effort selection and cancellation.

Protocol/API references: [ACP v1](https://agentclientprotocol.com/protocol/v1/overview),
[GitHub issues](https://docs.github.com/en/rest/issues/issues),
[GitHub pull requests](https://docs.github.com/en/rest/pulls/pulls).

Licensed under [Apache-2.0](LICENSE). Repository discovery and CLI/runner patterns
originate in BrokkAi/release-bot; common ACP code is maintained in BrokkAi/acp-go.
