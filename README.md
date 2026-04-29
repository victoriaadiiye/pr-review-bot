# PR Review Bot

Multi-agent code review bot that watches a Slack channel for GitHub PR links, runs parallel Claude-powered review agents, and posts synthesized reviews back to GitHub and Slack. Includes a **Quality Score** (0-100) that breaks down across 6 dimensions and tracks improvement across re-reviews.

## How it works

1. Post a GitHub PR link in the watched Slack channel
2. Bot reacts with :eyes: and launches up to 4 specialized review agents in parallel:
   - **Correctness** -- bugs, security, race conditions, error handling
   - **Design** -- architecture, complexity, naming, test quality
   - **Pragmatic** -- does it solve the problem, what breaks in prod, simpler approaches
   - **Go Expert** -- idiomatic Go, Go 1.26 features, project-specific patterns (highest authority)
3. A **scorer** evaluates code quality across 6 dimensions (runs in parallel with agents)
4. A **validator** checks all 4 reviews against the actual diff for accuracy
5. A **merger** synthesizes everything into one cohesive review with verdict
6. Review posted as a GitHub PR comment and Slack thread reply with quality score header
7. Bot reacts with :white_check_mark: and DMs you usage stats + score

## Review modes

Append flags after the PR link to change behavior:

```
https://github.com/org/repo/pull/42                    # default (initial)
https://github.com/org/repo/pull/42 --quick            # single agent, concise
https://github.com/org/repo/pull/42 --re-review        # focus on previously flagged issues
https://github.com/org/repo/pull/42 --final            # err on approval, nits are optional
https://github.com/org/repo/pull/42 --self              # DM only, no GitHub/channel post
https://github.com/org/repo/pull/42 --self --quick     # combine any mode with --self
https://github.com/org/repo/pull/42 --spec docs/SPEC.md  # check drift from spec (repo-relative)
https://github.com/org/repo/pull/42 --spec /abs/path.md  # check drift from spec (local file)
```

| Mode | Agents | Validator | Merger | Behavior |
|------|--------|-----------|--------|----------|
| `--initial` (default) | 4 | yes | yes | Full review |
| `--re-review` | 4 | yes | yes | Fetches previous PR comments. Agents focus on whether prior feedback was addressed. Merger leans toward approval if resolved |
| `--quick` | 1 | no | no | Go-expert only. Summary + issues + verdict. For trivial changes |
| `--final` | 4 | yes | yes | High bar for "Request Changes" -- only blocks on bugs/security/data loss. Everything else marked optional |
| `--self` | (per mode) | (per mode) | (per mode) | Review sent via DM only. No GitHub comment, no channel post. Combine with any mode above |

## Jira integration

The bot auto-detects Jira ticket keys (e.g. `PROJ-123`) from:
1. The Slack message text
2. The PR title (fallback)

If found and Jira env vars are configured, it fetches the ticket summary and description and includes them in every agent prompt so reviewers can evaluate whether the PR addresses the ticket requirements.

```
https://github.com/org/repo/pull/42 PROJ-123           # explicit ticket
https://github.com/org/repo/pull/42 --quick PROJ-123   # works with any mode
```

No Jira env vars? Feature silently skips. No ticket in message or PR title? Same.

## Spec compliance

Use `--spec <path>` to check the PR against a requirements/spec document. The bot evaluates whether the diff accurately implements the spec and flags drift — missing features, extra unspecified behavior, or contradictions.

```
https://github.com/org/repo/pull/42 --spec docs/REQUIREMENTS.md
https://github.com/org/repo/pull/42 --spec internal/auth/SPEC.md --final
https://github.com/org/repo/pull/42 --spec /Users/me/specs/auth-spec.md
```

**Path resolution:**
- Starts with `/`, `~`, or `.` — read from local filesystem
- Otherwise — fetched from the PR's base branch in the same GitHub repo via `gh api`

When a spec is provided:
- All review agents see the spec and evaluate compliance
- The scorer adds a **Spec Compliance** dimension (weighted 20%, other dimensions scaled down proportionally)
- The merger includes a dedicated **Spec Compliance** section in the final review
- Score header shows the spec compliance row

If the spec file can't be read, the bot warns via DM and continues the review without it.

## Quality Score

Every review includes a quality score header:

```
## Quality Score: 78/100

| Dimension | Score |
|---|---|
| Correctness | 8/10 |
| Security | 9/10 |
| Design | 7/10 |
| Go Quality | 8/10 |
| Testing | 6/10 |
| Production Readiness | 8/10 |
```

**Dimensions** (weighted for overall score):
- **Correctness** (25%) -- bugs, logic errors, edge cases, error handling
- **Security** (20%) -- vulnerabilities, data leaks, auth, injection risks
- **Design** (15%) -- architecture, complexity, naming, readability
- **Go Quality** (15%) -- idiomatic Go, stdlib usage, concurrency, error wrapping
- **Testing** (15%) -- test presence, quality, edge case coverage
- **Production Readiness** (10%) -- logging, monitoring, graceful degradation
- **Spec Compliance** (20%, only with `--spec`) -- requirement coverage, drift, unspecified behavior. When present, other weights scale down proportionally to make room.

On `--re-review`, the score shows a delta from the previous review:

```
## Quality Score: 85/100 (↑ +12)
```

The scorer runs as a parallel agent alongside the review agents, adding minimal latency.

## Usage tracking

Every review DM includes a usage summary:

```
7 calls | $0.1823 | 52.1k in + 9.4k out tokens | 38s API time
```

Tracks cost, token counts, and API time across all agent calls (reviewers + scorer + validator + merger).

## Setup

### Prerequisites

- Go 1.22+
- [Claude CLI](https://docs.anthropic.com/en/docs/claude-code) installed and authenticated
- [GitHub CLI](https://cli.github.com/) (`gh`) installed and authenticated
- A Slack app with Socket Mode enabled

### 1. Create Slack app

Use the included manifest to create your app:

```
slack-manifest.yaml
```

Required bot scopes: `channels:history`, `channels:read`, `groups:history`, `groups:read`, `chat:write`, `im:write`, `reactions:write`

Required event subscriptions: `message.channels`, `message.groups`

Enable **Socket Mode** in your app settings and generate an app-level token (`xapp-*`).

### 2. Configure

```sh
cp .env.example .env
```

```sh
# Required
SLACK_BOT_TOKEN=xoxb-...          # Bot User OAuth Token
SLACK_APP_TOKEN=xapp-...          # App-Level Token (Socket Mode)
WATCHED_CHANNEL_ID=C0123456789    # Channel to monitor for PR links
NOTIFY_USER_ID=U0123456789       # User who receives DM status updates

# Optional
REVIEW_QUESTIONS="Does this follow our error handling patterns?"

# Optional: Jira
JIRA_BASE_URL=https://yourco.atlassian.net
JIRA_EMAIL=you@company.com
JIRA_API_TOKEN=your-api-token
```

### 3. Build and run

```sh
go build -o pr-review-bot .
./pr-review-bot
```

### 4. Deploy on coder box

Requires [Task](https://taskfile.dev/) (`go install github.com/go-task/task/v3/cmd/task@latest`).

```sh
task deploy    # git pull + rebuild + restart tmux session
task build     # build only
task restart   # restart the bot in tmux
task logs      # tail bot.log
task status    # show last 20 lines from tmux pane
```

### 5. Run as macOS service (optional)

Edit `com.vuifhaolain.pr-review-bot.plist` to match your paths, then:

```sh
cp com.vuifhaolain.pr-review-bot.plist ~/Library/LaunchAgents/
launchctl load ~/Library/LaunchAgents/com.vuifhaolain.pr-review-bot.plist
```

Logs write to `bot.log` in the project directory.

```sh
# Stop
launchctl unload ~/Library/LaunchAgents/com.vuifhaolain.pr-review-bot.plist

# Restart
launchctl unload ~/Library/LaunchAgents/com.vuifhaolain.pr-review-bot.plist
launchctl load ~/Library/LaunchAgents/com.vuifhaolain.pr-review-bot.plist
```

## Architecture

```
Slack message (PR link + flags)
        |
        v
  parseMode() + parseJiraTicket() + parseSpecPath()
        |
        v
  fetchDiff (gh pr diff)
  fetchPRTitle (gh pr view)          -- Jira ticket auto-detect
  fetchJiraContext (Jira REST API)    -- if ticket found
  readSpecFile / fetchSpecFromRepo   -- if --spec
  fetchPreviousReviews (gh pr view)  -- if --re-review
        |
        v
  +-----------+-----------+-----------+-----------+---------+
  |correctness|  design   | pragmatic | go-expert | scorer  |  (parallel)
  +-----------+-----------+-----------+-----------+---------+
        |           |           |           |           |
        +-----+-----+-----+-----+           |
              |                              |
              v                              |
         validator (checks accuracy)         |
              |                              |
              v                              |
         merger (synthesizes final review)   |
              |                              |
              +----------+-------------------+
                         |
                         v
     Score header + merged review
     GitHub comment + Slack thread + DM with score + usage stats
     (or DM-only if --self)
```

Quick mode skips straight from diff to a single go-expert agent and returns.
