package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
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
	GetConversationHistory(params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error)
	GetConversationReplies(params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error)
}

type ReviewMode string

const (
	ModeInitial  ReviewMode = "initial"
	ModeReReview ReviewMode = "re-review"
	ModeQuick    ReviewMode = "quick"
	ModeFinal    ReviewMode = "final"
)

type ReviewRequest struct {
	Diff               string
	PRURL              string
	Questions          string
	Mode               ReviewMode
	SelfReview         bool
	JiraTicket         string
	JiraContext        string
	PreviousReviews    string
	AcknowledgedIssues string
	SpecContent        string
	SpecPath           string
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
	previousSpecPattern  = regexp.MustCompile(`<!-- spec: (\S+) -->`)
	reviewRequestPattern = regexp.MustCompile(`(?i)\breview\b`)
	ackPattern           = regexp.MustCompile(`(?i)\b(ack(nowledg(ed?|ing))?|won'?t\s*fix|wontfix|intentional|by\s*design|noted|accepted|will\s*(fix|address)\s*later|tracking\s+in|known\s+issue|out\s*of\s*scope|deferred)\b`)

	repoCache       *RepoCache
	anthropicClient anthropic.Client

	activeReviews   = make(map[string]context.CancelFunc)
	activeReviewsMu sync.Mutex
)

func reviewKey(ts, id string) string {
	return ts + "|" + id
}

func trackReview(ts, id string, cancel context.CancelFunc) {
	activeReviewsMu.Lock()
	defer activeReviewsMu.Unlock()
	key := reviewKey(ts, id)
	activeReviews[key] = cancel
	log.Printf("tracking review %s (%d active)", key, len(activeReviews))
}

func untrackReview(ts, id string) {
	activeReviewsMu.Lock()
	defer activeReviewsMu.Unlock()
	key := reviewKey(ts, id)
	delete(activeReviews, key)
	log.Printf("untracked review %s (%d active)", key, len(activeReviews))
}

func isReviewActive(prURL string) bool {
	activeReviewsMu.Lock()
	defer activeReviewsMu.Unlock()
	suffix := "|" + prURL
	for key := range activeReviews {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func cancelReview(ts string) bool {
	activeReviewsMu.Lock()
	defer activeReviewsMu.Unlock()
	prefix := ts + "|"
	cancelled := false
	for key, cancel := range activeReviews {
		if strings.HasPrefix(key, prefix) {
			cancel()
			delete(activeReviews, key)
			cancelled = true
			log.Printf("cancelled review %s", key)
		}
	}
	return cancelled
}

func findPRInThread(api SlackAPI, channelID, threadTS string) (owner, repo, prNum string, fullText string, ok bool) {
	msgs, _, _, err := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: channelID,
		Timestamp: threadTS,
		Limit:     50,
	})
	if err != nil {
		log.Printf("failed to fetch thread %s in %s: %v", threadTS, channelID, err)
		return "", "", "", "", false
	}
	for _, msg := range msgs {
		if m := ghPRPattern.FindStringSubmatch(msg.Text); m != nil {
			return m[1], m[2], m[3], msg.Text, true
		}
	}
	return "", "", "", "", false
}

func handleThreadFollowup(ctx context.Context, api SlackAPI, ev *slackevents.AppMentionEvent, owner, repo, prNum, notifyUserID string) {
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)

	_ = api.AddReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))

	diff, err := fetchDiff(ctx, owner, repo, prNum)
	if err != nil {
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_, _, _ = api.PostMessage(ev.Channel,
			slack.MsgOptionText(fmt.Sprintf("Failed to fetch diff for <%s>: %v", prURL, err), false),
			slack.MsgOptionTS(ev.ThreadTimeStamp))
		return
	}

	msgs, _, _, _ := api.GetConversationReplies(&slack.GetConversationRepliesParameters{
		ChannelID: ev.Channel,
		Timestamp: ev.ThreadTimeStamp,
		Limit:     50,
	})
	var threadContext strings.Builder
	for _, msg := range msgs {
		if msg.Timestamp == ev.TimeStamp {
			continue
		}
		threadContext.WriteString(msg.Text + "\n\n")
	}

	botMentionPattern := regexp.MustCompile(`<@[A-Z0-9]+>`)
	question := strings.TrimSpace(botMentionPattern.ReplaceAllString(ev.Text, ""))

	prompt := fmt.Sprintf(`You are a code review assistant. A reviewer asked a follow-up question about a PR review.

Answer the question concisely based on the diff and thread context. Be specific — reference files and lines.

## Question
%s

## PR Diff
`+"```diff\n%s\n```"+`

## Thread Context
%s`, question, diff, threadContext.String())

	answer, _, err := runClaude(ctx, prompt)
	if err != nil {
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_, _, _ = api.PostMessage(ev.Channel,
			slack.MsgOptionText(fmt.Sprintf("Failed to answer: %v", err), false),
			slack.MsgOptionTS(ev.ThreadTimeStamp))
		return
	}

	_, _, _ = api.PostMessage(ev.Channel,
		slack.MsgOptionText(answer, false),
		slack.MsgOptionTS(ev.ThreadTimeStamp))
	_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	dmUser(api, notifyUserID, fmt.Sprintf("Answered follow-up question in thread for <%s>", prURL))
}

func handleReactionReview(api SlackAPI, rev *slackevents.ReactionAddedEvent, channelID, notifyUserID, reviewQuestions string) {
	resp, err := api.GetConversationHistory(&slack.GetConversationHistoryParameters{
		ChannelID: channelID,
		Latest:    rev.Item.Timestamp,
		Inclusive: true,
		Limit:     1,
	})
	if err != nil || len(resp.Messages) == 0 {
		log.Printf("failed to fetch message for :claude_it: reaction on %s: %v", rev.Item.Timestamp, err)
		return
	}
	msg := resp.Messages[0]

	matches := ghPRPattern.FindAllStringSubmatch(msg.Text, -1)
	if len(matches) == 0 {
		log.Printf("no PR URL found in message %s for :claude_it: reaction", rev.Item.Timestamp)
		return
	}

	ev := &slackevents.MessageEvent{
		Text:      msg.Text,
		Channel:   channelID,
		TimeStamp: rev.Item.Timestamp,
	}

	for _, m := range matches {
		owner, repo, prNum := m[1], m[2], m[3]
		prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)
		if isReviewActive(prURL) {
			log.Printf("skipping duplicate :claude_it: review for %s (already in progress)", prURL)
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		trackReview(ev.TimeStamp, prURL, cancel)
		go handlePR(ctx, api, ev, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions)
	}
}

func main() {
	_ = godotenv.Load()
	repoCache = NewRepoCache()
	anthropicClient = anthropic.NewClient()

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

			switch outer.InnerEvent.Type {
			case string(slackevents.ReactionAdded):
				rev, ok := outer.InnerEvent.Data.(*slackevents.ReactionAddedEvent)
				if !ok {
					continue
				}
				if rev.Reaction == "no_entry_sign" {
					if cancelReview(rev.Item.Timestamp) {
						log.Printf("review cancelled by reaction on %s in %s", rev.Item.Timestamp, rev.Item.Channel)
					}
				}
				if rev.Reaction == "claude_it" {
					go handleReactionReview(api, rev, rev.Item.Channel, notifyUserID, reviewQuestions)
				}

			case string(slackevents.AppMention):
				ev, ok := outer.InnerEvent.Data.(*slackevents.AppMentionEvent)
				if !ok {
					continue
				}

				matches := ghPRPattern.FindAllStringSubmatch(ev.Text, -1)
				isReviewRequest := reviewRequestPattern.MatchString(ev.Text)
				inThread := ev.ThreadTimeStamp != ""

				if len(matches) > 0 && isReviewRequest {
					msgEv := &slackevents.MessageEvent{
						Text:      ev.Text,
						Channel:   ev.Channel,
						TimeStamp: ev.TimeStamp,
					}
					for _, m := range matches {
						owner, repo, prNum := m[1], m[2], m[3]
						prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)
						if isReviewActive(prURL) {
							log.Printf("skipping duplicate @mention review for %s (already in progress)", prURL)
							continue
						}
						ctx, cancel := context.WithCancel(context.Background())
						trackReview(msgEv.TimeStamp, prURL, cancel)
						go handlePR(ctx, api, msgEv, prURL, owner, repo, prNum, ev.Channel, notifyUserID, reviewQuestions)
					}
					continue
				}

				if inThread {
					owner, repo, prNum, parentText, found := findPRInThread(api, ev.Channel, ev.ThreadTimeStamp)
					if !found {
						continue
					}
					prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)
					if isReviewRequest {
						if isReviewActive(prURL) {
							log.Printf("skipping duplicate thread re-review for %s (already in progress)", prURL)
							continue
						}
						text := ev.Text
						if parseMode(text) == ModeInitial {
							text += " --re-review"
						}
						if specPath := parseSpecPath(parentText); specPath != "" && parseSpecPath(text) == "" {
							text += " --spec " + specPath
						}
						msgEv := &slackevents.MessageEvent{
							Text:      text,
							Channel:   ev.Channel,
							TimeStamp: ev.TimeStamp,
						}
						ctx, cancel := context.WithCancel(context.Background())
						trackReview(msgEv.TimeStamp, prURL, cancel)
						go handlePR(ctx, api, msgEv, prURL, owner, repo, prNum, ev.Channel, notifyUserID, reviewQuestions)
					} else {
						ctx, cancel := context.WithCancel(context.Background())
						trackReview(ev.TimeStamp, prURL, cancel)
						go func() {
							defer untrackReview(ev.TimeStamp, prURL)
							handleThreadFollowup(ctx, api, ev, owner, repo, prNum, notifyUserID)
						}()
					}
				}

			case string(slackevents.Message):
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
					if isReviewActive(prURL) {
						log.Printf("skipping duplicate auto-review for %s (already in progress)", prURL)
						continue
					}
					ctx, cancel := context.WithCancel(context.Background())
					trackReview(ev.TimeStamp, prURL, cancel)
					go handlePR(ctx, api, ev, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions)
				}
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

func handlePR(ctx context.Context, api SlackAPI, ev *slackevents.MessageEvent, prURL, owner, repo, prNum, channelID, notifyUserID, reviewQuestions string) {
	defer untrackReview(ev.TimeStamp, prURL)

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
	diff, err := fetchDiff(ctx, owner, repo, prNum)
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
		if title := fetchPRTitle(ctx, owner, repo, prNum); title != "" {
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

	previousReviews := fetchPRContext(ctx, owner, repo, prNum)
	acknowledgedIssues := fetchAcknowledgedIssues(ctx, owner, repo, prNum)
	if acknowledgedIssues != "" {
		dmUser(api, notifyUserID, fmt.Sprintf("Found acknowledged issues for <%s>", prURL))
	}

	specPath := parseSpecPath(ev.Text)
	if specPath == "" {
		if matches := previousSpecPattern.FindAllStringSubmatch(previousReviews, -1); len(matches) > 0 {
			specPath = matches[len(matches)-1][1]
			dmUser(api, notifyUserID, fmt.Sprintf("Reusing spec from previous review: %s", specPath))
		}
	}
	var specContent string
	if specPath != "" {
		var specErr error
		if strings.HasPrefix(specPath, "/") || strings.HasPrefix(specPath, "~") || strings.HasPrefix(specPath, ".") {
			specContent, specErr = readSpecFile(specPath)
		} else {
			specContent, specErr = fetchSpecFromRepo(ctx, owner, repo, specPath, prNum)
		}
		if specErr != nil {
			dmUser(api, notifyUserID, fmt.Sprintf("Warning: could not read spec %s: %v (continuing without spec)", specPath, specErr))
		} else {
			dmUser(api, notifyUserID, fmt.Sprintf("Including spec from %s (%d chars)...", specPath, len(specContent)))
		}
	}

	agentCount := 4
	if mode == ModeQuick {
		agentCount = 1
	}
	dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Launching %d agent(s) in %s mode...", len(diff), agentCount, mode))

	req := ReviewRequest{
		Diff:               diff,
		PRURL:              prURL,
		Questions:          reviewQuestions,
		Mode:               mode,
		SelfReview:         selfReview,
		JiraTicket:         jiraTicket,
		JiraContext:        jiraContext,
		PreviousReviews:    previousReviews,
		AcknowledgedIssues: acknowledgedIssues,
		SpecContent:        specContent,
		SpecPath:           specPath,
	}

	if ctx.Err() != nil {
		postCancelled(api, ev, prURL, channelID, notifyUserID)
		return
	}

	review, score, stats, err := reviewWithClaude(ctx, api, notifyUserID, req)
	if err != nil {
		if ctx.Err() != nil {
			postCancelled(api, ev, prURL, channelID, notifyUserID)
		} else if selfReview {
			_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
			_ = api.AddReaction("x", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
			dmUser(api, notifyUserID, fmt.Sprintf("Failed to review <%s>: %v", prURL, err))
		} else {
			postError(api, ev, prURL, channelID, notifyUserID, err)
		}
		return
	}

	if req.SpecPath != "" {
		review += fmt.Sprintf("\n\n<!-- spec: %s -->", req.SpecPath)
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

// --- Repo Cache ---

type RepoCache struct {
	baseDir string
	mu      sync.Mutex
	locks   map[string]*sync.Mutex
}

func NewRepoCache() *RepoCache {
	dir := os.Getenv("REPO_CACHE_DIR")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			log.Fatalf("repo-cache: cannot determine home dir: %v", err)
		}
		dir = filepath.Join(home, ".pr-review-cache")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Fatalf("repo-cache: cannot create %s: %v", dir, err)
	}
	log.Printf("repo-cache: %s", dir)
	return &RepoCache{
		baseDir: dir,
		locks:   make(map[string]*sync.Mutex),
	}
}

func (c *RepoCache) repoLock(slug string) *sync.Mutex {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.locks[slug] == nil {
		c.locks[slug] = &sync.Mutex{}
	}
	return c.locks[slug]
}

func (c *RepoCache) gitDir(owner, repo string) string {
	return filepath.Join(c.baseDir, owner, repo+".git")
}

func (c *RepoCache) EnsureRepo(ctx context.Context, owner, repo string) (string, error) {
	slug := owner + "/" + repo
	mu := c.repoLock(slug)
	mu.Lock()
	defer mu.Unlock()

	gd := c.gitDir(owner, repo)

	if _, err := os.Stat(filepath.Join(gd, "HEAD")); os.IsNotExist(err) {
		log.Printf("repo-cache: cloning %s", slug)
		if err := os.MkdirAll(filepath.Dir(gd), 0o755); err != nil {
			return "", fmt.Errorf("create cache dir: %w", err)
		}
		repoURL := fmt.Sprintf("https://github.com/%s/%s.git", owner, repo)
		cmd := exec.CommandContext(ctx, "git", "clone", "--bare", repoURL, gd)
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("bare clone %s: %s", slug, string(out))
		}
		log.Printf("repo-cache: cloned %s", slug)
	} else {
		log.Printf("repo-cache: fetching %s", slug)
		cmd := exec.CommandContext(ctx, "git", "--git-dir", gd, "fetch", "--prune", "origin")
		if out, err := cmd.CombinedOutput(); err != nil {
			return "", fmt.Errorf("fetch %s: %s", slug, string(out))
		}
	}

	return gd, nil
}

func (c *RepoCache) FetchPR(ctx context.Context, gitDir, owner, repo, prNum string) error {
	slug := owner + "/" + repo
	mu := c.repoLock(slug)
	mu.Lock()
	defer mu.Unlock()

	ref := fmt.Sprintf("+pull/%s/head:refs/prs/%s", prNum, prNum)
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "fetch", "origin", ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("fetch PR %s#%s: %s", slug, prNum, string(out))
	}
	return nil
}

func (c *RepoCache) FileContent(ctx context.Context, gitDir, ref, path string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "show", ref+":"+path)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// --- Smart Diff ---

type filePriority int

const (
	prioImpl filePriority = iota
	prioConfig
	prioTest
	prioGenerated
)

func classifyFile(path string) filePriority {
	base := filepath.Base(path)

	if strings.Contains(path, "vendor/") || strings.Contains(path, "node_modules/") ||
		strings.HasSuffix(base, ".lock") || base == "package-lock.json" ||
		base == "yarn.lock" || base == "pnpm-lock.yaml" || base == "go.sum" ||
		strings.Contains(path, "generated") || strings.HasSuffix(base, ".gen.go") ||
		strings.HasSuffix(base, ".pb.go") {
		return prioGenerated
	}

	if strings.HasSuffix(base, "_test.go") ||
		strings.HasSuffix(base, ".test.ts") || strings.HasSuffix(base, ".test.tsx") ||
		strings.HasSuffix(base, ".test.js") || strings.HasSuffix(base, ".test.jsx") ||
		strings.HasSuffix(base, ".spec.ts") || strings.HasSuffix(base, ".spec.tsx") ||
		strings.Contains(path, "/test/") || strings.Contains(path, "/tests/") ||
		strings.Contains(path, "/__tests__/") || strings.Contains(path, "/testdata/") {
		return prioTest
	}

	if base == "go.mod" || base == "package.json" || base == "tsconfig.json" ||
		strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") ||
		strings.HasSuffix(base, ".toml") || base == ".gitignore" ||
		base == "Dockerfile" || base == "Makefile" || base == "Taskfile.yaml" {
		return prioConfig
	}

	return prioImpl
}

type changedFile struct {
	path     string
	diff     string
	priority filePriority
	size     int
}

var diffFilePattern = regexp.MustCompile(`(?m)^diff --git a/\S+ b/(\S+)`)

func splitDiffByFile(fullDiff string) map[string]string {
	result := make(map[string]string)
	locs := diffFilePattern.FindAllStringSubmatchIndex(fullDiff, -1)
	for i, loc := range locs {
		end := len(fullDiff)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		path := fullDiff[loc[2]:loc[3]]
		result[path] = fullDiff[loc[0]:end]
	}
	return result
}

func buildSmartDiff(ctx context.Context, gitDir, mergeBase, prRef string) (string, error) {
	const maxChars = 120_000

	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "diff", mergeBase, prRef)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("diff: %s", string(exitErr.Stderr))
		}
		return "", fmt.Errorf("diff: %w", err)
	}
	fullDiff := string(out)

	fileCount := len(diffFilePattern.FindAllString(fullDiff, -1))
	log.Printf("repo-cache: diff is %d chars, %d file(s)", len(fullDiff), fileCount)

	if len(fullDiff) <= maxChars {
		return fullDiff, nil
	}

	fileDiffs := splitDiffByFile(fullDiff)
	diffs := make([]changedFile, 0, len(fileDiffs))
	for path, d := range fileDiffs {
		diffs = append(diffs, changedFile{
			path:     path,
			diff:     d,
			priority: classifyFile(path),
			size:     len(d),
		})
	}

	sort.Slice(diffs, func(i, j int) bool {
		if diffs[i].priority != diffs[j].priority {
			return diffs[i].priority < diffs[j].priority
		}
		return diffs[i].size < diffs[j].size
	})

	var result strings.Builder
	var omitted []string
	for _, f := range diffs {
		if result.Len()+f.size > maxChars {
			omitted = append(omitted, fmt.Sprintf("%s (%s)", f.path, humanSize(f.size)))
			continue
		}
		result.WriteString(f.diff)
	}

	if len(omitted) > 0 {
		fmt.Fprintf(&result, "\n\n[%d file(s) omitted — review separately:\n", len(omitted))
		for _, o := range omitted {
			fmt.Fprintf(&result, "  - %s\n", o)
		}
		result.WriteString("]")
	}

	log.Printf("repo-cache: smart diff %d/%d chars, %d file(s) omitted", result.Len(), maxChars, len(omitted))
	return result.String(), nil
}

func humanSize(chars int) string {
	if chars < 1000 {
		return fmt.Sprintf("%d chars", chars)
	}
	return fmt.Sprintf("%.1fk chars", float64(chars)/1000)
}

// --- Fetch Diff ---

func fetchDiff(ctx context.Context, owner, repo, prNum string) (string, error) {
	gitDir, err := repoCache.EnsureRepo(ctx, owner, repo)
	if err != nil {
		return "", fmt.Errorf("repo cache: %w", err)
	}

	baseRef, err := getPRBaseRef(ctx, owner, repo, prNum)
	if err != nil {
		return "", err
	}

	if err := repoCache.FetchPR(ctx, gitDir, owner, repo, prNum); err != nil {
		return "", err
	}

	prRef := "refs/prs/" + prNum
	baseRefFull := "refs/heads/" + baseRef

	mergeBase, err := gitMergeBase(ctx, gitDir, baseRefFull, prRef)
	if err != nil {
		return "", err
	}

	return buildSmartDiff(ctx, gitDir, mergeBase, prRef)
}

func getPRBaseRef(ctx context.Context, owner, repo, prNum string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--json", "baseRefName", "--jq", ".baseRefName")
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("get PR base ref: %w; stderr: %s", err, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("get PR base ref: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func gitMergeBase(ctx context.Context, gitDir, ref1, ref2 string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir, "merge-base", ref1, ref2)
	out, err := cmd.Output()
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return "", fmt.Errorf("merge-base %s %s: %s", ref1, ref2, string(exitErr.Stderr))
		}
		return "", fmt.Errorf("merge-base: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

func fetchPRTitle(ctx context.Context, owner, repo, prNum string) string {
	cmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", fmt.Sprintf("%s/%s", owner, repo),
		"--json", "title", "--jq", ".title")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func fetchPRContext(ctx context.Context, owner, repo, prNum string) string {
	repoSlug := fmt.Sprintf("%s/%s", owner, repo)

	descCmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", repoSlug,
		"--json", "title,body,author",
		"--jq", `"## PR: " + .title + "\nAuthor: " + .author.login + "\n\n" + .body`)
	descOut, _ := descCmd.Output()

	commentsCmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", repoSlug,
		"--json", "comments",
		"--jq", `.comments[] | "### Comment by " + .author.login + " (" + .createdAt + ")\n" + .body`)
	commentsOut, _ := commentsCmd.Output()

	reviewsCmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", repoSlug,
		"--json", "reviews",
		"--jq", `.reviews[] | select(.body != "") | "### Review by " + .author.login + " [" + .state + "] (" + .submittedAt + ")\n" + .body`)
	reviewsOut, _ := reviewsCmd.Output()

	var parts []string
	if desc := strings.TrimSpace(string(descOut)); desc != "" {
		parts = append(parts, desc)
	}
	if reviews := strings.TrimSpace(string(reviewsOut)); reviews != "" {
		parts = append(parts, reviews)
	}
	if comments := strings.TrimSpace(string(commentsOut)); comments != "" {
		parts = append(parts, comments)
	}

	result := strings.Join(parts, "\n\n---\n\n")
	if len(result) > 12000 {
		result = result[:12000] + "\n[truncated]"
	}
	if result == "" {
		return ""
	}
	log.Printf("pr-context: fetched %d chars for %s/%s#%s", len(result), owner, repo, prNum)
	return result
}

func fetchAcknowledgedIssues(ctx context.Context, owner, repo, prNum string) string {
	repoSlug := fmt.Sprintf("%s/%s", owner, repo)

	// Fetch issue-level comments
	issueCmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum,
		"--repo", repoSlug, "--json", "comments")
	issueOut, _ := issueCmd.Output()

	// Fetch inline review comments
	reviewCmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/%s/pulls/%s/comments", owner, repo, prNum))
	reviewOut, _ := reviewCmd.Output()

	type comment struct {
		Author string
		Body   string
	}
	var acked []comment

	var issueResult struct {
		Comments []struct {
			Author struct {
				Login string `json:"login"`
			} `json:"author"`
			Body string `json:"body"`
		} `json:"comments"`
	}
	if json.Unmarshal(issueOut, &issueResult) == nil {
		for _, c := range issueResult.Comments {
			if ackPattern.MatchString(c.Body) {
				acked = append(acked, comment{Author: c.Author.Login, Body: c.Body})
			}
		}
	}

	var reviewComments []struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string `json:"body"`
	}
	if json.Unmarshal(reviewOut, &reviewComments) == nil {
		for _, c := range reviewComments {
			if ackPattern.MatchString(c.Body) {
				acked = append(acked, comment{Author: c.User.Login, Body: c.Body})
			}
		}
	}

	if len(acked) == 0 {
		return ""
	}

	var sb strings.Builder
	for _, c := range acked {
		fmt.Fprintf(&sb, "**%s:** %s\n\n", c.Author, c.Body)
	}
	log.Printf("ack: found %d acknowledged issues for %s/%s#%s", len(acked), owner, repo, prNum)
	return strings.TrimSpace(sb.String())
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

func fetchSpecFromRepo(ctx context.Context, owner, repo, specPath, prNum string) (string, error) {
	repoSlug := fmt.Sprintf("%s/%s", owner, repo)
	headCmd := exec.CommandContext(ctx, "gh", "pr", "view", prNum, "--repo", repoSlug,
		"--json", "headRefName", "--jq", ".headRefName")
	headOut, err := headCmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("get head ref for spec: %s", strings.TrimSpace(string(headOut)))
	}
	headRef := strings.TrimSpace(string(headOut))

	cmd := exec.CommandContext(ctx, "gh", "api",
		fmt.Sprintf("repos/%s/%s/contents/%s?ref=%s", owner, repo, specPath, headRef),
		"-H", "Accept: application/vnd.github.raw")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("fetch %s from %s@%s: %s", specPath, repoSlug, headRef, strings.TrimSpace(string(out)))
	}

	content := string(out)
	const maxChars = 20_000
	if len(content) > maxChars {
		content = content[:maxChars] + "\n[spec truncated]"
	}
	return content, nil
}

func reviewWithClaude(ctx context.Context, api SlackAPI, notifyUserID string, req ReviewRequest) (string, *ScoreResult, *UsageStats, error) {
	stats := &UsageStats{}

	var extraContext strings.Builder
	if req.JiraContext != "" {
		extraContext.WriteString("\n\n" + req.JiraContext + "\n")
	}
	if req.PreviousReviews != "" {
		if req.Mode == ModeReReview {
			extraContext.WriteString(fmt.Sprintf("\n\n## PR Discussion & Previous Reviews\nThis PR was reviewed before. Consider whether previous feedback was addressed and focus on what changed:\n\n%s\n", req.PreviousReviews))
		} else {
			extraContext.WriteString(fmt.Sprintf("\n\n## PR Discussion Context\nThe following is the PR description, review comments, and discussion so far. Use this to understand the author's intent and any concerns already raised:\n\n%s\n", req.PreviousReviews))
		}
	}
	if req.SpecContent != "" {
		extraContext.WriteString(fmt.Sprintf("\n\n## Specification / Requirements\nThe following spec defines what this PR should implement. Evaluate whether the PR accurately implements the spec and flag any drift from requirements — missing features, extra unspecified behavior, or contradictions:\n\n%s\n", req.SpecContent))
	}
	if req.AcknowledgedIssues != "" {
		extraContext.WriteString(fmt.Sprintf("\n\n## Acknowledged Issues\nThe following issues from previous reviews have been explicitly acknowledged by the author (via ack, won't fix, intentional, by design, etc.). Do NOT re-flag these as issues. Do NOT penalize the score for these items. Only mention them if the code has materially changed in a way that reintroduces the concern:\n\n%s\n", req.AcknowledgedIssues))
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
		score, resp, scoreErr = runScorer(ctx, req.Diff, req.SpecContent, req.AcknowledgedIssues)
		if scoreErr != nil {
			log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
		} else {
			stats.Add(resp)
			log.Printf("scorer: done for %s (score: %d/100, $%.4f)", req.PRURL, score.Overall, resp.TotalCostUSD)
		}
	}()

	if req.Mode == ModeQuick {
		result, err := runQuickReview(ctx, req.PRURL, req.Diff, contextBlock, questionsStr, stats)
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

	sharedContext := buildSharedContext(req.PRURL, req.Diff, req.Mode, contextBlock, questionsStr)
	perspectives := buildPerspectives()

	reviews := make([]string, len(perspectives))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for i, p := range perspectives {
		wg.Add(1)
		go func(idx int, name, prompt string) {
			defer wg.Done()
			log.Printf("agent %s: starting %s review for %s", name, req.Mode, req.PRURL)
			text, resp, err := runClaudeWithCache(ctx, sharedContext, prompt)
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
			log.Printf("agent %s: done for %s ($%.4f, cache: %d/%d)", name, req.PRURL, resp.TotalCostUSD, resp.Usage.CacheReadInputTokens, resp.Usage.InputTokens)
		}(i, p.name, p.prompt)
	}
	wg.Wait()

	if firstErr != nil {
		return "", nil, stats, firstErr
	}

	dmUser(api, notifyUserID, fmt.Sprintf("All %d agents done. Running validator...", len(perspectives)))
	allReviews := strings.Join(reviews, "\n\n---\n\n")

	log.Printf("validator: starting for %s", req.PRURL)
	validated, valResp, err := runClaude(ctx, fmt.Sprintf(`You are a review validator. You have %d independent code reviews of a PR and the original diff.

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
	merged, mergeResp, err := runMerger(ctx, allReviews, validated, req.Mode, req.SpecContent, req.AcknowledgedIssues)
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

func buildSharedContext(prURL, diff string, mode ReviewMode, contextBlock, questionsStr string) string {
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

	return fmt.Sprintf(`%sReview this pull request: %s
%s
%s

`+"```diff\n%s\n```", modePreamble, prURL, contextBlock, questionsStr, diff)
}

func buildPerspectives() []perspective {
	return []perspective{
		{
			name: "correctness",
			prompt: `You are a code review agent focused on CORRECTNESS and SECURITY.

Focus on:
- Bugs, logic errors, edge cases
- Security vulnerabilities (injection, auth issues, data leaks)
- Race conditions, error handling gaps
- API contract violations

Be specific. Reference exact lines. No fluff.`,
		},
		{
			name: "design",
			prompt: `You are a code review agent focused on DESIGN and MAINTAINABILITY.

Focus on:
- Architecture and design patterns
- Code organization, naming, readability
- Unnecessary complexity or premature abstraction
- Missing tests or test quality
- Performance implications

Be specific. Reference exact lines. No fluff.`,
		},
		{
			name: "pragmatic",
			prompt: `You are a pragmatic senior engineer reviewing this PR.

Focus on:
- Does this actually solve the problem it claims to?
- What could break in production?
- What would you want changed before approving?
- Are there simpler approaches?

Be direct and opinionated. Skip obvious things that are fine.`,
		},
		{
			name: "go-expert",
			prompt: `You are an elite Go code reviewer with deep expertise in Go 1.26, its standard library, and production-grade Go development. You review with the rigor of a senior staff engineer at a top-tier infrastructure company.

## Review Criteria

### Correctness
- Logic errors, off-by-one mistakes, race conditions
- Proper error handling: explicit error returns, no swallowed errors, %w for wrapping
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

Be specific, not vague. Show exactly what and why. Respect existing codebase patterns — don't suggest rewrites outside PR scope.`,
		},
	}
}

func runQuickReview(ctx context.Context, prURL, diff, contextBlock, questionsStr string, stats *UsageStats) (string, error) {
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
	text, resp, err := runClaude(ctx, prompt)
	if err != nil {
		return "", fmt.Errorf("quick review failed: %w", err)
	}
	stats.Add(resp)
	log.Printf("quick-review: done for %s ($%.4f)", prURL, resp.TotalCostUSD)
	return text, nil
}

func runScorer(ctx context.Context, diff, specContent, acknowledgedIssues string) (ScoreResult, claudeResponse, error) {
	specDimension := ""
	specBlock := ""
	specJSON := ""
	if specContent != "" {
		specDimension = "\n- spec_compliance: How accurately and completely the diff implements the spec requirements, without drift or missing items"
		specBlock = fmt.Sprintf("\n## Specification\nEvaluate the diff against this spec:\n\n%s\n\n", specContent)
		specJSON = `,"spec_compliance":N`
	}

	ackBlock := ""
	if acknowledgedIssues != "" {
		ackBlock = fmt.Sprintf("\n## Acknowledged Issues\nThe following issues were explicitly acknowledged by the author (ack, won't fix, intentional, by design, etc.). Do NOT penalize scores for these items — they represent informed decisions, not oversights:\n\n%s\n\n", acknowledgedIssues)
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
%s%s
Respond with ONLY this JSON object, no markdown fences, no other text:
{"correctness":N,"security":N,"design":N,"go_quality":N,"testing":N,"production_readiness":N%s,"overall":N,"summary":"one sentence"}

`+"```diff\n%s\n```", specDimension, specBlock, ackBlock, specJSON, diff)

	text, resp, err := runClaude(ctx, prompt)
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

func runMerger(ctx context.Context, allReviews, validated string, mode ReviewMode, specContent, acknowledgedIssues string) (string, claudeResponse, error) {
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

	ackSection := ""
	ackRule := ""
	if acknowledgedIssues != "" {
		ackSection = "\n8. **Acknowledged Issues** — briefly list items the author already acknowledged, confirming they are not blocking"
		ackRule = "\n- Issues explicitly acknowledged by the author (ack, won't fix, intentional, by design) must NOT appear in Critical Issues or Design Concerns. List them separately as acknowledged. Do not let them influence the verdict negatively"
	}

	text, resp, err := runClaude(ctx, fmt.Sprintf(`You are a review synthesizer. You have 4 independent code reviews and a validation report.

Merge them into ONE cohesive, comprehensive review. Structure:

1. **Summary** — one sentence on what this PR does
2. **Critical Issues** — bugs, security, correctness problems (if any)
3. **Design Concerns** — architecture, complexity, maintainability (if any)
4. **Suggestions** — improvements worth making
5. **What's Good** — brief acknowledgment of things done well (1-2 lines max)
6. **Verdict** — Approve / Request Changes / Needs Discussion%s%s

Rules:
- The GO-EXPERT review is the most authoritative voice. When reviewers conflict, defer to GO-EXPERT. Its critical issues are always included. Its verdict carries the most weight in the final verdict.
- Deduplicate overlapping feedback
- Drop anything the validator flagged as incorrect
- Incorporate answers to reviewer questions from the validation
- Keep it actionable and specific
- Reference file names and line numbers where relevant%s%s
%s
## Reviews
%s

## Validation Report
%s%s`, specSection, ackSection, specRule, ackRule, modeRules, allReviews, validated, specContext))
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

func getModel() anthropic.Model {
	model := os.Getenv("CLAUDE_MODEL")
	if model == "" {
		return anthropic.ModelClaudeOpus4_6
	}
	return anthropic.Model(model)
}

func estimateCost(model string, in, out, cacheWrite, cacheRead int64) float64 {
	var inPerM, outPerM float64
	switch {
	case strings.Contains(model, "opus"):
		inPerM, outPerM = 15.0, 75.0
	case strings.Contains(model, "sonnet"):
		inPerM, outPerM = 3.0, 15.0
	case strings.Contains(model, "haiku"):
		inPerM, outPerM = 0.80, 4.0
	default:
		inPerM, outPerM = 15.0, 75.0
	}
	cost := float64(in)/1e6*inPerM + float64(out)/1e6*outPerM
	cost += float64(cacheWrite) / 1e6 * inPerM * 1.25
	cost += float64(cacheRead) / 1e6 * inPerM * 0.1
	return cost
}

func extractResult(content []anthropic.ContentBlockUnion, usage anthropic.Usage, duration time.Duration) (string, claudeResponse) {
	var text string
	for _, block := range content {
		if block.Type == "text" {
			text += block.Text
		}
	}
	model := string(getModel())
	cr := claudeResponse{
		Result:        text,
		TotalCostUSD:  estimateCost(model, usage.InputTokens, usage.OutputTokens, usage.CacheCreationInputTokens, usage.CacheReadInputTokens),
		DurationMS:    duration.Milliseconds(),
		DurationAPIMS: duration.Milliseconds(),
		NumTurns:      1,
	}
	cr.Usage.InputTokens = usage.InputTokens
	cr.Usage.OutputTokens = usage.OutputTokens
	cr.Usage.CacheCreationInputTokens = usage.CacheCreationInputTokens
	cr.Usage.CacheReadInputTokens = usage.CacheReadInputTokens
	return strings.TrimSpace(text), cr
}

func runClaude(ctx context.Context, prompt string) (string, claudeResponse, error) {
	start := time.Now()
	msg, err := anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     getModel(),
		MaxTokens: 16384,
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(prompt)),
		},
	})
	if err != nil {
		return "", claudeResponse{}, fmt.Errorf("anthropic API: %w", err)
	}
	text, cr := extractResult(msg.Content, msg.Usage, time.Since(start))
	return text, cr, nil
}

func runClaudeWithCache(ctx context.Context, cachedContent, prompt string) (string, claudeResponse, error) {
	start := time.Now()
	msg, err := anthropicClient.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     getModel(),
		MaxTokens: 16384,
		Messages: []anthropic.MessageParam{
			{
				Role: anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{
					{OfText: &anthropic.TextBlockParam{
						Text:         cachedContent,
						CacheControl: anthropic.NewCacheControlEphemeralParam(),
					}},
					anthropic.NewTextBlock(prompt),
				},
			},
		},
	})
	if err != nil {
		return "", claudeResponse{}, fmt.Errorf("anthropic API: %w", err)
	}
	text, cr := extractResult(msg.Content, msg.Usage, time.Since(start))
	return text, cr, nil
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

func postCancelled(api SlackAPI, ev *slackevents.MessageEvent, prURL, channelID, notifyUserID string) {
	log.Printf("review cancelled for %s", prURL)
	_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	_ = api.AddReaction("no_entry_sign", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
	_, _, _ = api.PostMessage(
		channelID,
		slack.MsgOptionText(fmt.Sprintf("Review cancelled for <%s>", prURL), false),
		slack.MsgOptionTS(ev.TimeStamp),
	)
	dmUser(api, notifyUserID, fmt.Sprintf("Review cancelled for <%s>", prURL))
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
