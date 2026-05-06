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
	"text/template"
	"time"

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

	agentMaxTurns = 6
)

type ReviewRequest struct {
	Diff               string
	PRURL              string
	Owner              string
	Repo               string
	PRNum              string
	Questions          string
	Mode               ReviewMode
	SelfReview         bool
	JiraTicket         string
	JiraContext        string
	PreviousReviews    string
	AcknowledgedIssues string
	SpecContent        string
	SpecPath           string
	Flags              map[string]bool
}

type claudeResponse struct {
	Result        string  `json:"result"`
	SessionID     string  `json:"session_id"`
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

type AgentMetric struct {
	Name     string
	CostUSD  float64
	Duration time.Duration
}

type UsageStats struct {
	mu                sync.Mutex
	TotalCostUSD      float64
	TotalDurationMS   int64
	TotalInputTokens  int64
	TotalOutputTokens int64
	TotalCacheRead    int64
	AgentCalls        int
	AgentMetrics      []AgentMetric
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

func (u *UsageStats) AddAgent(name string, cost float64, dur time.Duration) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.AgentMetrics = append(u.AgentMetrics, AgentMetric{Name: name, CostUSD: cost, Duration: dur})
}

func (u *UsageStats) String() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return fmt.Sprintf("%d calls | $%.4f | %s in + %s out tokens | %ds API time",
		u.AgentCalls, u.TotalCostUSD,
		formatTokens(u.TotalInputTokens), formatTokens(u.TotalOutputTokens),
		u.TotalDurationMS/1000)
}

func (u *UsageStats) MetricsSummary(model, triggerUser, channelID string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var b strings.Builder
	b.WriteString("*Review Metrics*\n")
	b.WriteString(fmt.Sprintf("> *Model:* `%s`\n", model))
	b.WriteString(fmt.Sprintf("> *Triggered by:* <@%s> in <#%s>\n", triggerUser, channelID))
	b.WriteString(fmt.Sprintf("> *Total cost:* $%.4f\n", u.TotalCostUSD))
	b.WriteString("> *Agent breakdown:*\n")
	for _, m := range u.AgentMetrics {
		b.WriteString(fmt.Sprintf(">   • `%s` — $%.4f, %s\n", m.Name, m.CostUSD, m.Duration.Round(time.Second)))
	}
	return b.String()
}

func formatTokens(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d", n)
	}
	return fmt.Sprintf("%.1fk", float64(n)/1000)
}

func diffLines(diff string) int {
	return strings.Count(diff, "\n")
}

type agentFile struct {
	name     string
	flag     string
	model    string
	template *template.Template
}

type promptData struct {
	ModePreamble string
	PRURL        string
	ContextBlock string
	QuestionsStr string
	Diff         string
}

var agentsDir = "agents"

type ProjectAgentConfig struct {
	Agents []string `json:"agents"`
}

type ProjectsConfig struct {
	Projects map[string]ProjectAgentConfig `json:"projects"`
	Default  ProjectAgentConfig            `json:"default"`
}

var projectsConfigPath = "projects.json"

func loadProjectsConfig() (*ProjectsConfig, error) {
	data, err := os.ReadFile(projectsConfigPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read projects config: %w", err)
	}
	var cfg ProjectsConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse projects config: %w", err)
	}
	return &cfg, nil
}

func (cfg *ProjectsConfig) agentsForRepo(owner, repo string) []string {
	if cfg == nil {
		return nil
	}
	key := owner + "/" + repo
	for pattern, pc := range cfg.Projects {
		if strings.EqualFold(pattern, key) {
			return pc.Agents
		}
	}
	if len(cfg.Default.Agents) > 0 {
		return cfg.Default.Agents
	}
	return nil
}

func filterAgentsByProject(agents []agentFile, allowed []string) []agentFile {
	if len(allowed) == 0 {
		return agents
	}
	set := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		set[name] = true
	}
	var filtered []agentFile
	for _, a := range agents {
		if set[a.name] {
			filtered = append(filtered, a)
		}
	}
	return filtered
}

func loadAgents() ([]agentFile, error) {
	entries, err := os.ReadDir(agentsDir)
	if err != nil {
		return nil, fmt.Errorf("read agents dir %s: %w", agentsDir, err)
	}
	var agents []agentFile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		path := filepath.Join(agentsDir, e.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read agent file %s: %w", path, err)
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		content := string(raw)
		var flag, model string
		if strings.HasPrefix(content, "---\n") {
			if end := strings.Index(content[4:], "\n---\n"); end >= 0 {
				frontmatter := content[4 : 4+end]
				content = content[4+end+5:]
				for _, line := range strings.Split(frontmatter, "\n") {
					if strings.HasPrefix(line, "flag:") {
						flag = strings.TrimSpace(strings.TrimPrefix(line, "flag:"))
					}
					if strings.HasPrefix(line, "model:") {
						model = strings.TrimSpace(strings.TrimPrefix(line, "model:"))
					}
				}
			}
		}
		tmpl, err := template.New(name).Parse(content)
		if err != nil {
			return nil, fmt.Errorf("parse agent template %s: %w", path, err)
		}
		agents = append(agents, agentFile{name: name, flag: flag, model: model, template: tmpl})
	}
	if len(agents) == 0 {
		return nil, fmt.Errorf("no .md agent files found in %s", agentsDir)
	}
	sort.Slice(agents, func(i, j int) bool {
		return agents[i].name < agents[j].name
	})
	log.Printf("loaded %d agent(s) from %s: %s", len(agents), agentsDir, agentNames(agents))
	return agents, nil
}

func agentNames(agents []agentFile) string {
	names := make([]string, len(agents))
	for i, a := range agents {
		names[i] = a.name
	}
	return strings.Join(names, ", ")
}

func renderAgent(a agentFile, data promptData) (string, error) {
	var buf strings.Builder
	if err := a.template.Execute(&buf, data); err != nil {
		return "", fmt.Errorf("render agent %s: %w", a.name, err)
	}
	return buf.String(), nil
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

type PerspectiveScore struct {
	Agent      string `json:"agent"`
	Score      int    `json:"score"`
	Confidence int    `json:"confidence"`
	Rationale  string `json:"rationale"`
}

const scoreSuffix = `

## Perspective Score

After your review, rate this PR's overall quality FROM YOUR PERSPECTIVE on a scale of 0-100 (100 = flawless, 0 = critically broken).

End your response with EXACTLY this JSON block on its own line:
` + "```" + `
{"score":N,"confidence":N,"rationale":"one sentence explaining your score"}
` + "```" + `
- score: 0-100 overall quality from your review perspective
- confidence: 0-100 how confident you are in your assessment (low if diff is unclear or you lack context)
- rationale: one sentence summary of why you gave this score`

var perspectiveScorePattern = regexp.MustCompile("```(?:json)?\\s*\\n?\\s*({\\s*\"score\"\\s*:.+?})\\s*\\n?\\s*```")

func extractPerspectiveScore(agentName, text string) (review string, ps PerspectiveScore) {
	ps.Agent = agentName
	loc := perspectiveScorePattern.FindStringSubmatchIndex(text)
	if loc == nil {
		return text, ps
	}
	jsonStr := text[loc[2]:loc[3]]
	review = strings.TrimSpace(text[:loc[0]])
	if err := json.Unmarshal([]byte(jsonStr), &ps); err != nil {
		log.Printf("agent %s: failed to parse perspective score: %v", agentName, err)
		return text, PerspectiveScore{Agent: agentName}
	}
	ps.Agent = agentName
	return review, ps
}

var (
	ghPRPattern          = regexp.MustCompile(`<?https://github\.com/([^/>\s]+)/([^/>\s]+)/pull/(\d+)[^>\s]*>?`)
	jiraTicketPattern    = regexp.MustCompile(`\b[A-Z]{2,}-\d+\b`)
	modePattern          = regexp.MustCompile(`--(initial|re-review|quick|final)\b`)
	selfPattern          = regexp.MustCompile(`--self\b`)
	testPattern          = regexp.MustCompile(`--test\b`)
	specPattern          = regexp.MustCompile(`--spec\s+(\S+)`)
	flagPattern          = regexp.MustCompile(`--([a-z][-a-z0-9]*)\b`)
	previousScorePattern = regexp.MustCompile(`\*\*Quality Score: (\d+)/100\*\*`)
	previousSpecPattern  = regexp.MustCompile(`<!-- spec: (\S+) -->`)
	reviewRequestPattern = regexp.MustCompile(`(?i)\breview\b`)
	ackPattern           = regexp.MustCompile(`(?i)\b(ack(nowledg(ed?|ing))?|won'?t\s*fix|wontfix|intentional|by\s*design|noted|accepted|will\s*(fix|address)\s*later|tracking\s+in|known\s+issue|out\s*of\s*scope|deferred)\b`)

	repoCache    *RepoCache
	sessionStore *SessionStore

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

func runCLI(args []string) {
	_ = godotenv.Load()
	repoCache = NewRepoCache()
	sessionStore = NewSessionStore()

	input := strings.Join(args, " ")
	m := ghPRPattern.FindStringSubmatch(input)
	if m == nil {
		fmt.Fprintf(os.Stderr, "Usage: pr-review-bot review <github-pr-url> [--initial|--quick|--re-review|--final] [--test] [--bare-necessities] [--no-github]\n")
		os.Exit(1)
	}
	owner, repo, prNum := m[1], m[2], m[3]
	prURL := fmt.Sprintf("https://github.com/%s/%s/pull/%s", owner, repo, prNum)

	mode, modeExplicit := parseMode(input)
	flags := parseFlags(input)
	noGitHub := strings.Contains(input, "--no-github")

	if mode == ModeInitial && !modeExplicit && sessionStore.Get(prURL) != "" {
		mode = ModeReReview
		log.Printf("auto-re-review: found existing session for %s, upgrading to re-review", prURL)
	}

	model := os.Getenv("CLAUDE_MODEL")
	if model == "" {
		model = "claude-opus-4-6"
	}

	ctx := context.Background()

	log.Printf("cli: fetching diff for %s", prURL)
	diff, err := fetchDiff(ctx, owner, repo, prNum)
	if err != nil {
		log.Fatalf("cli: failed to fetch diff: %v", err)
	}
	log.Printf("cli: diff fetched (%d chars, %d lines)", len(diff), diffLines(diff))

	if testPattern.MatchString(input) {
		agents, _ := loadAgents()
		projCfg, _ := loadProjectsConfig()
		filtered := filterAgentsByProject(agents, projCfg.agentsForRepo(owner, repo))
		filtered = filterAgents(filtered, flags)
		var names []string
		for _, a := range filtered {
			names = append(names, a.name)
		}

		claudeStatus := "ok"
		claudeStart := time.Now()
		claudeCmd := exec.CommandContext(ctx, "claude", "--version")
		claudeOut, claudeErr := claudeCmd.Output()
		claudeLatency := time.Since(claudeStart).Round(time.Millisecond)
		claudeVersion := strings.TrimSpace(string(claudeOut))
		if claudeErr != nil {
			claudeStatus = fmt.Sprintf("FAILED: %v", claudeErr)
			claudeVersion = "unknown"
		}

		ghStatus := "ok"
		ghStart := time.Now()
		ghCmd := exec.CommandContext(ctx, "gh", "auth", "status")
		if ghErr := ghCmd.Run(); ghErr != nil {
			ghStatus = fmt.Sprintf("FAILED: %v", ghErr)
		}
		ghLatency := time.Since(ghStart).Round(time.Millisecond)

		fmt.Printf("Test run for %s\n", prURL)
		fmt.Printf("  Diff:       %d chars, %d lines\n", len(diff), diffLines(diff))
		fmt.Printf("  Model:      %s\n", model)
		fmt.Printf("  Mode:       %s\n", mode)
		fmt.Printf("  Agents:     %s\n", strings.Join(names, ", "))
		fmt.Printf("  Pipeline:   agents → validator → scorer → merger\n")
		fmt.Printf("  Preflight:\n")
		fmt.Printf("    claude:   %s (v%s, %s)\n", claudeStatus, claudeVersion, claudeLatency)
		fmt.Printf("    gh:       %s (%s)\n", ghStatus, ghLatency)
		fmt.Printf("    diff:     ok\n")
		fmt.Printf("\nNo agents were run. Remove --test to run a full review.\n")
		return
	}

	nopAPI := &nopSlack{}
	reviewQuestions := os.Getenv("REVIEW_QUESTIONS")

	previousReviews := fetchPRContext(ctx, owner, repo, prNum)
	acknowledgedIssues := fetchAcknowledgedIssues(ctx, owner, repo, prNum)

	req := ReviewRequest{
		Diff:               diff,
		PRURL:              prURL,
		Owner:              owner,
		Repo:               repo,
		PRNum:              prNum,
		Questions:          reviewQuestions,
		Mode:               mode,
		Flags:              flags,
		PreviousReviews:    previousReviews,
		AcknowledgedIssues: acknowledgedIssues,
	}

	jiraTicket := parseJiraTicket(input)
	if jiraTicket == "" {
		if title := fetchPRTitle(ctx, owner, repo, prNum); title != "" {
			if jm := jiraTicketPattern.FindString(title); jm != "" {
				jiraTicket = jm
			}
		}
	}
	if jiraTicket != "" {
		req.JiraTicket = jiraTicket
		req.JiraContext = fetchJiraContext(jiraTicket)
	}

	log.Printf("cli: starting %s review of %s", mode, prURL)
	review, score, stats, err := reviewWithClaude(ctx, nopAPI, "", req)
	if err != nil {
		log.Fatalf("cli: review failed: %v", err)
	}

	fmt.Println(review)
	fmt.Fprintln(os.Stderr)
	if score != nil {
		fmt.Fprintf(os.Stderr, "Score: %d/100\n", score.Overall)
	}
	fmt.Fprintf(os.Stderr, "Usage: %s\n", stats)
	fmt.Fprintf(os.Stderr, "%s", stats.MetricsSummary(model, "cli", "cli"))

	if !noGitHub {
		log.Printf("cli: posting review to GitHub %s", prURL)
		if err := postGitHubComment(owner, repo, prNum, review); err != nil {
			log.Fatalf("cli: failed to post to GitHub: %v", err)
		}
		log.Printf("cli: review posted to GitHub")
	} else {
		log.Printf("cli: --no-github flag set, skipping GitHub post")
	}
}

type nopSlack struct{}

func (n *nopSlack) AddReaction(string, slack.ItemRef) error                { return nil }
func (n *nopSlack) RemoveReaction(string, slack.ItemRef) error             { return nil }
func (n *nopSlack) PostMessage(string, ...slack.MsgOption) (string, string, error) {
	return "", "", nil
}
func (n *nopSlack) OpenConversation(*slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	return &slack.Channel{}, false, false, nil
}
func (n *nopSlack) GetConversationHistory(*slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	return &slack.GetConversationHistoryResponse{}, nil
}
func (n *nopSlack) GetConversationReplies(*slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "review" {
		runCLI(os.Args[2:])
		return
	}

	_ = godotenv.Load()
	repoCache = NewRepoCache()
	sessionStore = NewSessionStore()

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
						if mode, explicit := parseMode(text); mode == ModeInitial && !explicit {
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

func parseMode(text string) (ReviewMode, bool) {
	if m := modePattern.FindStringSubmatch(text); m != nil {
		return ReviewMode(m[1]), true
	}
	return ModeInitial, false
}

func parseSpecPath(text string) string {
	if m := specPattern.FindStringSubmatch(text); m != nil {
		return m[1]
	}
	return ""
}

func parseFlags(text string) map[string]bool {
	reserved := map[string]bool{
		"initial": true, "re-review": true, "quick": true, "final": true,
		"self": true, "spec": true, "test": true, "no-github": true,
	}
	flags := make(map[string]bool)
	for _, m := range flagPattern.FindAllStringSubmatch(text, -1) {
		if !reserved[m[1]] {
			flags[m[1]] = true
		}
	}
	return flags
}

func filterAgents(agents []agentFile, flags map[string]bool) []agentFile {
	var filtered []agentFile
	for _, a := range agents {
		if a.flag == "" || flags[a.flag] {
			filtered = append(filtered, a)
		}
	}
	return filtered
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

	mode, modeExplicit := parseMode(ev.Text)
	selfReview := selfPattern.MatchString(ev.Text)
	jiraTicket := parseJiraTicket(ev.Text)
	flags := parseFlags(ev.Text)

	if mode == ModeInitial && !modeExplicit && sessionStore.Get(prURL) != "" {
		mode = ModeReReview
		log.Printf("auto-re-review: found existing session for %s, upgrading to re-review", prURL)
	}

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

	if testPattern.MatchString(ev.Text) {
		model := os.Getenv("CLAUDE_MODEL")
		if model == "" {
			model = "claude-opus-4-6"
		}
		agents, _ := loadAgents()
		projCfg, _ := loadProjectsConfig()
		filtered := filterAgentsByProject(agents, projCfg.agentsForRepo(owner, repo))
		filtered = filterAgents(filtered, flags)
		var agentNames []string
		for _, a := range filtered {
			agentNames = append(agentNames, a.name)
		}
		lines := diffLines(diff)

		claudeStatus := "ok"
		claudeStart := time.Now()
		claudeCmd := exec.CommandContext(ctx, "claude", "--version")
		claudeOut, claudeErr := claudeCmd.Output()
		claudeLatency := time.Since(claudeStart).Round(time.Millisecond)
		claudeVersion := strings.TrimSpace(string(claudeOut))
		if claudeErr != nil {
			claudeStatus = fmt.Sprintf("FAILED: %v", claudeErr)
			claudeVersion = "unknown"
		}

		ghStatus := "ok"
		ghStart := time.Now()
		ghCmd := exec.CommandContext(ctx, "gh", "auth", "status")
		if ghErr := ghCmd.Run(); ghErr != nil {
			ghStatus = fmt.Sprintf("FAILED: %v", ghErr)
		}
		ghLatency := time.Since(ghStart).Round(time.Millisecond)

		msg := fmt.Sprintf("*Test run for <%s>*\n"+
			"> *Diff:* %d chars, %d lines\n"+
			"> *Model:* `%s`\n"+
			"> *Mode:* %s\n"+
			"> *Agents:* %s\n"+
			"> *Triggered by:* <@%s> in <#%s>\n"+
			"> *Pipeline:* agents → validator → scorer → merger\n"+
			">\n"+
			"> *Preflight checks:*\n"+
			">   • `claude` CLI: %s (v%s, %s)\n"+
			">   • `gh` CLI: %s (%s)\n"+
			">   • git diff: fetched (%s)\n"+
			">\n"+
			"> No agents were run. Use without `--test` to run a full review.",
			prURL, len(diff), lines, model, mode,
			strings.Join(agentNames, ", "), ev.User, ev.Channel,
			claudeStatus, claudeVersion, claudeLatency,
			ghStatus, ghLatency,
			time.Duration(0)) // diff already fetched above
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_, _, _ = api.PostMessage(channelID,
			slack.MsgOptionText(msg, false),
			slack.MsgOptionTS(ev.TimeStamp))
		dmUser(api, notifyUserID, msg)
		log.Printf("test run: completed for %s (claude=%s, gh=%s, diff=%d lines)", prURL, claudeStatus, ghStatus, lines)
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

	if mode == ModeReReview {
		sessionID := sessionStore.Get(prURL)
		if sessionID != "" {
			dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Resuming previous session for re-review...", len(diff)))
		} else {
			dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Running delta re-review (no previous session)...", len(diff)))
		}
	} else {
		if mode == ModeQuick {
			dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Launching 1 agent in %s mode...", len(diff), mode))
		} else {
			agents, agentErr := loadAgents()
			agentCount := 0
			if agentErr == nil {
				projCfg, _ := loadProjectsConfig()
				filtered := filterAgentsByProject(agents, projCfg.agentsForRepo(owner, repo))
				agentCount = len(filterAgents(filtered, flags))
			}
			dmUser(api, notifyUserID, fmt.Sprintf("Diff fetched (%d chars). Launching %d agent(s) in %s mode...", len(diff), agentCount, mode))
		}
	}

	req := ReviewRequest{
		Diff:               diff,
		PRURL:              prURL,
		Owner:              owner,
		Repo:               repo,
		PRNum:              prNum,
		Questions:          reviewQuestions,
		Mode:               mode,
		SelfReview:         selfReview,
		JiraTicket:         jiraTicket,
		JiraContext:        jiraContext,
		PreviousReviews:    previousReviews,
		AcknowledgedIssues: acknowledgedIssues,
		SpecContent:        specContent,
		SpecPath:           specPath,
		Flags:              flags,
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

	model := os.Getenv("CLAUDE_MODEL")
	if model == "" {
		model = "claude-opus-4-6"
	}
	metrics := stats.MetricsSummary(model, ev.User, ev.Channel)

	if selfReview {
		dmUser(api, notifyUserID, fmt.Sprintf("*%s review for <%s>:*\n\n%s", modeLabel, prURL, review))
		_ = api.RemoveReaction("eyes", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		_ = api.AddReaction("white_check_mark", slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> sent via DM only.%s\nUsage: %s\n\n%s", modeLabel, prURL, scoreMsg, stats, metrics))
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

	dmUser(api, notifyUserID, fmt.Sprintf("Done! %s review for <%s> posted on GitHub and in <#%s>.%s\nUsage: %s\n\n%s", modeLabel, prURL, channelID, scoreMsg, stats, metrics))
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

func (c *RepoCache) CreateWorktree(ctx context.Context, owner, repo, prNum string) (string, error) {
	gd := c.gitDir(owner, repo)
	tmpDir, err := os.MkdirTemp("", "pr-review-wt-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	ref := "refs/prs/" + prNum
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gd, "worktree", "add", "--detach", tmpDir, ref)
	if out, err := cmd.CombinedOutput(); err != nil {
		os.RemoveAll(tmpDir)
		return "", fmt.Errorf("create worktree: %s", string(out))
	}
	log.Printf("repo-cache: created worktree at %s for %s/%s#%s", tmpDir, owner, repo, prNum)
	return tmpDir, nil
}

func (c *RepoCache) RemoveWorktree(ctx context.Context, owner, repo, wtDir string) {
	gd := c.gitDir(owner, repo)
	_ = exec.CommandContext(ctx, "git", "--git-dir", gd, "worktree", "remove", "--force", wtDir).Run()
	os.RemoveAll(wtDir)
	log.Printf("repo-cache: removed worktree %s", wtDir)
}

// --- Session Store ---

type SessionStore struct {
	path string
	mu   sync.Mutex
	data map[string]string // PR URL → merger session ID
}

func NewSessionStore() *SessionStore {
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatalf("session-store: cannot determine home dir: %v", err)
	}
	path := filepath.Join(home, ".pr-review-cache", "sessions.json")
	s := &SessionStore{path: path, data: make(map[string]string)}
	if raw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(raw, &s.data)
	}
	log.Printf("session-store: %s (%d sessions)", path, len(s.data))
	return s
}

func (s *SessionStore) Get(prURL string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data[prURL]
}

func (s *SessionStore) Set(prURL, sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data[prURL] = sessionID
	raw, _ := json.Marshal(s.data)
	_ = os.WriteFile(s.path, raw, 0o644)
	log.Printf("session-store: saved %s → %s", prURL, sessionID)
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

func nonMergeFiles(ctx context.Context, gitDir, mergeBase, prRef string) ([]string, error) {
	cmd := exec.CommandContext(ctx, "git", "--git-dir", gitDir,
		"log", "--no-merges", "--name-only", "--format=",
		mergeBase+".."+prRef)
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("non-merge files: %w", err)
	}
	seen := make(map[string]bool)
	var files []string
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !seen[line] {
			seen[line] = true
			files = append(files, line)
		}
	}
	return files, nil
}

func buildSmartDiff(ctx context.Context, gitDir, mergeBase, prRef string) (string, error) {
	const maxChars = 120_000

	files, filesErr := nonMergeFiles(ctx, gitDir, mergeBase, prRef)
	if filesErr != nil {
		log.Printf("repo-cache: non-merge files failed, falling back to full diff: %v", filesErr)
	}

	args := []string{"--git-dir", gitDir, "diff", mergeBase, prRef}
	if filesErr == nil && len(files) > 0 {
		args = append(args, "--")
		args = append(args, files...)
		log.Printf("repo-cache: scoping diff to %d file(s) from non-merge commits", len(files))
	}
	cmd := exec.CommandContext(ctx, "git", args...)
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

	if req.Mode == ModeQuick {
		result, err := runQuickReview(ctx, req.PRURL, req.Diff, contextBlock, questionsStr, stats)
		if err != nil {
			return "", nil, stats, err
		}
		score, scoreResp, scoreErr := runScorer(ctx, nil, req.Diff, req.SpecContent, req.AcknowledgedIssues)
		if scoreErr == nil {
			stats.Add(scoreResp)
			result = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + result
			return result, &score, stats, nil
		}
		log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
		return result, nil, stats, nil
	}

	if req.Mode == ModeReReview {
		if sessionID := sessionStore.Get(req.PRURL); sessionID != "" {
			result, mergeResp, err := runReReview(ctx, api, notifyUserID, req, sessionID, stats)
			if err == nil {
				stats.Add(mergeResp)
				sessionStore.Set(req.PRURL, mergeResp.SessionID)
				score, scoreResp, scoreErr := runScorer(ctx, nil, req.Diff, req.SpecContent, req.AcknowledgedIssues)
				if scoreErr == nil {
					stats.Add(scoreResp)
					result = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + result
					return result, &score, stats, nil
				}
				log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
				return result, nil, stats, nil
			}
			log.Printf("re-review session resume failed for %s: %v — falling back to delta review", req.PRURL, err)
			dmUser(api, notifyUserID, fmt.Sprintf("Session resume failed, running delta review for <%s>...", req.PRURL))
		} else {
			log.Printf("re-review: no stored session for %s — running delta review", req.PRURL)
			dmUser(api, notifyUserID, fmt.Sprintf("No previous session found for <%s>, running delta review...", req.PRURL))
		}
		result, deltaResp, err := runDeltaReReview(ctx, req, contextBlock, questionsStr)
		if err != nil {
			return "", nil, stats, err
		}
		stats.Add(deltaResp)
		if deltaResp.SessionID != "" {
			sessionStore.Set(req.PRURL, deltaResp.SessionID)
		}
		score, scoreResp, scoreErr := runScorer(ctx, nil, req.Diff, req.SpecContent, req.AcknowledgedIssues)
		if scoreErr == nil {
			stats.Add(scoreResp)
			result = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + result
			return result, &score, stats, nil
		}
		log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
		return result, nil, stats, nil
	}

	allAgents, err := loadAgents()
	if err != nil {
		return "", nil, stats, fmt.Errorf("load agents: %w", err)
	}
	projCfg, cfgErr := loadProjectsConfig()
	if cfgErr != nil {
		log.Printf("warning: %v — using all agents", cfgErr)
	}
	agents := filterAgentsByProject(allAgents, projCfg.agentsForRepo(req.Owner, req.Repo))
	agents = filterAgents(agents, req.Flags)

	agentWorkDir := ""
	if req.PRNum != "" {
		wt, wtErr := repoCache.CreateWorktree(ctx, req.Owner, req.Repo, req.PRNum)
		if wtErr != nil {
			log.Printf("worktree: failed, agents run without repo context: %v", wtErr)
		} else {
			agentWorkDir = wt
			defer repoCache.RemoveWorktree(ctx, req.Owner, req.Repo, wt)
		}
	}

	data := promptData{
		ModePreamble: modePreamble(req.Mode),
		PRURL:        req.PRURL,
		ContextBlock: contextBlock,
		QuestionsStr: questionsStr,
		Diff:         req.Diff,
	}

	reviews := make([]string, len(agents))
	perspectiveScores := make([]PerspectiveScore, len(agents))
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error

	for i, a := range agents {
		wg.Add(1)
		go func(idx int, agent agentFile) {
			defer wg.Done()
			prompt, renderErr := renderAgent(agent, data)
			if renderErr != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = renderErr
				}
				mu.Unlock()
				return
			}
			prompt += scoreSuffix
			agentModel := agent.model
			if agentModel == "" {
				agentModel = "default"
			}
			log.Printf("agent %s: starting %s review for %s (model=%s, max-turns=%d)", agent.name, req.Mode, req.PRURL, agentModel, agentMaxTurns)
			agentStart := time.Now()
			text, resp, err := runClaudeInDir(ctx, prompt, agentWorkDir, agent.model, agentMaxTurns)
			agentDur := time.Since(agentStart)
			if err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = fmt.Errorf("agent %s failed: %w", agent.name, err)
				}
				mu.Unlock()
				return
			}
			stats.Add(resp)
			stats.AddAgent(agent.name, resp.TotalCostUSD, agentDur)
			reviewText, ps := extractPerspectiveScore(agent.name, text)
			mu.Lock()
			reviews[idx] = fmt.Sprintf("## %s Review\n\n%s", strings.ToUpper(agent.name), reviewText)
			perspectiveScores[idx] = ps
			mu.Unlock()
			log.Printf("agent %s: done for %s (perspective: %d/100, $%.4f)", agent.name, req.PRURL, ps.Score, resp.TotalCostUSD)
		}(i, a)
	}
	wg.Wait()

	if firstErr != nil {
		return "", nil, stats, firstErr
	}

	dmUser(api, notifyUserID, fmt.Sprintf("All %d agents done. Running validator...", len(agents)))
	allReviews := strings.Join(reviews, "\n\n---\n\n")

	log.Printf("validator: starting for %s", req.PRURL)
	valStart := time.Now()
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
%s`, len(agents), req.Diff, allReviews))
	if err != nil {
		return "", nil, stats, fmt.Errorf("validator failed: %w", err)
	}
	stats.Add(valResp)
	stats.AddAgent("validator", valResp.TotalCostUSD, time.Since(valStart))
	log.Printf("validator: done for %s ($%.4f)", req.PRURL, valResp.TotalCostUSD)

	dmUser(api, notifyUserID, "Validator done. Scoring + merging...")

	var (
		score      ScoreResult
		scoreResp  claudeResponse
		scoreErr   error
		merged     string
		mergeResp  claudeResponse
		mergeErr   error
		scoreMerge sync.WaitGroup
	)

	scoreMerge.Add(2)
	go func() {
		defer scoreMerge.Done()
		log.Printf("scorer: starting for %s", req.PRURL)
		scorerStart := time.Now()
		score, scoreResp, scoreErr = runScorer(ctx, perspectiveScores, req.Diff, req.SpecContent, req.AcknowledgedIssues)
		if scoreErr != nil {
			log.Printf("scorer: failed for %s: %v", req.PRURL, scoreErr)
		} else {
			stats.Add(scoreResp)
			stats.AddAgent("scorer", scoreResp.TotalCostUSD, time.Since(scorerStart))
			log.Printf("scorer: done for %s (score: %d/100, $%.4f)", req.PRURL, score.Overall, scoreResp.TotalCostUSD)
		}
	}()
	go func() {
		defer scoreMerge.Done()
		log.Printf("merger: starting for %s", req.PRURL)
		mergerStart := time.Now()
		merged, mergeResp, mergeErr = runMerger(ctx, allReviews, validated, req.Mode, req.SpecContent, req.AcknowledgedIssues)
		if mergeErr != nil {
			return
		}
		stats.Add(mergeResp)
		stats.AddAgent("merger", mergeResp.TotalCostUSD, time.Since(mergerStart))
		log.Printf("merger: done for %s ($%.4f)", req.PRURL, mergeResp.TotalCostUSD)
	}()
	scoreMerge.Wait()

	if mergeErr != nil {
		return "", nil, stats, mergeErr
	}

	if mergeResp.SessionID != "" {
		sessionStore.Set(req.PRURL, mergeResp.SessionID)
	}

	if scoreErr == nil {
		merged = formatScoreHeader(score, req.PreviousReviews) + "\n\n---\n\n" + merged
		return merged, &score, stats, nil
	}

	return merged, nil, stats, nil
}

func runReReview(ctx context.Context, api SlackAPI, notifyUserID string, req ReviewRequest, sessionID string, stats *UsageStats) (string, claudeResponse, error) {
	dmUser(api, notifyUserID, fmt.Sprintf("Resuming previous review session for <%s>...", req.PRURL))
	log.Printf("re-review: resuming session %s for %s", sessionID, req.PRURL)

	var ackNote string
	if req.AcknowledgedIssues != "" {
		ackNote = fmt.Sprintf("\n\n## Acknowledged Issues\nThese were explicitly acknowledged by the author — do NOT re-flag or penalize:\n\n%s", req.AcknowledgedIssues)
	}

	prompt := fmt.Sprintf(`You are continuing your previous code review of this PR: %s
The author has pushed changes since your last review. Below is the COMPLETE CURRENT DIFF of the PR (not just what changed since last review).

Your job:
1. Compare this diff against the issues you raised in your previous review
2. For each previous issue: state whether it was RESOLVED, PARTIALLY RESOLVED, or STILL PRESENT
3. Flag any NEW issues introduced in the updated code
4. Provide an updated merged review in the same format as before (Summary, Critical Issues, Design Concerns, Suggestions, What's Good, Verdict)
5. If all critical issues are resolved and no new ones appeared, recommend approval

IMPORTANT: Do NOT include a Quality Score section or score table — scoring is handled separately.
Do NOT add meta-commentary about the diff or your process. Go straight into the review.
%s
## Updated Diff
`+"```diff\n%s\n```", req.PRURL, ackNote, req.Diff)

	text, resp, err := runClaudeWithSession(ctx, prompt, sessionID)
	if err != nil {
		return "", claudeResponse{}, err
	}
	log.Printf("re-review: session resume done for %s ($%.4f)", req.PRURL, resp.TotalCostUSD)
	return text, resp, nil
}

func runDeltaReReview(ctx context.Context, req ReviewRequest, contextBlock, questionsStr string) (string, claudeResponse, error) {
	var ackNote string
	if req.AcknowledgedIssues != "" {
		ackNote = fmt.Sprintf("\n\n## Acknowledged Issues\nThese were explicitly acknowledged by the author — do NOT re-flag or penalize:\n\n%s", req.AcknowledgedIssues)
	}

	prompt := fmt.Sprintf(`You are a code review assistant performing a RE-REVIEW of %s. This PR was previously reviewed and the author has pushed updates.

Below you have the previous review discussion and the COMPLETE CURRENT DIFF. Your job:
1. Identify which issues from previous reviews were RESOLVED, PARTIALLY RESOLVED, or STILL PRESENT
2. Flag any NEW issues in the current diff
3. Assess how thoroughly the author addressed feedback — this should positively influence your verdict
4. If all critical issues are resolved and no new critical issues appeared, recommend approval
5. Output a full review: Summary, Previous Issues Status, New Issues (if any), Verdict

IMPORTANT: Do NOT include a Quality Score section or score table — scoring is handled separately.

Be specific about what changed. Reference files and lines.
%s
%s
%s

## Current Diff
`+"```diff\n%s\n```", req.PRURL, contextBlock, ackNote, questionsStr, req.Diff)

	log.Printf("delta-re-review: starting for %s", req.PRURL)
	text, resp, err := runClaude(ctx, prompt)
	if err != nil {
		return "", claudeResponse{}, fmt.Errorf("delta re-review failed: %w", err)
	}
	log.Printf("delta-re-review: done for %s ($%.4f)", req.PRURL, resp.TotalCostUSD)
	return text, resp, nil
}

const diffScopeRule = `IMPORTANT: Only raise issues about code that appears in the diff below. Do not speculate about code outside the diff, do not flag pre-existing issues in unchanged code, and do not hallucinate file contents you cannot see. Every finding must quote the exact code from the diff. If the diff is clean, say so.

`

func modePreamble(mode ReviewMode) string {
	base := diffScopeRule
	switch mode {
	case ModeReReview:
		return base + `NOTE: This is a RE-REVIEW. This PR has been reviewed before by an automated system. Focus on:
- Whether previously identified issues have been addressed
- Any new issues introduced since the last review
- Remaining concerns that still need attention
Do not repeat feedback that has clearly been addressed.

`
	case ModeFinal:
		return base + `NOTE: This is a FINAL REVIEW before merge. Err on the side of approval:
- Only flag truly critical/blocking issues (bugs, security, data loss)
- Mention nice-to-haves and nit picks as OPTIONAL/non-blocking
- If the code is generally sound and functional, recommend approval

`
	default:
		return base
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
Do NOT include a Quality Score section or score table — scoring is handled separately.

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

func runScorer(ctx context.Context, perspectiveScores []PerspectiveScore, diff, specContent, acknowledgedIssues string) (ScoreResult, claudeResponse, error) {
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

	perspectiveBlock := ""
	if len(perspectiveScores) > 0 {
		var sb strings.Builder
		sb.WriteString("\n## Reviewer Perspective Scores\n")
		sb.WriteString("The following scores were given by independent review agents. Evaluate the merit of each score — consider whether the reviewer's rationale is sound, whether their confidence level is justified, and whether they may have over- or under-weighted certain aspects. Use these as informed inputs, not as votes to average.\n\n")
		for _, ps := range perspectiveScores {
			if ps.Score > 0 {
				fmt.Fprintf(&sb, "- **%s**: %d/100 (confidence: %d/100) — %s\n", ps.Agent, ps.Score, ps.Confidence, ps.Rationale)
			}
		}
		perspectiveBlock = sb.String()
	}

	prompt := fmt.Sprintf(`You are a code quality scorer. Evaluate this PR diff and produce a comprehensive quality score.
%s
Score each dimension 0-10 (10 = excellent, 0 = critical problems):
- correctness: Logic errors, bugs, edge cases, error handling
- security: Vulnerabilities, data leaks, auth issues, injection risks
- design: Architecture, complexity, naming, readability
- go_quality: Idiomatic Go, stdlib usage, concurrency patterns, error wrapping
- testing: Test presence and quality, edge case coverage
- production_readiness: Logging, monitoring, graceful degradation%s

If a dimension has no relevant code in the diff (e.g., no security-sensitive changes), score 8-9 reflecting no risk introduced.

When reviewer perspective scores are provided, critically evaluate each one:
- A high-confidence score from a domain-relevant reviewer (e.g., go-expert on Go code) carries more weight
- A low-confidence score or one from a less relevant perspective should be weighted less
- If a reviewer's rationale contradicts the actual diff, disregard their score
- Your dimensional scores should reflect your own analysis informed by — but not averaging — the perspective scores
%s%s
Respond with ONLY this JSON object, no markdown fences, no other text:
{"correctness":N,"security":N,"design":N,"go_quality":N,"testing":N,"production_readiness":N%s,"overall":N,"summary":"one sentence"}

`+"```diff\n%s\n```", perspectiveBlock, specDimension, specBlock, ackBlock, specJSON, diff)

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
- Reference file names and line numbers where relevant
- Do NOT include a Quality Score section, score table, or numerical scores — scoring is handled separately%s%s
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

func runClaude(ctx context.Context, prompt string) (string, claudeResponse, error) {
	return runClaudeOpts(ctx, prompt, "", "", 0)
}

func runClaudeInDir(ctx context.Context, prompt, workDir, modelOverride string, maxTurns int) (string, claudeResponse, error) {
	return runClaudeOpts(ctx, prompt, "", workDir, maxTurns, modelOverride)
}

func runClaudeWithSession(ctx context.Context, prompt, resumeSessionID string) (string, claudeResponse, error) {
	return runClaudeOpts(ctx, prompt, resumeSessionID, "", 0)
}

func runClaudeOpts(ctx context.Context, prompt, resumeSessionID, workDir string, maxTurns int, modelOverride ...string) (string, claudeResponse, error) {
	if workDir == "" {
		workDir = os.TempDir()
	}
	model := ""
	if len(modelOverride) > 0 && modelOverride[0] != "" {
		model = resolveModel(modelOverride[0])
	}
	if model == "" {
		model = os.Getenv("CLAUDE_MODEL")
	}
	if model == "" {
		model = "claude-opus-4-6"
	}
	args := []string{"-p", "Follow the instructions provided on stdin.", "--output-format", "json", "--model", model}
	if maxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(maxTurns))
	}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}
	cmd := exec.CommandContext(ctx, "claude", args...)
	cmd.Dir = workDir
	cmd.Stdin = strings.NewReader(prompt)
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

var modelAliases = map[string]string{
	"opus":   "claude-opus-4-6",
	"sonnet": "claude-sonnet-4-6",
	"haiku":  "claude-haiku-4-5-20251001",
}

func resolveModel(name string) string {
	if full, ok := modelAliases[name]; ok {
		return full
	}
	return name
}
