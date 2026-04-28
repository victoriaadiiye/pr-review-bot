package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

var ghPRPattern = regexp.MustCompile(`<?https://github\.com/([^/>\s]+)/([^/>\s]+)/pull/(\d+)[^>\s]*>?`)

func main() {
	_ = godotenv.Load()

	botToken := mustEnv("SLACK_BOT_TOKEN")
	appToken := mustEnv("SLACK_APP_TOKEN")
	channelID := mustEnv("WATCHED_CHANNEL_ID")
	notifyUserID := mustEnv("NOTIFY_USER_ID")

	reviewQuestions := os.Getenv("REVIEW_QUESTIONS")

	api := slack.New(botToken, slack.OptionAppLevelToken(appToken))
	client := socketmode.New(api)

	go func() {
		for evt := range client.Events {
			if evt.Type != socketmode.EventTypeEventsAPI {
				continue
			}
			outer, ok := evt.Data.(slackevents.EventsAPIEvent)
			if !ok {
				continue
			}
			client.Ack(*evt.Request)

			if outer.InnerEvent.Type != string(slackevents.Message) {
				continue
			}
			ev, ok := outer.InnerEvent.Data.(*slackevents.MessageEvent)
			if !ok || ev.BotID != "" || ev.SubType != "" {
				continue
			}
			if ev.Channel != channelID {
				continue
			}

			matches := ghPRPattern.FindAllStringSubmatch(ev.Text, -1)
			if len(matches) == 0 {
				continue
			}

			for _, m := range matches {
				owner, repo, prNum := m[1], m[2], m[3]
				prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)

				go handlePR(api, ev, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions)
			}
		}
	}()

	log.Println("PR Review Bot running...")
	if err := client.Run(); err != nil {
		log.Fatal(err)
	}
}

func handlePR(api *slack.Client, ev *slackevents.MessageEvent, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions string) {
	_ = api.AddReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))

	diff, err := fetchDiff(owner, repo, prNum)
	if err != nil {
		postError(api, ev, prURL, channelID, notifyUserID, err)
		return
	}

	review, err := reviewWithClaude(diff, prURL, reviewQuestions)
	if err != nil {
		postError(api, ev, prURL, channelID, notifyUserID, err)
		return
	}

	// Post review to GitHub PR
	if err := postGitHubComment(owner, repo, prNum, review); err != nil {
		log.Printf("failed to post GitHub comment for %s: %v", prURL, err)
		dmUser(api, notifyUserID, fmt.Sprintf("Failed to post review on <%s>: %v", prURL, err))
	}

	// Post review back into private channel (for Workflow 2 to pick up)
	_, _, err = api.PostMessage(
		channelID,
		slack.MsgOptionText(fmt.Sprintf("*Review for <%s>:*\n\n%s", prURL, review), false),
		slack.MsgOptionTS(ev.TimeStamp),
	)
	if err != nil {
		log.Printf("failed to post review in channel for %s: %v", prURL, err)
	}

	_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))

	dmUser(api, notifyUserID, fmt.Sprintf("Reviewed <%s>. Posted on GitHub and in <#%s>.", prURL, channelID))
}

func fetchDiff(owner, repo, prNum string) (string, error) {
	cmd := exec.Command("gh", "pr", "diff", prNum, "--repo", fmt.Sprintf("%s/%s", owner, repo))
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("gh pr diff failed: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("gh pr diff failed: %w", err)
	}

	diff := string(out)
	const maxChars = 80_000
	if len(diff) > maxChars {
		diff = diff[:maxChars] + "\n\n[diff truncated]"
	}
	return diff, nil
}

func reviewWithClaude(diff, prURL, reviewQuestions string) (string, error) {
	perspectives := []struct {
		name   string
		prompt string
	}{
		{
			name: "correctness",
			prompt: fmt.Sprintf(`You are a code review agent focused on CORRECTNESS and SECURITY.
Review this pull request: %s

Focus on:
- Bugs, logic errors, edge cases
- Security vulnerabilities (injection, auth issues, data leaks)
- Race conditions, error handling gaps
- API contract violations

Be specific. Reference exact lines. No fluff.

%s

`+"```diff\n%s\n```", prURL, questionsBlock(reviewQuestions), diff),
		},
		{
			name: "design",
			prompt: fmt.Sprintf(`You are a code review agent focused on DESIGN and MAINTAINABILITY.
Review this pull request: %s

Focus on:
- Architecture and design patterns
- Code organization, naming, readability
- Unnecessary complexity or premature abstraction
- Missing tests or test quality
- Performance implications

Be specific. Reference exact lines. No fluff.

%s

`+"```diff\n%s\n```", prURL, questionsBlock(reviewQuestions), diff),
		},
		{
			name: "pragmatic",
			prompt: fmt.Sprintf(`You are a pragmatic senior engineer reviewing this PR.
Review this pull request: %s

Focus on:
- Does this actually solve the problem it claims to?
- What could break in production?
- What would you want changed before approving?
- Are there simpler approaches?

Be direct and opinionated. Skip obvious things that are fine.

%s

`+"```diff\n%s\n```", prURL, questionsBlock(reviewQuestions), diff),
		},
		{
			name: "go-expert",
			prompt: fmt.Sprintf(`You are an elite Go code reviewer with deep expertise in Go 1.26, its standard library, and production-grade Go development. You review with the rigor of a senior staff engineer at a top-tier infrastructure company.

Review this pull request: %s

## Review Criteria

### Correctness
- Logic errors, off-by-one mistakes, race conditions
- Proper error handling: explicit error returns, no swallowed errors, %%w for wrapping
- No panics outside main()
- Correct use of concurrency primitives (sync.Mutex, channels, context.Context)
- context.Context must be the first parameter in non-handler functions

### Go 1.26 Best Practices
- Use Go 1.26 features: enhanced http.ServeMux with {param} path values, range-over-func iterators
- Prefer standard library over third-party dependencies
- Use slog for structured logging
- Idiomatic Go: receiver names, interface naming (-er suffix), zero-value usefulness

### Style & Formatting
- gofumpt formatting compliance
- Exported types and functions must have doc comments
- Functions should be ≤ ~50 lines; flag functions that are too long
- No variable shadowing (especially err — use named variants like parseErr, decodeErr)
- //nolint:lintname // reason format (double-slash before reason)

### Testing (TDD Compliance)
- Tests exist for new functionality
- Tests are meaningful, not just happy paths
- Table-driven tests where appropriate
- httptest for HTTP handler testing
- No test pollution (parallel tests, proper cleanup)

### Project-Specific Patterns
- Module: github.com/Qumulo/qompass
- Structure: cmd/qompass/, internal/ packages, tests/integration/
- ClickHouse: clickhouse-go v2 is the only allowed external runtime dependency
- LowCardinality for <10K unique values, no Nullable columns in ClickHouse schemas
- *json.RawMessage null gotcha: marshaling null gives nil pointer
- gzip bodies <10 bytes trigger io.ErrUnexpectedEOF

## Output Format

### Critical Issues 🔴
Must-fix: bugs, security, data loss, race conditions.

### Suggestions 🟡
Important improvements: error handling, edge cases, performance.

### Nits 🟢
Minor style, naming, documentation.

### What Looks Good ✅
Well-written code worth reinforcing.

For each finding: file and line, what the issue is, why it matters, concrete fix.

Be specific, not vague. Show exactly what and why. Respect existing codebase patterns — don't suggest rewrites outside PR scope.

%s

`+"```diff\n%s\n```", prURL, questionsBlock(reviewQuestions), diff),
		},
	}

	reviews := make([]string, len(perspectives))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for i, p := range perspectives {
		wg.Add(1)
		go func(idx int, name, prompt string) {
			defer wg.Done()
			log.Printf("agent %s: starting review for %s", name, prURL)
			out, err := runClaude(prompt)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("agent %s failed: %w", name, err)
				}
				mu.Unlock()
				return
			}
			mu.Lock()
			reviews[idx] = fmt.Sprintf("## %s Review\n\n%s", strings.ToUpper(name), out)
			mu.Unlock()
			log.Printf("agent %s: done for %s", name, prURL)
		}(i, p.name, p.prompt)
	}
	wg.Wait()

	if firstErr != nil {
		return "", firstErr
	}

	allReviews := strings.Join(reviews, "\n\n---\n\n")

	log.Printf("validator: starting for %s", prURL)
	validated, err := runClaude(fmt.Sprintf(`You are a review validator. You have 4 independent code reviews of a PR and the original diff.

Your job:
1. Check each review for accuracy — are the claims correct given the actual diff?
2. Flag any incorrect or misleading feedback
3. Note if reviewers missed anything important
4. Check if any questions raised by reviewers can be answered from the diff itself — if so, answer them

Be concise. Output a validation report.

## Original Diff
`+"```diff\n%s\n```"+`

## Reviews to Validate
%s`, diff, allReviews))
	if err != nil {
		return "", fmt.Errorf("validator failed: %w", err)
	}
	log.Printf("validator: done for %s", prURL)

	log.Printf("merger: starting for %s", prURL)
	merged, err := runClaude(fmt.Sprintf(`You are a review synthesizer. You have 4 independent code reviews and a validation report.

Merge them into ONE cohesive, comprehensive review. Structure:

1. **Summary** — one sentence on what this PR does
2. **Critical Issues** — bugs, security, correctness problems (if any)
3. **Design Concerns** — architecture, complexity, maintainability (if any)
4. **Suggestions** — improvements worth making
5. **What's Good** — brief acknowledgment of things done well (1-2 lines max)
6. **Verdict** — Approve / Request Changes / Needs Discussion

Rules:
- The GO-EXPERT review is the most authoritative voice. When reviewers conflict, defer to GO-EXPERT. Its critical issues are always included. Its verdict carries the most weight in the final verdict.
- Deduplicate overlapping feedback
- Drop anything the validator flagged as incorrect
- Incorporate answers to reviewer questions from the validation
- Keep it actionable and specific
- Reference file names and line numbers where relevant

## Reviews
%s

## Validation Report
%s`, allReviews, validated))
	if err != nil {
		return "", fmt.Errorf("merger failed: %w", err)
	}
	log.Printf("merger: done for %s", prURL)

	return merged, nil
}

func questionsBlock(questions string) string {
	if questions == "" {
		return ""
	}
	return fmt.Sprintf("Also specifically answer these questions:\n%s", questions)
}

func runClaude(prompt string) (string, error) {
	cmd := exec.Command("claude", "-p", prompt, "--output-format", "text")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("claude CLI: %s", string(exitErr.Stderr))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func postGitHubComment(owner, repo, prNum, review string) error {
	cmd := exec.Command("gh", "pr", "comment", prNum,
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--body", review,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("gh pr comment failed: %s", string(out))
	}
	return nil
}

func dmUser(api *slack.Client, userID, msg string) {
	_, _, _, err := api.OpenConversation(&slack.OpenConversationParameters{Users: []string{userID}})
	if err != nil {
		log.Printf("failed to open DM with %s: %v", userID, err)
		return
	}
	_, _, err = api.PostMessage(userID, slack.MsgOptionText(msg, false))
	if err != nil {
		log.Printf("failed to DM %s: %v", userID, err)
	}
}

func postError(api *slack.Client, ev *slackevents.MessageEvent, prURL, channelID, notifyUserID string, reviewErr error) {
	log.Printf("failed to review %s: %v", prURL, reviewErr)
	_, _, _ = api.PostMessage(
		channelID,
		slack.MsgOptionText(fmt.Sprintf("Failed to review <%s>: %v", prURL, reviewErr), false),
		slack.MsgOptionTS(ev.TimeStamp),
	)
	dmUser(api, notifyUserID, fmt.Sprintf("Failed to review <%s>: %v", prURL, reviewErr))
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required env var %s not set", key)
	}
	return v
}
