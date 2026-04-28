package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
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

type ReviewMode string

const (
	ModeInitial  ReviewMode = "initial"
	ModeReReview ReviewMode = "re-review"
	ModeQuick    ReviewMode = "quick"
	ModeFinal    ReviewMode = "final"
)

type ReviewRequest struct {
	Diff            string
	PRURL           string
	Questions       string
	Mode            ReviewMode
	SelfReview      bool
	JiraTicket      string
	JiraContext     string
	PreviousReviews string
}

type claudeResponse struct {
	Result        string  `json:"result"`
	TotalCostUSD  float64 `json:"total_cost_usd"`
	DurationMS    int64   `json:"duration_ms"`
	DurationAPIMS int64   `json:"duration_api_ms"`
	NumTurns      int     `json:"num_turns"`
	IsError       bool    `json:"is_error"`
	Usage         struct {
		InputTokens              int64 `json:"input_tokens"`
		OutputTokens             int64 `json:"output_tokens"`
		CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
		CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	} `json:"usage"`
}

type UsageStats struct {
	mu                sync.Mutex
	TotalCostUSD      float64
	TotalDurationMS   int64
	TotalInputTokens  int64
	TotalOutputTokens int64
	TotalCacheRead    int64
	AgentCalls        int
}

func (u *UsageStats) Add(resp claudeResponse) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.TotalCostUSD += resp.TotalCostUSD
	u.TotalDurationMS += resp.DurationAPIMS
	u.TotalInputTokens += resp.Usage.InputTokens
	u.TotalOutputTokens += resp.Usage.OutputTokens
	u.TotalCacheRead += resp.Usage.CacheReadInputTokens
	u.AgentCalls++
}

func (u *UsageStats) String() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return fmt.Sprintf("%d calls | $%.4f | %s in + %s out tokens | %ds API time",
		u.AgentCalls, u.TotalCostUSD,
		formatTokens(u.TotalInputTokens), formatTokens(u.TotalOutputTokens),
		u.TotalDurationMS/1000)
}

func formatTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

type perspective struct {
	name   string
	prompt string
}

var (
	ghPRPattern       = regexp.MustCompile(`<?https://github\.com/([^/>\s]+)/([^/>\s]+)/pull/(\d+)[^>\s]*>?`)
	jiraTicketPattern = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	modePattern       = regexp.MustCompile(`--(initial|re-review|quick|final)\b`)
	selfPattern       = regexp.MustCompile(`--self\b`)
)

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

func parseMode(text string) ReviewMode {
	if m := modePattern.FindStringSubmatch(text); m != nil {
		return ReviewMode(m[1])
	}
	return ModeInitial
}

func parseJiraTicket(text string) string {
	cleaned := ghPRPattern.ReplaceAllString(text, "")
	cleaned = modePattern.ReplaceAllString(cleaned, "")
	cleaned = selfPattern.ReplaceAllString(cleaned, "")
	if m := jiraTicketPattern.FindString(cleaned); m != "" {
		return m
	}
	return ""
}

func handlePR(api *slack.Client, ev *slackevents.MessageEvent, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions string) {
	mode := parseMode(ev.Text)
	selfReview := selfPattern.MatchString(ev.Text)
	jiraTicket := parseJiraTicket(ev.Text)

	_ = api.AddReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))

	modeDesc := string(mode)
	if selfReview {
		modeDesc += " (self)"
	}
	dmUser(api, notifyUserID, fmt.Sprintf("Starting %s review of <%s>...", modeDesc, prURL))

	dmUser(api, notifyUserID, fmt.Sprintf("Fetching diff for <%s>...", prURL))
	diff, err := fetchDiff(owner, repo, prNum)
	if err != nil {
		if selfReview {
			dmUser(api, notifyUserID, fmt.Sprintf("Failed to review <%s>: %v", prURL, err))
		} else {
			postError(api, ev, prURL, channelID, notifyUserID, err)
		}
		return
	}

	if jiraTicket == "" {
		if title := fetchPRTitle(owner, repo, prNum); title != "" {
			if m := jiraTicketPattern.FindString(title); m != "" {
				jiraTicket = m
			}
		}
	}

	var jiraContext string
	if jiraTicket != "" {
		jiraContext = fetchJiraContext(jiraTicket)
		if jiraContext != "" {
			dmUser(api, notifyUserID, fmt.Sprintf("Including Jira context for %s...", jiraTicket))
		}
	}

	var previousReviews string
	if mode == ModeReReview {
		previousReviews = fetchPreviousReviews(owner, repo, prNum)
	}

	agentCount := 4
	if mode == ModeQuick {
		agentCount = 1
	}
	dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Launching %d agent(s) in %s mode...", len(diff), agentCount, mode))

	req := ReviewRequest{
		Diff:            diff,
		PRURL:           prURL,
		Questions:       reviewQuestions,
		Mode:            mode,
		SelfReview:      selfReview,
		JiraTicket:      jiraTicket,
		JiraContext:     jiraContext,
		PreviousReviews: previousReviews,
	}

	review, stats, err := reviewWithClaude(api, notifyUserID, req)
	if err != nil {
		if selfReview {
			dmUser(api, notifyUserID, fmt.Sprintf("Failed to review <%s>: %v", prURL, err))
		} else {
			postError(api, ev, prURL, channelID, notifyUserID, err)
		}
		return
	}

	modeLabel := capitalize(string(mode))

	if selfReview {
		dmUser(api, notifyUserID, fmt.Sprintf("*%s review for <%s>:*\n\n%s", modeLabel, prURL, review))
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> sent via DM only.\nUsage: %s", modeLabel, prURL, stats))
		return
	}

	dmUser(api, notifyUserID, fmt.Sprintf("Posting review to GitHub PR <%s>...", prURL))
	if err := postGitHubComment(owner, repo, prNum, review); err != nil {
		log.Printf("failed to post GitHub comment for %s: %v", prURL, err)
		dmUser(api, notifyUserID, fmt.Sprintf("Failed to post review on <%s>: %v", prURL, err))
	}

	_, _, err = api.PostMessage(
		channelID,
		slack.MsgOptionText(fmt.Sprintf("*%s review for <%s>:*\n\n%s", modeLabel, prURL, review), false),
		slack.MsgOptionTS(ev.TimeStamp),
	)
	if err != nil {
		log.Printf("failed to post review in channel for %s: %v", prURL, err)
	}

	_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))

	dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> posted on GitHub and in <#%s>.\nUsage: %s", modeLabel, prURL, channelID, stats))
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

func fetchPRTitle(owner, repo, prNum string) string {
	cmd := exec.Command("gh", "pr", "view", prNum,
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--json", "title", "--jq", ".title")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fetchPreviousReviews(owner, repo, prNum string) string {
	cmd := exec.Command("gh", "pr", "view", prNum,
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--json", "comments", "--jq", ".comments[].body")
	out, err := cmd.Output()
	if err != nil {
		log.Printf("failed to fetch previous reviews for %s/%s#%s: %v", owner, repo, prNum, err)
		return ""
	}
	result := strings.TrimSpace(string(out))
	if len(result) > 8000 {
		result = result[:8000] + "\n[truncated]"
	}
	return result
}

func fetchJiraContext(ticketKey string) string {
	baseURL := os.Getenv("JIRA_BASE_URL")
	email := os.Getenv("JIRA_EMAIL")
	token := os.Getenv("JIRA_API_TOKEN")

	if baseURL == "" || email == "" || token == "" {
		return ""
	}

	apiURL := fmt.Sprintf("%s/rest/api/2/issue/%s?fields=summary,description",
		strings.TrimRight(baseURL, "/"), ticketKey)

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		log.Printf("jira: failed to create request: %v", err)
		return ""
	}
	req.SetBasicAuth(email, token)
	req.Header.Set("Accept", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("jira: request failed for %s: %v", ticketKey, err)
		return ""
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		body, _ := io.ReadAll(resp.Body)
		log.Printf("jira: status %d for %s: %s", resp.StatusCode, ticketKey, string(body))
		return ""
	}

	var issue struct {
		Key    string `json:"key"`
		Fields struct {
			Summary     string `json:"summary"`
			Description string `json:"description"`
		} `json:"fields"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&issue); err != nil {
		log.Printf("jira: decode failed for %s: %v", ticketKey, err)
		return ""
	}

	desc := issue.Fields.Description
	if len(desc) > 2000 {
		desc = desc[:2000] + "\n[truncated]"
	}

	return fmt.Sprintf("## Jira Ticket: %s\n**Summary:** %s\n**Description:**\n%s\n\nEvaluate whether this PR adequately addresses the ticket requirements.",
		issue.Key, issue.Fields.Summary, desc)
}

func reviewWithClaude(api *slack.Client, notifyUserID string, req ReviewRequest) (string, *UsageStats, error) {
	stats := &UsageStats{}

	var extraContext strings.Builder
	if req.JiraContext != "" {
		extraContext.WriteString("\n\n" + req.JiraContext + "\n")
	}
	if req.PreviousReviews != "" {
		extraContext.WriteString(fmt.Sprintf("\n\n## Previous Review Comments\nThis PR was reviewed before. Consider whether previous feedback was addressed:\n\n%s\n", req.PreviousReviews))
	}
	contextBlock := extraContext.String()
	questionsStr := questionsBlock(req.Questions)

	if req.Mode == ModeQuick {
		result, err := runQuickReview(req.PRURL, req.Diff, contextBlock, questionsStr, stats)
		return result, stats, err
	}

	perspectives := buildPerspectives(req.PRURL, req.Diff, req.Mode, contextBlock, questionsStr)

	reviews := make([]string, len(perspectives))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for i, p := range perspectives {
		wg.Add(1)
		go func(idx int, name, prompt string) {
			defer wg.Done()
			log.Printf("agent %s: starting %s review for %s", name, req.Mode, req.PRURL)
			text, resp, err := runClaude(prompt)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("agent %s failed: %w", name, err)
				}
				mu.Unlock()
				return
			}
			stats.Add(resp)
			mu.Lock()
			reviews[idx] = fmt.Sprintf("## %s Review\n\n%s", strings.ToUpper(name), text)
			mu.Unlock()
			log.Printf("agent %s: done for %s ($%.4f)", name, req.PRURL, resp.TotalCostUSD)
		}(i, p.name, p.prompt)
	}
	wg.Wait()

	if firstErr != nil {
		return "", stats, firstErr
	}

	dmUser(api, notifyUserID, fmt.Sprintf("All %d agents done. Running validator...", len(perspectives)))
	allReviews := strings.Join(reviews, "\n\n---\n\n")

	log.Printf("validator: starting for %s", req.PRURL)
	validated, valResp, err := runClaude(fmt.Sprintf(`You are a review validator. You have %d independent code reviews of a PR and the original diff.

Your job:
1. Check each review for accuracy — are the claims correct given the actual diff?
2. Flag any incorrect or misleading feedback
3. Note if reviewers missed anything important
4. Check if any questions raised by reviewers can be answered from the diff itself — if so, answer them

Be concise. Output a validation report.

## Original Diff
`+"```diff\n%s\n```"+`

## Reviews to Validate
%s`, len(perspectives), req.Diff, allReviews))
	if err != nil {
		return "", stats, fmt.Errorf("validator failed: %w", err)
	}
	stats.Add(valResp)
	log.Printf("validator: done for %s ($%.4f)", req.PRURL, valResp.TotalCostUSD)

	dmUser(api, notifyUserID, "Validator done. Merging reviews...")
	log.Printf("merger: starting for %s", req.PRURL)
	merged, mergeResp, err := runMerger(allReviews, validated, req.Mode)
	if err != nil {
		return "", stats, err
	}
	stats.Add(mergeResp)
	log.Printf("merger: done for %s ($%.4f)", req.PRURL, mergeResp.TotalCostUSD)

	return merged, stats, nil
}

func buildPerspectives(prURL, diff string, mode ReviewMode, contextBlock, questionsStr string) []perspective {
	var modePreamble string
	switch mode {
	case ModeReReview:
		modePreamble = `NOTE: This is a RE-REVIEW. This PR has been reviewed before by an automated system. Focus on:
- Whether previously identified issues have been addressed
- Any new issues introduced since the last review
- Remaining concerns that still need attention
Do not repeat feedback that has clearly been addressed.

`
	case ModeFinal:
		modePreamble = `NOTE: This is a FINAL REVIEW before merge. Err on the side of approval:
- Only flag truly critical/blocking issues (bugs, security, data loss)
- Mention nice-to-haves and nit picks as OPTIONAL/non-blocking
- If the code is generally sound and functional, recommend approval

`
	}

	return []perspective{
		{
			name: "correctness",
			prompt: fmt.Sprintf(`%sYou are a code review agent focused on CORRECTNESS and SECURITY.
Review this pull request: %s
%s
Focus on:
- Bugs, logic errors, edge cases
- Security vulnerabilities (injection, auth issues, data leaks)
- Race conditions, error handling gaps
- API contract violations

Be specific. Reference exact lines. No fluff.

%s

`+"```diff\n%s\n```", modePreamble, prURL, contextBlock, questionsStr, diff),
		},
		{
			name: "design",
			prompt: fmt.Sprintf(`%sYou are a code review agent focused on DESIGN and MAINTAINABILITY.
Review this pull request: %s
%s
Focus on:
- Architecture and design patterns
- Code organization, naming, readability
- Unnecessary complexity or premature abstraction
- Missing tests or test quality
- Performance implications

Be specific. Reference exact lines. No fluff.

%s

`+"```diff\n%s\n```", modePreamble, prURL, contextBlock, questionsStr, diff),
		},
		{
			name: "pragmatic",
			prompt: fmt.Sprintf(`%sYou are a pragmatic senior engineer reviewing this PR.
Review this pull request: %s
%s
Focus on:
- Does this actually solve the problem it claims to?
- What could break in production?
- What would you want changed before approving?
- Are there simpler approaches?

Be direct and opinionated. Skip obvious things that are fine.

%s

`+"```diff\n%s\n```", modePreamble, prURL, contextBlock, questionsStr, diff),
		},
		{
			name: "go-expert",
			prompt: fmt.Sprintf(`%sYou are an elite Go code reviewer with deep expertise in Go 1.26, its standard library, and production-grade Go development. You review with the rigor of a senior staff engineer at a top-tier infrastructure company.

Review this pull request: %s
%s
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

`+"```diff\n%s\n```", modePreamble, prURL, contextBlock, questionsStr, diff),
		},
	}
}

func runQuickReview(prURL, diff, contextBlock, questionsStr string, stats *UsageStats) (string, error) {
	prompt := fmt.Sprintf(`You are an expert Go code reviewer doing a QUICK REVIEW. Be concise and focused.

Review this pull request: %s
%s
Prioritize:
1. Critical bugs, security issues, data loss risks
2. Correctness problems and race conditions
3. Obvious design issues

Skip: style nits, minor naming suggestions, test coverage gaps for non-critical paths.

Output format:
- **Summary** — one sentence on what this PR does
- **Issues** (if any) — what's wrong and how to fix it
- **Verdict** — Approve / Request Changes

Keep it short. If the code is sound, say so and approve.

%s

`+"```diff\n%s\n```", prURL, contextBlock, questionsStr, diff)

	log.Printf("quick-review: starting for %s", prURL)
	text, resp, err := runClaude(prompt)
	if err != nil {
		return "", fmt.Errorf("quick review failed: %w", err)
	}
	stats.Add(resp)
	log.Printf("quick-review: done for %s ($%.4f)", prURL, resp.TotalCostUSD)
	return text, nil
}

func runMerger(allReviews, validated string, mode ReviewMode) (string, claudeResponse, error) {
	var modeRules string
	switch mode {
	case ModeFinal:
		modeRules = `
IMPORTANT — FINAL REVIEW RULES:
- The bar for "Request Changes" is HIGH — only for genuine bugs, security issues, or data loss risks
- If there are no critical/blocking issues, the verdict MUST be "Approve"
- All non-critical feedback should be marked as OPTIONAL and NON-BLOCKING
- Frame suggestions as "Consider for a future PR" rather than required changes
`
	case ModeReReview:
		modeRules = `
RE-REVIEW RULES:
- Focus on what changed since the previous review
- Briefly acknowledge resolved issues
- Emphasize remaining or newly introduced concerns
- If all previous critical issues are resolved and no new ones appeared, lean toward approval
`
	}

	text, resp, err := runClaude(fmt.Sprintf(`You are a review synthesizer. You have 4 independent code reviews and a validation report.

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
%s
## Reviews
%s

## Validation Report
%s`, modeRules, allReviews, validated))
	if err != nil {
		return "", claudeResponse{}, fmt.Errorf("merger failed: %w", err)
	}
	return text, resp, nil
}

func questionsBlock(questions string) string {
	if questions == "" {
		return ""
	}
	return fmt.Sprintf("Also specifically answer these questions:\n%s", questions)
}

func runClaude(prompt string) (string, claudeResponse, error) {
	cmd := exec.Command("claude", "-p", prompt, "--output-format", "json")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", claudeResponse{}, fmt.Errorf("claude CLI: %s", string(exitErr.Stderr))
		}
		return "", claudeResponse{}, err
	}

	var resp claudeResponse
	if err := json.Unmarshal(out, &resp); err != nil {
		return strings.TrimSpace(string(out)), claudeResponse{}, nil
	}
	if resp.IsError {
		return "", claudeResponse{}, fmt.Errorf("claude returned error: %s", resp.Result)
	}

	return strings.TrimSpace(resp.Result), resp, nil
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

func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
