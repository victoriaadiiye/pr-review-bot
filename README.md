# PR Review Bot

Multi-agent code review bot that watches a Slack channel for GitHub PR links, runs parallel Claude-powered review agents, and posts synthesized reviews back to GitHub and Slack.

## How it works

1. Post a GitHub PR link in the watched Slack channel
2. Bot reacts with :eyes: and launches up to 4 specialized review agents in parallel:
   - **Correctness** -- bugs, security, race conditions, error handling
   - **Design** -- architecture, complexity, naming, test quality
   - **Pragmatic** -- does it solve the problem, what breaks in prod, simpler approaches
   - **Go Expert** -- idiomatic Go, Go 1.26 features, project-specific patterns (highest authority)
3. A **validator** checks all 4 reviews against the actual diff for accuracy
4. A **merger** synthesizes everything into one cohesive review with verdict
5. Review posted as a GitHub PR comment and Slack thread reply
6. Bot reacts with :white_check_mark: and DMs you usage stats

## Review modes

Append flags after the PR link to change behavior:

```
https://github.com/org/repo/pull/42                    # default (initial)
https://github.com/org/repo/pull/42 --quick            # single agent, concise
https://github.com/org/repo/pull/42 --re-review        # focus on previously flagged issues
https://github.com/org/repo/pull/42 --final            # err on approval, nits are optional
https://github.com/org/repo/pull/42 --self              # DM only, no GitHub/channel post
https://github.com/org/repo/pull/42 --self --quick     # combine any mode with --self
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

## Usage tracking

Every review DM includes a usage summary:

```
6 calls | $0.1523 | 45.2k in + 8.1k out tokens | 32s API time
```

Tracks cost, token counts, and API time across all agent calls (reviewers + validator + merger).

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

### 4. Run as macOS service (optional)

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
  parseMode() + parseJiraTicket()
        |
        v
  fetchDiff (gh pr diff)
  fetchPRTitle (gh pr view)        -- Jira ticket auto-detect
  fetchJiraContext (Jira REST API)  -- if ticket found
  fetchPreviousReviews (gh pr view) -- if --re-review
        |
        v
  +-----------+-----------+-----------+-----------+
  |correctness|  design   | pragmatic | go-expert |  (parallel)
  +-----------+-----------+-----------+-----------+
        |           |           |           |
        +-----+-----+-----+-----+
              |
              v
         validator (checks accuracy)
              |
              v
         merger (synthesizes final review)
              |
              v
     GitHub comment + Slack thread + DM with usage stats
     (or DM-only if --self)
```

Quick mode skips straight from diff to a single go-expert agent and returns.
