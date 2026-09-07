# Brokk Issue Bot

Walk GitHub issues, give a coding agent a focused attempt at each one, and open
pull requests for review. A Go daemon with the same no-config startup as
[release-bot](https://github.com/BrokkAi/release-bot). Both use the shared
[acp-go](https://github.com/BrokkAi/acp-go) protocol client and process runner.

## Install and run

```sh
npm install -g @brokkai/issue-bot
cd /path/to/your-repo
bib
```

For a single invocation, use `npx --yes @brokkai/issue-bot` from your repository.
The npm package requires Node.js 18+ and includes the native binary for
Linux/macOS on x64 or arm64 through optional dependencies. Keep optional
dependencies enabled; installing and running the package does not require Go.

Or install a prebuilt Linux/macOS binary (amd64 or arm64):

```sh
curl -fsSL https://raw.githubusercontent.com/BrokkAi/issue-bot/master/install.sh | sh
```

The installer checks the archive's SHA-256 and installs `bib` to `~/.local/bin`.
Set `INSTALL_DIR` to use another directory. From source, `make build` produces
`bin/bib`. Source builds require Go 1.27.1. You can also install from Go:

```sh
go install github.com/BrokkAi/issue-bot/cmd/bib@latest
```

Runtime requirements: Git, authenticated `gh` with repository read/push/PR access,
and an authenticated ACP agent. The default is `codex-acp`; if it is missing,
`bib` uses `npx --yes @agentclientprotocol/codex-acp` (requires Node.js and may
download the adapter). Explicit agent commands are used exactly as supplied.

No configuration file is needed. The bot discovers `origin` (or the only remote)
and the repository's default branch. It creates a managed clone and a separate
Git worktree for each issue. Your source checkout and uncommitted edits are left
alone. Starting it authorizes unattended edits, command execution, commits,
branch pushes, claim/status comments and PR creation for the configured repository.

```sh
bib /path/to/repo
bib https://github.com/OWNER/REPO.git
bib --label bug --label ready
bib once --issue 123
bib --model YOUR_MODEL_ID --effort low
bib --agent your-acp-agent --agent-arg=--stdio
bib --claim-timeout 15m
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

1. Walk all open, unlocked issues, oldest first, with pagination. No label
   filter is required. Optional repeated `--label` flags require all labels;
   `exclude_labels` can opt out particular labels.
2. Skip issues that already have a linked or cross-referenced PR, including PRs
   created by people or other bots, on other branches or forks. Open, draft,
   merged and closed PRs all count. GitHub's issue links and timeline are checked;
   a PR with no reference to the issue cannot be associated automatically.
3. Post an **“I'm starting work on this issue”** comment with a unique claim and
   expiry time. Other instances skip issues with an active claim and continue
   walking the queue. Recheck competing claims before launching the agent.
4. Create a persistent `issue-bot/<base-branch-hash>/<issue-number>` branch and
   isolated worktree. The agent reads repository instructions, inspects the issue
   and discussion, implements a focused fix, runs checks, and commits locally.
5. Require a completion receipt with the explanation and validation evidence.
   Independently check the branch, clean worktree, starting commit and real diff.
   An optional operator `verify` command must also pass. Recheck claim ownership
   and existing PRs before pushing and again before creating the PR.
6. Push without force and create a draft PR with the explanation, check results
   and `Fixes #N`. Confirm its repository, branches, issue marker and head commit.
   Update the starting comment to **“Done — pull request: URL”**, then continue
   to the next available issue. `--draft=false` creates regular PRs. The bot
   does not merge PRs or close issues itself.

A PR is recorded as **submitted**, not as proof that the issue is solved. Agent
validation evidence is a report; the bot does not infer test correctness from
prose or wait for remote CI. Use repository branch protections and review, and
configure `verify` when you need an independent local gate.

Ambiguous, already-fixed, unsupported or oversized issues can be marked blocked
with an explanation on the issue. They do not prevent attempts on later issues.
Failed or interrupted attempts mark the claim released; detailed diagnostics stay
in local logs. Transient work failures retry after 15 minutes, up to
three attempts; exhausted jobs require `retry`. Failed work and the previous
failure are preserved. Agent setup failures (login, executable, model or effort)
stop the daemon without consuming an issue attempt.

The daemon polls every five minutes when no issue is ready; while work is
available it proceeds sequentially. Each attempt has a two-hour budget. Saved
pending jobs are checked for an existing PR before further agent work, including
at the attempt limit or after the issue closes. A lost PR-creation response is
reconciled by the persistent branch and issue marker. A closed, unmerged bot PR
blocks the job for inspection. Work is not automatically deleted or reset.

## Coordination between instances

The claim timeout defaults to **15 minutes**, configurable with
`--claim-timeout 30m` or `"claim_timeout": "30m"` (minimum 30 seconds).
While working, the bot renews the same comment every third of that duration.
The per-attempt `timeout` is separate and still defaults to two hours. A crashed
instance stops renewing; another instance can take over after its claim expires.
A renewal error cancels the active agent, and publication requires a fresh
ownership check. Keep participating machines' clocks synchronized.

Claims cover the repository and issue number across base branches and machines.
Each installation keeps its own managed workspace; local locks still protect
against two processes sharing the same checkout/state. Simultaneous claimants
wait two seconds after posting and elect the lowest active GitHub comment ID;
losers release their comment and move on. GitHub comments provide **advisory
coordination, not an atomic distributed lock**: delayed visibility can briefly
allow duplicate local effort. Rechecking ownership and PR links before publication
reduces duplicate PRs, but GitHub offers no atomic comment-claim/PR transaction.
All participating instances need this version's claim protocol; older versions
do not honor it.

Starting comments and completion updates are idempotent across retries: the
random token is saved before the initial POST, and a failed completion update
is retried without running the agent or creating a PR again. The same comment
is updated throughout the attempt so heartbeats do not produce comment spam.
If an attempt cannot continue, its comment is marked released. An uneditable or
deleted comment stops renewal; a previously saved completion can be posted again
if its comment was deleted. Issues/PR read access, issue-comment write access,
and the existing push/PR-creation rights are required.

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
  "claim_timeout": "15m",
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

## Publishing npm packages

Tag releases build four native archives with checksums and commit metadata.
After the GitHub release finishes, run the **Publish packages** workflow from
that same tag, supplying the tag as input. The default `publish=false` builds
and tests the npm tarballs and saves them as a workflow artifact. Set
`publish=true` to publish the four native packages followed by the launcher.
Retries verify existing versions against the staged bytes and skip identical
uploads; conflicting versions stop publication.

The workflow uses the `packages-publish` environment. Configure npm trusted
publishing for each of the five `@brokkai/issue-bot*` packages with repository
`BrokkAi/issue-bot`, workflow `publish-packages.yml`, and environment
`packages-publish`. An environment secret named `NPM_TOKEN` can be used for
bootstrap publication. Only npm is packaged; uv support is pending.

For a local installer check from a clean commit (requires Go, Node.js 24 and npm):

```sh
node --test --test-isolation=none npm/bib.test.cjs
python3 -m unittest discover -s scripts -p '*_test.py'
python3 scripts/smoke_installers.py
```

The smoke check builds native archives and all five npm tarballs, then installs
and launches the local platform package offline with lifecycle scripts disabled.

## Development

```sh
make check build
./bin/bib --help
```

Tests use temporary Git remotes and simulated GitHub responses. They cover
walking multiple issues, existing human/fork PR detection, competing claims,
claim expiry and renewal failure, idempotent starting/completion comments,
PR reconciliation after a lost response, blocked-job
fairness, retries preserving edits, setup failures, independent verification,
configuration, discovery and locks. They do not run a paid agent or create PRs
in live repositories. The shared ACP library and release-bot additionally test
protocol and subprocess interoperability, model/effort selection and cancellation.

Protocol/API references: [ACP v1](https://agentclientprotocol.com/protocol/v1/overview),
[GitHub issues](https://docs.github.com/en/rest/issues/issues),
[GitHub pull requests](https://docs.github.com/en/rest/pulls/pulls).

Licensed under [Apache-2.0](LICENSE). Repository discovery and CLI/runner patterns
originate in BrokkAi/release-bot; common ACP code is maintained in BrokkAi/acp-go.
