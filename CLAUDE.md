# CLAUDE.md

## What this is

Slack bot that watches a channel for GitHub PR links, runs parallel Claude-powered review agents, and posts synthesized reviews back to GitHub and Slack. Single Go binary, no web server — uses Slack Socket Mode.

## Commands

```sh
go build -o pr-review-bot .    # build
go test ./...                  # run tests
task deploy                    # git pull + docker compose up --build
task redeploy                  # docker compose up --build (no pull)
task kill                      # docker compose down
task logs                      # docker compose logs -f
task status                    # container status + /metrics
task post -- <pr-url> [flags]  # post review to running container
task deploy-legacy             # old tmux-based deploy
```

## Architecture

Single-file Go app (`main.go`) + agent prompt templates (`agents/*.md`).

**Flow:** Slack event → parse flags/mode → fetch diff via bare git clone cache → launch N agents in parallel (each calls `claude` CLI as subprocess) → validator checks agent accuracy → scorer rates quality → merger synthesizes final review → post to GitHub + Slack.

**Key types:**
- `ReviewRequest` — all inputs for a review (diff, mode, flags, jira context, spec, etc.)
- `agentFile` — loaded from `agents/*.md`, supports Go template variables and YAML frontmatter (`flag:` for gated agents)
- `promptData` — template data passed to agents: `ModePreamble`, `PRURL`, `ContextBlock`, `QuestionsStr`, `Diff`
- `RepoCache` — bare git clone cache at `~/.pr-review-cache/` for fast diff generation
- `SessionStore` — persists Claude session IDs per PR URL for re-review continuity

**Agents** (all in `agents/`):
- `correctness.md` — bugs, security, race conditions
- `design.md` — architecture, complexity, naming
- `go-expert.md` — idiomatic Go, highest authority in merger
- `pragmatic.md` — does it solve the problem, what breaks in prod
- `necessity.md` — scope auditor, gated behind `--bare-necessities` flag

**Review pipeline stages:** agents (parallel) → validator → scorer → merger

**Review modes:** `--initial` (default, 4 agents), `--quick` (1 agent), `--re-review` (delta focus), `--final` (high approval bar), `--self` (DM only)

## Environment

- Go 1.25+, uses `go-task` (`task`) for build/deploy
- Requires `claude` CLI and `gh` CLI on PATH
- Config via `.env` (see `.env.example`): Slack tokens, channel ID, notify user, optional Jira/model config
- Runs on a coder box in tmux, or as macOS launchd service

## Conventions

- All application code in `main.go` + `main_test.go` (single package `main`)
- Agent prompts are Go templates in `agents/*.md` — use `{{.PRURL}}`, `{{.Diff}}`, `{{.ModePreamble}}`, `{{.ContextBlock}}`, `{{.QuestionsStr}}`
- Gated agents use YAML frontmatter: `flag: <flag-name>` — only included when user passes `--<flag-name>`
- Tests use `mockSlack` implementing `SlackAPI` interface — no real Slack calls in tests
- Table-driven tests preferred
- No external test dependencies — stdlib `testing` only
