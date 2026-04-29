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
	"strconv"
	"strings"
	"sync"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"github.com/slack-go/slack/socketmode"
)

type SlackAPI interface {
	AddReaction(name string, item slack.ItemRef) error
	RemoveReaction(name string, item slack.ItemRef) error
	PostMessage(channelID string, options ...slack.MsgOption) (string, string, error)
	OpenConversation(params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error)
}

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
	SpecContent     string
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

type ScoreResult struct {
	Correctness         int    `json:"correctness"`
	Security            int    `json:"security"`
	Design              int    `json:"design"`
	GoQuality           int    `json:"go_quality"`
	Testing             int    `json:"testing"`
	ProductionReadiness int    `json:"production_readiness"`
	SpecCompliance      int    `json:"spec_compliance"`
	Overall             int    `json:"overall"`
	Summary             string `json:"summary"`
}

var (
	ghPRPattern          = regexp.MustCompile(`<?https://github\.com/([^/>\s]+)/([^/>\s]+)/pull/(\d+)[^>\s]*>?`)
	jiraTicketPattern    = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	modePattern          = regexp.MustCompile(`--(initial|re-review|quick|final)\b`)
	selfPattern          = regexp.MustCompile(`--self\b`)
	specPattern          = regexp.MustCompile(`--spec\s+(\S+)`)
	previousScorePattern = regexp.MustCompile(`\*\*Quality Score: (\d+)/100\*\*`)
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

func parseSpecPath(text string) string {
	if m := specPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func parseJiraTicket(text string) string {
	cleaned := ghPRPattern.ReplaceAllString(text, "")
	cleaned = modePattern.ReplaceAllString(cleaned, "")
	cleaned = selfPattern.ReplaceAllString(cleaned, "")
	cleaned = specPattern.ReplaceAllString(cleaned, "")
	if m := jiraTicketPattern.FindString(cleaned); m != "" {
		return m
	}
	return ""
}

func handlePR(api SlackAPI, ev *slackevents.MessageEvent, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions string) {
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
			_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
			_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
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

	specPath := parseSpecPath(ev.Text)
	var specContent string
	if specPath != "" {
		var specErr error
		if strings.HasPrefix(specPath, "/") || strings.HasPrefix(specPath, "~") || strings.HasPrefix(specPath, ".") {
			specContent, specErr = readSpecFile(specPath)
		} else {
			specContent, specErr = fetchSpecFromRepo(owner, repo, specPath, prNum)
		}
		if specErr != nil {
			dmUser(api, notifyUserID, fmt.Sprintf("Warning: could not read spec %s: %v (continuing without spec)", specPath, specErr))
		} else {
			dmUser(api, notifyUserID, fmt.Sprintf("Including spec from %s (%d chars)...", specPath, len(specContent)))
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
		SpecContent:     specContent,
	}

	review, score, stats, err := reviewWithClaude(api, notifyUserID, req)
	if err != nil {
		if selfReview {
			_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
			_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
			dmUser(api, notifyUserID, fmt.Sprintf("Failed to review <%s>: %v", prURL, err))
		} else {
			postError(api, ev, prURL, channelID, notifyUserID, err)
		}
		return
	}

	modeLabel := capitalize(string(mode))
	scoreMsg := ""
	if score != nil {
		scoreMsg = fmt.Sprintf(" | Score: %d/100", score.Overall)
	}

	if selfReview {
		dmUser(api, notifyUserID, fmt.Sprintf("*%s review for <%s>:*\n\n%s", modeLabel, prURL, review))
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> sent via DM only.%s\nUsage: %s", modeLabel, prURL, scoreMsg, stats))
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

	dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> posted on GitHub and in <#%s>.%s\nUsage: %s", modeLabel, prURL, channelID, scoreMsg, stats))
}

func fetchDiff(owner, repo, prNum string) (string, error) {
	cmd := exec.Command("gh", "pr", "diff", prNum, "--repo", fmt.Sprintf("%s/%s", owner, repo))
	out, err := cmd.Output()
	if err != nil {
		log.Printf("gh pr diff failed for %s/%s#%s, trying git fallback", owner, repo, prNum)
		return fetchDiffViaGit(owner, repo, prNum)
	}

	diff := string(out)
	const maxChars = 80_000
	if len(diff) > maxChars {
		diff = diff[:maxChars] + "\n\n[diff truncated]"
	}
	return diff, nil
}

func fetchDiffViaGit(owner, repo, prNum string) (string, error) {
	repoSlug := fmt.Sprintf("%s/%s", owner, repo)
	repoURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)

	baseCmd := exec.Command("gh", "pr", "view", prNum, "--repo", repoSlug, "--json", "baseRefName", "--jq", ".baseRefName")
	baseOut, err := baseCmd.Output()
	if err != nil {
		return "", fmt.Errorf("get PR base ref: %w", err)
	}
	baseRef := strings.TrimSpace(string(baseOut))

	tmpDir, err := os.MkdirTemp("", "pr-diff-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	if out, err := exec.Command("git", "init", tmpDir).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git init: %s", string(out))
	}
	if out, err := exec.Command("git", "-C", tmpDir, "remote", "add", "origin", repoURL).CombinedOutput(); err != nil {
		return "", fmt.Errorf("git remote add: %s", string(out))
	}

	log.Printf("fetching base ref %s and PR #%s head via git for %s", baseRef, prNum, repoSlug)
	if out, err := exec.Command("git", "-C", tmpDir, "fetch", "--depth=1", "origin", baseRef+":base").CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetch base: %s", string(out))
	}
	if out, err := exec.Command("git", "-C", tmpDir, "fetch", "--depth=1", "origin", fmt.Sprintf("pull/%s/head:pr", prNum)).CombinedOutput(); err != nil {
		return "", fmt.Errorf("fetch PR head: %s", string(out))
	}

	diffCmd := exec.Command("git", "-C", tmpDir, "diff", "base..pr")
	out, err := diffCmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("git diff: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("git diff: %w", err)
	}

	diff := string(out)
	const maxChars = 80_000
	if len(diff) > maxChars {
		diff = diff[:maxChars] + "\n\n[diff truncated]"
	}
	log.Printf("git fallback: got %d char diff for %s#%s", len(diff), repoSlug, prNum)
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

func readSpecFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read spec file %s: %w", path, err)
	}
	content := string(data)
	const maxChars = 20_000
	if len(content) > maxChars {
		content = content[:maxChars] + "\n[spec truncated]"
	}
	return content, nil
}

func fetchSpecFromRepo(owner, repo, specPath, prNum string) (string, error) {
	repoSlug := fmt.Sprintf("%s/%s", owner, repo)
	baseCmd := exec.Command("gh", "pr", "view", prNum, "--repo", repoSlug,
		"--json", "baseRefName", "--jq", ".baseRefName")
	baseOut, err := baseCmd.Output()
	if err != nil {
		return "", fmt.Errorf("get base ref for spec: %w", err)
	}
	baseRef := strings.TrimSpace(string(baseOut))

	cmd := exec.Command("gh", "api",
		fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, specPath, baseRef),
		"-H", "Accept: application/vnd.github.raw")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("fetch %s from %s@%s: %w", specPath, repoSlug, baseRef, err)
	}

	content := string(out)
	const maxChars = 20_000
	if len(content) > maxChars {
		content = content[:maxChars] + "\n[spec truncated]"
	}
	return content, nil
}

func reviewWithClaude(api SlackAPI, notifyUserID string, req ReviewRequest) (string, *ScoreResult, *UsageStats, error) {
	stats := &UsageStats{}

	var extraContext strings.Builder
	if req.JiraContext != "" {
		extraContext.WriteString("\n\n" + req.JiraContext + "\n")
	}
	if req.PreviousReviews != "" {
		extraContext.WriteString(fmt.Sprintf("\n\n## Previous Review Comments\nThis PR was reviewed before. Consider whether previous feedback was addressed:\n\n%s\n", req.PreviousReviews))
	}
	if req.SpecContent != "" {
		extraContext.WriteString(fmt.Sprintf("\n\n## Specification / Requirements\nThe following spec defines what this PR should implement. Evaluate whether the PR accurately implements the spec and flag any drift from requirements — missing features, extra unspecified behavior, or contradictions:\n\n%s\n", req.SpecContent))
	}
	contextBlock := extraContext.String()
	questionsStr := questionsBlock(req.Questions)

	var score ScoreResult
	var scoreErr error
	var scoreWg sync.WaitGroup
	scoreWg.Add(1)
	go func() {
		defer scoreWg.Done()
		log.Printf("scorer: starting for %s", req.PRURL)
		var resp claudeResponse
		score, resp, scoreErr = runScorer(req.Diff, req.SpecContent)
		if scoreErr != nil {
			log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
		} else {
			stats.Add(resp)
			log.Printf("scorer: done for %s (score: %d/100, $%.4f)", req.PRURL, score.Overall, resp.TotalCostUSD)
		}
	}()

	if req.Mode == ModeQuick {
		result, err := runQuickReview(req.PRURL, req.Diff, contextBlock, questionsStr, stats)
		if err != nil {
			return "", nil, stats, err
		}
		scoreWg.Wait()
		if scoreErr == nil {
			result = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + result
			return result, &score, stats, nil
		}
		return result, nil, stats, nil
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
		return "", nil, stats, firstErr
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
		return "", nil, stats, fmt.Errorf("validator failed: %w", err)
	}
	stats.Add(valResp)
	log.Printf("validator: done for %s ($%.4f)", req.PRURL, valResp.TotalCostUSD)

	dmUser(api, notifyUserID, "Validator done. Merging reviews...")
	log.Printf("merger: starting for %s", req.PRURL)
	merged, mergeResp, err := runMerger(allReviews, validated, req.Mode, req.SpecContent)
	if err != nil {
		return "", nil, stats, err
	}
	stats.Add(mergeResp)
	log.Printf("merger: done for %s ($%.4f)", req.PRURL, mergeResp.TotalCostUSD)

	scoreWg.Wait()
	if scoreErr == nil {
		merged = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + merged
		return merged, &score, stats, nil
	}

	return merged, nil, stats, nil
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

func runScorer(diff string, specContent string) (ScoreResult, claudeResponse, error) {
	specDimension := ""
	specBlock := ""
	specJSON := ""
	if specContent != "" {
		specDimension = "\n- spec_compliance: How accurately and completely the diff implements the spec requirements, without drift or missing items"
		specBlock = fmt.Sprintf("\n## Specification\nEvaluate the diff against this spec:\n\n%s\n\n", specContent)
		specJSON = `,"spec_compliance":N`
	}

	prompt := fmt.Sprintf(`You are a code quality scorer. Evaluate this PR diff and rate the code quality.

Score each dimension 0-10 (10 = excellent, 0 = critical problems):
- correctness: Logic errors, bugs, edge cases, error handling
- security: Vulnerabilities, data leaks, auth issues, injection risks
- design: Architecture, complexity, naming, readability
- go_quality: Idiomatic Go, stdlib usage, concurrency patterns, error wrapping
- testing: Test presence and quality, edge case coverage
- production_readiness: Logging, monitoring, graceful degradation%s

If a dimension has no relevant code in the diff (e.g., no security-sensitive changes), score 8-9 reflecting no risk introduced.
%s
Respond with ONLY this JSON object, no markdown fences, no other text:
{"correctness":N,"security":N,"design":N,"go_quality":N,"testing":N,"production_readiness":N%s,"overall":N,"summary":"one sentence"}

`+"```diff\n%s\n```", specDimension, specBlock, specJSON, diff)

	text, resp, err := runClaude(prompt)
	if err != nil {
		return ScoreResult{}, claudeResponse{}, err
	}

	var score ScoreResult
	raw := strings.TrimSpace(text)
	if i := strings.Index(raw, "{"); i >= 0 {
		if j := strings.LastIndex(raw, "}"); j > i {
			raw = raw[i : j+1]
		}
	}

	if err := json.Unmarshal([]byte(raw), &score); err != nil {
		return ScoreResult{}, resp, fmt.Errorf("scorer JSON parse: %w", err)
	}

	if score.SpecCompliance > 0 {
		score.Overall = (score.Correctness*20 + score.Security*16 + score.Design*12 +
			score.GoQuality*12 + score.Testing*12 + score.ProductionReadiness*8 +
			score.SpecCompliance*20) / 10
	} else {
		score.Overall = (score.Correctness*25 + score.Security*20 + score.Design*15 +
			score.GoQuality*15 + score.Testing*15 + score.ProductionReadiness*10) / 10
	}

	return score, resp, nil
}

func formatScoreHeader(score ScoreResult, previousReviews string) string {
	header := fmt.Sprintf("## Quality Score: %d/100", score.Overall)

	if previousReviews != "" {
		if matches := previousScorePattern.FindAllStringSubmatch(previousReviews, -1); len(matches) > 0 {
			last := matches[len(matches)-1]
			if prev, err := strconv.Atoi(last[1]); err == nil {
				delta := score.Overall - prev
				switch {
				case delta > 0:
					header += fmt.Sprintf(" (↑ +%d)", delta)
				case delta < 0:
					header += fmt.Sprintf(" (↓ %d)", delta)
				default:
					header += " (no change)"
				}
			}
		}
	}

	specRow := ""
	if score.SpecCompliance > 0 {
		specRow = fmt.Sprintf("| Spec Compliance | %d/10 |\n", score.SpecCompliance)
	}

	header += fmt.Sprintf(`

| Dimension | Score |
|---|---|
| Correctness | %d/10 |
| Security | %d/10 |
| Design | %d/10 |
| Go Quality | %d/10 |
| Testing | %d/10 |
| Production Readiness | %d/10 |
%s
> %s`,
		score.Correctness, score.Security, score.Design,
		score.GoQuality, score.Testing, score.ProductionReadiness,
		specRow, score.Summary)

	return header
}

func runMerger(allReviews, validated string, mode ReviewMode, specContent string) (string, claudeResponse, error) {
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

	specSection := ""
	specRule := ""
	specContext := ""
	if specContent != "" {
		specSection = "\n7. **Spec Compliance** — how well the PR implements the spec, deviations, missing requirements"
		specRule = "\n- Include a dedicated Spec Compliance section evaluating requirement coverage, drift, and any unspecified behavior"
		specContext = fmt.Sprintf("\n\n## Specification\n%s", specContent)
	}

	text, resp, err := runClaude(fmt.Sprintf(`You are a review synthesizer. You have 4 independent code reviews and a validation report.

Merge them into ONE cohesive, comprehensive review. Structure:

1. **Summary** — one sentence on what this PR does
2. **Critical Issues** — bugs, security, correctness problems (if any)
3. **Design Concerns** — architecture, complexity, maintainability (if any)
4. **Suggestions** — improvements worth making
5. **What's Good** — brief acknowledgment of things done well (1-2 lines max)
6. **Verdict** — Approve / Request Changes / Needs Discussion%s

Rules:
- The GO-EXPERT review is the most authoritative voice. When reviewers conflict, defer to GO-EXPERT. Its critical issues are always included. Its verdict carries the most weight in the final verdict.
- Deduplicate overlapping feedback
- Drop anything the validator flagged as incorrect
- Incorporate answers to reviewer questions from the validation
- Keep it actionable and specific
- Reference file names and line numbers where relevant%s
%s
## Reviews
%s

## Validation Report
%s%s`, specSection, specRule, modeRules, allReviews, validated, specContext))
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

func dmUser(api SlackAPI, userID, msg string) {
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

func postError(api SlackAPI, ev *slackevents.MessageEvent, prURL, channelID, notifyUserID string, reviewErr error) {
	log.Printf("failed to review %s: %v", prURL, reviewErr)
	_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
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
