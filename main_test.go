package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
)

type reactionCall struct {
	action string // "add" or "remove"
	name   string
}

type messageCall struct {
	channel string
	text    string
	threadTS string
}

type mockSlack struct {
	mu        sync.Mutex
	reactions []reactionCall
	messages  []messageCall
}

func (m *mockSlack) AddReaction(name string, item slack.ItemRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reactions = append(m.reactions, reactionCall{action: "add", name: name})
	return nil
}

func (m *mockSlack) RemoveReaction(name string, item slack.ItemRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reactions = append(m.reactions, reactionCall{action: "remove", name: name})
	return nil
}

func (m *mockSlack) PostMessage(channelID string, options ...slack.MsgOption) (string, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.messages = append(m.messages, messageCall{channel: channelID})
	return "", "", nil
}

func (m *mockSlack) OpenConversation(params *slack.OpenConversationParameters) (*slack.Channel, bool, bool, error) {
	return &slack.Channel{}, false, false, nil
}

func (m *mockSlack) GetConversationHistory(params *slack.GetConversationHistoryParameters) (*slack.GetConversationHistoryResponse, error) {
	return &slack.GetConversationHistoryResponse{}, nil
}

func (m *mockSlack) GetConversationReplies(params *slack.GetConversationRepliesParameters) ([]slack.Message, bool, string, error) {
	return nil, false, "", nil
}

func TestPostError_RemovesEyesAndAddsX(t *testing.T) {
	mock := &mockSlack{}
	ev := &slackevents.MessageEvent{
		Channel:   "C123",
		TimeStamp: "1234567890.123456",
	}

	postError(mock, ev, "https://github.com/org/repo/pull/1", "C123", "U999", errors.New("something broke"))

	wantReactions := []reactionCall{
		{action: "remove", name: "claude-intesifies"},
		{action: "add", name: "qumulo-oh-no"},
	}

	if len(mock.reactions) != len(wantReactions) {
		t.Fatalf("got %d reactions, want %d: %+v", len(mock.reactions), len(wantReactions), mock.reactions)
	}
	for i, want := range wantReactions {
		got := mock.reactions[i]
		if got != want {
			t.Errorf("reaction[%d] = %+v, want %+v", i, got, want)
		}
	}
}

func TestPostError_PostsThreadReply(t *testing.T) {
	mock := &mockSlack{}
	ev := &slackevents.MessageEvent{
		Channel:   "C123",
		TimeStamp: "1234567890.123456",
	}

	postError(mock, ev, "https://github.com/org/repo/pull/1", "C123", "U999", errors.New("boom"))

	var channelPost bool
	var dmPost bool
	for _, msg := range mock.messages {
		if msg.channel == "C123" {
			channelPost = true
		}
		if msg.channel == "U999" {
			dmPost = true
		}
	}
	if !channelPost {
		t.Error("expected thread reply in channel C123")
	}
	if !dmPost {
		t.Error("expected DM to user U999")
	}
}

func TestPostCancelled_RemovesEyesAndAddsNoEntry(t *testing.T) {
	mock := &mockSlack{}
	ev := &slackevents.MessageEvent{
		Channel:   "C123",
		TimeStamp: "1234567890.123456",
	}

	postCancelled(mock, ev, "https://github.com/org/repo/pull/1", "C123", "U999")

	wantReactions := []reactionCall{
		{action: "remove", name: "claude-intesifies"},
		{action: "add", name: "no_entry_sign"},
	}

	if len(mock.reactions) != len(wantReactions) {
		t.Fatalf("got %d reactions, want %d: %+v", len(mock.reactions), len(wantReactions), mock.reactions)
	}
	for i, want := range wantReactions {
		got := mock.reactions[i]
		if got != want {
			t.Errorf("reaction[%d] = %+v, want %+v", i, got, want)
		}
	}
}

func TestPostCancelled_PostsThreadAndDM(t *testing.T) {
	mock := &mockSlack{}
	ev := &slackevents.MessageEvent{
		Channel:   "C123",
		TimeStamp: "1234567890.123456",
	}

	postCancelled(mock, ev, "https://github.com/org/repo/pull/1", "C123", "U999")

	var channelPost bool
	var dmPost bool
	for _, msg := range mock.messages {
		if msg.channel == "C123" {
			channelPost = true
		}
		if msg.channel == "U999" {
			dmPost = true
		}
	}
	if !channelPost {
		t.Error("expected thread reply in channel C123")
	}
	if !dmPost {
		t.Error("expected DM to user U999")
	}
}

func TestCancelReview_ReturnsTrueAndCallsCancel(t *testing.T) {
	called := false
	trackReview("ts123", "https://github.com/org/repo/pull/1", func() { called = true })

	if !cancelReview("ts123") {
		t.Error("cancelReview should return true for tracked review")
	}
	if !called {
		t.Error("cancel func should have been called")
	}
	if cancelReview("ts123") {
		t.Error("cancelReview should return false after already cancelled")
	}
}

func TestCancelReview_ReturnsFalseForUnknown(t *testing.T) {
	if cancelReview("unknown") {
		t.Error("cancelReview should return false for unknown timestamp")
	}
}

func TestDmUser_PostsToUser(t *testing.T) {
	mock := &mockSlack{}
	dmUser(mock, "U123", "hello")

	if len(mock.messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(mock.messages))
	}
	if mock.messages[0].channel != "U123" {
		t.Errorf("DM sent to %s, want U123", mock.messages[0].channel)
	}
}

func TestPreviousSpecPattern_ExtractsPath(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantNil bool
	}{
		{
			name:  "repo-relative path",
			input: "some review text\n\n<!-- spec: docs/SPEC.md -->",
			want:  "docs/SPEC.md",
		},
		{
			name:  "absolute path",
			input: "review\n<!-- spec: /Users/dan/specs/api.md -->",
			want:  "/Users/dan/specs/api.md",
		},
		{
			name:  "multiple specs takes last",
			input: "<!-- spec: old/spec.md -->\nstuff\n<!-- spec: new/spec.md -->",
			want:  "new/spec.md",
		},
		{
			name:    "no spec tag",
			input:   "just a normal review comment with no metadata",
			wantNil: true,
		},
		{
			name:    "empty string",
			input:   "",
			wantNil: true,
		},
		{
			name:  "spec tag among other html comments",
			input: "<!-- something else -->\n<!-- spec: path/to/spec.md -->\n<!-- another -->",
			want:  "path/to/spec.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := previousSpecPattern.FindAllStringSubmatch(tt.input, -1)
			if tt.wantNil {
				if len(matches) != 0 {
					t.Errorf("expected no matches, got %v", matches)
				}
				return
			}
			if len(matches) == 0 {
				t.Fatal("expected matches, got none")
			}
			got := matches[len(matches)-1][1]
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpecMetadataAppended(t *testing.T) {
	review := "## Quality Score: 85/100\n\n---\n\n## Summary\nLooks good."
	specPath := "docs/API-SPEC.md"

	result := review + fmt.Sprintf("\n\n<!-- spec: %s -->", specPath)

	matches := previousSpecPattern.FindAllStringSubmatch(result, -1)
	if len(matches) == 0 {
		t.Fatal("spec metadata not found in review output")
	}
	got := matches[0][1]
	if got != specPath {
		t.Errorf("extracted spec %q, want %q", got, specPath)
	}
}

func TestSpecMetadata_NotAppendedWithoutSpec(t *testing.T) {
	review := "## Summary\nAll good."
	specPath := ""

	result := review
	if specPath != "" {
		result += fmt.Sprintf("\n\n<!-- spec: %s -->", specPath)
	}

	matches := previousSpecPattern.FindAllStringSubmatch(result, -1)
	if len(matches) != 0 {
		t.Error("spec metadata should not be present when no spec used")
	}
}

func TestReviewRequestPattern(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"<@U123> review https://github.com/org/repo/pull/1", true},
		{"<@U123> Review https://github.com/org/repo/pull/1", true},
		{"<@U123> please review https://github.com/org/repo/pull/1 --quick", true},
		{"<@U123> can you review this? https://github.com/org/repo/pull/1", true},
		{"<@U123> REVIEW https://github.com/org/repo/pull/1", true},
		{"<@U123> https://github.com/org/repo/pull/1", false},
		{"<@U123> hey check this out https://github.com/org/repo/pull/1", false},
		{"<@U123> what do you think?", false},
		{"<@U123> reviewed this already", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := reviewRequestPattern.MatchString(tt.input)
			if got != tt.want {
				t.Errorf("reviewRequestPattern.MatchString(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestParseSpecPath(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"https://github.com/org/repo/pull/1 --spec docs/SPEC.md", "docs/SPEC.md"},
		{"https://github.com/org/repo/pull/1 --spec /abs/path.md --re-review", "/abs/path.md"},
		{"https://github.com/org/repo/pull/1", ""},
		{"https://github.com/org/repo/pull/1 --re-review", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseSpecPath(tt.input)
			if got != tt.want {
				t.Errorf("parseSpecPath(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestAckPattern_MatchesExpected(t *testing.T) {
	shouldMatch := []string{
		"ack",
		"Ack, will fix in follow-up",
		"acknowledged",
		"Acknowledged — this is by design",
		"won't fix",
		"wont fix — intentional complexity",
		"wontfix",
		"This is intentional",
		"by design",
		"noted, will address later",
		"accepted",
		"will fix later",
		"will address later",
		"tracking in PROJ-123",
		"known issue",
		"out of scope for this PR",
		"deferred to next sprint",
	}

	for _, input := range shouldMatch {
		if !ackPattern.MatchString(input) {
			t.Errorf("ackPattern should match %q", input)
		}
	}
}

func TestAckPattern_RejectsNonAck(t *testing.T) {
	shouldNotMatch := []string{
		"this looks like a bug",
		"please fix this",
		"I disagree with this approach",
		"can you explain why?",
		"LGTM",
		"nice work",
		"needs more tests",
		"what about edge cases?",
	}

	for _, input := range shouldNotMatch {
		if ackPattern.MatchString(input) {
			t.Errorf("ackPattern should NOT match %q", input)
		}
	}
}

func TestIsReviewActive_DetectsByPRURL(t *testing.T) {
	prURL := "https://github.com/org/repo/pull/42"
	trackReview("ts-active-1", prURL, func() {})
	defer untrackReview("ts-active-1", prURL)

	if !isReviewActive(prURL) {
		t.Error("isReviewActive should return true for tracked PR")
	}
	if isReviewActive("https://github.com/org/repo/pull/99") {
		t.Error("isReviewActive should return false for untracked PR")
	}
}

func TestParallelTracking_IndependentReviews(t *testing.T) {
	pr1 := "https://github.com/org/repo/pull/1"
	pr2 := "https://github.com/org/repo/pull/2"

	var called1, called2 bool
	trackReview("ts-par-1", pr1, func() { called1 = true })
	trackReview("ts-par-2", pr2, func() { called2 = true })

	cancelReview("ts-par-1")
	if !called1 {
		t.Error("cancel should have fired for PR 1")
	}
	if called2 {
		t.Error("cancel should NOT have fired for PR 2")
	}

	untrackReview("ts-par-2", pr2)
}

func TestCancelReview_CancelsAllInSameMessage(t *testing.T) {
	pr1 := "https://github.com/org/repo/pull/10"
	pr2 := "https://github.com/org/repo/pull/20"
	ts := "ts-multi-pr"

	var called1, called2 bool
	trackReview(ts, pr1, func() { called1 = true })
	trackReview(ts, pr2, func() { called2 = true })

	cancelReview(ts)
	if !called1 {
		t.Error("cancel should fire for PR 1 in same message")
	}
	if !called2 {
		t.Error("cancel should fire for PR 2 in same message")
	}
}

func TestClassifyFile(t *testing.T) {
	tests := []struct {
		path string
		want filePriority
	}{
		{"internal/server/handler.go", prioImpl},
		{"cmd/app/main.go", prioImpl},
		{"README.md", prioImpl},
		{"internal/server/handler_test.go", prioTest},
		{"tests/integration/api_test.go", prioTest},
		{"src/components/Button.test.tsx", prioTest},
		{"__tests__/utils.test.js", prioTest},
		{"pkg/store/testdata/fixture.json", prioTest},
		{"go.mod", prioConfig},
		{"package.json", prioConfig},
		{"Dockerfile", prioConfig},
		{"deploy/values.yaml", prioConfig},
		{"Taskfile.yaml", prioConfig},
		{"vendor/github.com/pkg/errors/errors.go", prioGenerated},
		{"go.sum", prioGenerated},
		{"package-lock.json", prioGenerated},
		{"api/v1/types.pb.go", prioGenerated},
		{"internal/generated/schema.gen.go", prioGenerated},
		{"node_modules/react/index.js", prioGenerated},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			got := classifyFile(tt.path)
			if got != tt.want {
				t.Errorf("classifyFile(%q) = %d, want %d", tt.path, got, tt.want)
			}
		})
	}
}

func TestSplitDiffByFile(t *testing.T) {
	diff := `diff --git a/main.go b/main.go
index abc..def 100644
--- a/main.go
+++ b/main.go
@@ -1,3 +1,4 @@
 package main
+import "fmt"
diff --git a/util.go b/util.go
index 111..222 100644
--- a/util.go
+++ b/util.go
@@ -5,2 +5,3 @@
 func helper() {
+	return
 }
`
	result := splitDiffByFile(diff)

	if len(result) != 2 {
		t.Fatalf("got %d files, want 2", len(result))
	}
	if _, ok := result["main.go"]; !ok {
		t.Error("missing main.go in split result")
	}
	if _, ok := result["util.go"]; !ok {
		t.Error("missing util.go in split result")
	}
	if !strings.Contains(result["main.go"], `import "fmt"`) {
		t.Error("main.go diff should contain the added import")
	}
	if !strings.Contains(result["util.go"], "return") {
		t.Error("util.go diff should contain the added return")
	}
}

func TestSplitDiffByFile_Empty(t *testing.T) {
	result := splitDiffByFile("")
	if len(result) != 0 {
		t.Errorf("empty diff should produce 0 files, got %d", len(result))
	}
}

func TestHumanSize(t *testing.T) {
	tests := []struct {
		input int
		want  string
	}{
		{500, "500 chars"},
		{999, "999 chars"},
		{1000, "1.0k chars"},
		{1500, "1.5k chars"},
		{80000, "80.0k chars"},
	}
	for _, tt := range tests {
		got := humanSize(tt.input)
		if got != tt.want {
			t.Errorf("humanSize(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestLoadAgents_DiscoversFiles(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "alpha.md"), []byte("Review {{.PRURL}}"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.md"), []byte("Check {{.Diff}}"), 0o644)
	os.WriteFile(filepath.Join(dir, "not-an-agent.txt"), []byte("ignored"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0].name != "alpha" {
		t.Errorf("agents[0].name = %q, want alpha", agents[0].name)
	}
	if agents[1].name != "beta" {
		t.Errorf("agents[1].name = %q, want beta", agents[1].name)
	}
}

func TestLoadAgents_EmptyDirErrors(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	_, err := loadAgents()
	if err == nil {
		t.Fatal("loadAgents should error on empty dir")
	}
	if !strings.Contains(err.Error(), "no .md agent files") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestLoadAgents_SkipsSubdirs(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.Mkdir(filepath.Join(dir, "subdir.md"), 0o755)
	os.WriteFile(filepath.Join(dir, "real.md"), []byte("{{.PRURL}}"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(agents))
	}
	if agents[0].name != "real" {
		t.Errorf("name = %q, want real", agents[0].name)
	}
}

func TestRenderAgent(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "test.md"), []byte("Review {{.PRURL}} with mode {{.ModePreamble}}diff:\n{{.Diff}}"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}

	data := promptData{
		ModePreamble: "FINAL ",
		PRURL:        "https://github.com/org/repo/pull/42",
		Diff:         "+added line",
	}

	result, err := renderAgent(agents[0], data)
	if err != nil {
		t.Fatalf("renderAgent: %v", err)
	}
	if !strings.Contains(result, "https://github.com/org/repo/pull/42") {
		t.Error("rendered prompt should contain PR URL")
	}
	if !strings.Contains(result, "FINAL ") {
		t.Error("rendered prompt should contain mode preamble")
	}
	if !strings.Contains(result, "+added line") {
		t.Error("rendered prompt should contain diff")
	}
}

func TestLoadAgents_InvalidTemplate(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "bad.md"), []byte("{{.Unclosed"), 0o644)

	_, err := loadAgents()
	if err == nil {
		t.Fatal("loadAgents should error on invalid template")
	}
	if !strings.Contains(err.Error(), "parse agent template") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestParseMode(t *testing.T) {
	tests := []struct {
		input        string
		wantMode     ReviewMode
		wantExplicit bool
	}{
		{"https://github.com/org/repo/pull/1", ModeInitial, false},
		{"https://github.com/org/repo/pull/1 --initial", ModeInitial, true},
		{"https://github.com/org/repo/pull/1 --quick", ModeQuick, true},
		{"https://github.com/org/repo/pull/1 --re-review", ModeReReview, true},
		{"https://github.com/org/repo/pull/1 --final", ModeFinal, true},
		{"review --initial --bare-necessities", ModeInitial, true},
		{"just some text", ModeInitial, false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			mode, explicit := parseMode(tt.input)
			if mode != tt.wantMode {
				t.Errorf("mode = %q, want %q", mode, tt.wantMode)
			}
			if explicit != tt.wantExplicit {
				t.Errorf("explicit = %v, want %v", explicit, tt.wantExplicit)
			}
		})
	}
}

func TestModePreamble(t *testing.T) {
	if !strings.Contains(modePreamble(ModeInitial), "Only raise issues") {
		t.Error("initial mode should contain diff scope rule")
	}
	if !strings.Contains(modePreamble(ModeQuick), "Only raise issues") {
		t.Error("quick mode should contain diff scope rule")
	}
	if !strings.Contains(modePreamble(ModeReReview), "RE-REVIEW") {
		t.Error("re-review preamble should contain RE-REVIEW")
	}
	if !strings.Contains(modePreamble(ModeFinal), "FINAL REVIEW") {
		t.Error("final preamble should contain FINAL REVIEW")
	}
	for _, mode := range []ReviewMode{ModeInitial, ModeQuick, ModeReReview, ModeFinal} {
		if !strings.Contains(modePreamble(mode), "Only raise issues") {
			t.Errorf("mode %q missing diff scope rule", mode)
		}
	}
}

func TestLoadAgents_SortedAlphabetically(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "zebra.md"), []byte("{{.PRURL}}"), 0o644)
	os.WriteFile(filepath.Join(dir, "alpha.md"), []byte("{{.PRURL}}"), 0o644)
	os.WriteFile(filepath.Join(dir, "middle.md"), []byte("{{.PRURL}}"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}
	if agents[0].name != "alpha" || agents[1].name != "middle" || agents[2].name != "zebra" {
		t.Errorf("agents not sorted: %s, %s, %s", agents[0].name, agents[1].name, agents[2].name)
	}
}

func TestLoadAgents_RealAgentsDir(t *testing.T) {
	old := agentsDir
	agentsDir = "agents"
	defer func() { agentsDir = old }()

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents on real agents/ dir: %v", err)
	}
	if len(agents) < 1 {
		t.Fatal("expected at least 1 agent in agents/ dir")
	}

	wantNames := map[string]bool{
		"correctness": false,
		"go-expert":   false,
		"pragmatic":   false,
	}
	for _, a := range agents {
		if _, ok := wantNames[a.name]; ok {
			wantNames[a.name] = true
		}
	}
	for name, found := range wantNames {
		if !found {
			t.Errorf("expected agent %q not found in agents/ dir", name)
		}
	}

	data := promptData{
		ModePreamble: "TEST ",
		PRURL:        "https://github.com/org/repo/pull/1",
		ContextBlock: "context here",
		QuestionsStr: "questions here",
		Diff:         "+added\n-removed",
	}
	for _, a := range agents {
		rendered, err := renderAgent(a, data)
		if err != nil {
			t.Errorf("renderAgent(%s): %v", a.name, err)
			continue
		}
		if len(rendered) == 0 {
			t.Errorf("agent %s: rendered output is empty", a.name)
		}
		if strings.Contains(rendered, "{{.") {
			t.Errorf("agent %s: unrendered template variable in output", a.name)
		}
	}
}

func TestRenderAgent_AllFields(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	tmplContent := "P={{.ModePreamble}} U={{.PRURL}} C={{.ContextBlock}} Q={{.QuestionsStr}} D={{.Diff}}"
	os.WriteFile(filepath.Join(dir, "full.md"), []byte(tmplContent), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}

	data := promptData{
		ModePreamble: "MODE",
		PRURL:        "URL",
		ContextBlock: "CTX",
		QuestionsStr: "QST",
		Diff:         "DIF",
	}
	result, err := renderAgent(agents[0], data)
	if err != nil {
		t.Fatalf("renderAgent: %v", err)
	}
	want := "P=MODE U=URL C=CTX Q=QST D=DIF"
	if result != want {
		t.Errorf("got %q, want %q", result, want)
	}
}

func TestRenderAgent_EmptyFields(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "empty.md"), []byte("start{{.ModePreamble}}{{.QuestionsStr}}end"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}

	result, err := renderAgent(agents[0], promptData{})
	if err != nil {
		t.Fatalf("renderAgent: %v", err)
	}
	if result != "startend" {
		t.Errorf("got %q, want %q", result, "startend")
	}
}

func TestAgentNames(t *testing.T) {
	agents := []agentFile{
		{name: "alpha"},
		{name: "beta"},
		{name: "gamma"},
	}
	got := agentNames(agents)
	if got != "alpha, beta, gamma" {
		t.Errorf("agentNames = %q, want %q", got, "alpha, beta, gamma")
	}
}

func TestAgentNames_Empty(t *testing.T) {
	got := agentNames(nil)
	if got != "" {
		t.Errorf("agentNames(nil) = %q, want empty", got)
	}
}

func TestParseFlags(t *testing.T) {
	tests := []struct {
		input string
		want  map[string]bool
	}{
		{"review https://github.com/org/repo/pull/1 --bare-necessities", map[string]bool{"bare-necessities": true}},
		{"review https://github.com/org/repo/pull/1 --bare-necessities --deep-dive", map[string]bool{"bare-necessities": true, "deep-dive": true}},
		{"review https://github.com/org/repo/pull/1", map[string]bool{}},
		{"review https://github.com/org/repo/pull/1 --quick", map[string]bool{}},
		{"review https://github.com/org/repo/pull/1 --self --quick --bare-necessities", map[string]bool{"bare-necessities": true}},
		{"review --initial --spec docs/SPEC.md --bare-necessities", map[string]bool{"bare-necessities": true}},
		{"review --re-review --final", map[string]bool{}},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := parseFlags(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("parseFlags(%q) = %v, want %v", tt.input, got, tt.want)
			}
			for k := range tt.want {
				if !got[k] {
					t.Errorf("parseFlags(%q) missing flag %q", tt.input, k)
				}
			}
		})
	}
}

func TestFilterAgents(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "necessity", flag: "bare-necessities"},
		{name: "deep", flag: "deep-dive"},
	}

	t.Run("no flags — only unflagged agents", func(t *testing.T) {
		got := filterAgents(agents, map[string]bool{})
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
		if got[0].name != "correctness" || got[1].name != "design" {
			t.Errorf("got %s, %s — want correctness, design", got[0].name, got[1].name)
		}
	})

	t.Run("bare-necessities flag — includes necessity", func(t *testing.T) {
		got := filterAgents(agents, map[string]bool{"bare-necessities": true})
		if len(got) != 3 {
			t.Fatalf("got %d agents, want 3", len(got))
		}
		names := agentNames(got)
		if !strings.Contains(names, "necessity") {
			t.Errorf("expected necessity in %s", names)
		}
		if strings.Contains(names, "deep") {
			t.Errorf("deep should not be included: %s", names)
		}
	})

	t.Run("both flags — all agents", func(t *testing.T) {
		got := filterAgents(agents, map[string]bool{"bare-necessities": true, "deep-dive": true})
		if len(got) != 4 {
			t.Fatalf("got %d agents, want 4", len(got))
		}
	})

	t.Run("nil flags — only unflagged", func(t *testing.T) {
		got := filterAgents(agents, nil)
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
	})
}

func TestFilterFlaggedAgents(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "necessity", flag: "bare-necessities"},
		{name: "deep", flag: "deep-dive"},
	}

	t.Run("no flags — no agents", func(t *testing.T) {
		got := filterFlaggedAgents(agents, map[string]bool{})
		if len(got) != 0 {
			t.Fatalf("got %d agents, want 0", len(got))
		}
	})

	t.Run("bare-necessities flag — only necessity", func(t *testing.T) {
		got := filterFlaggedAgents(agents, map[string]bool{"bare-necessities": true})
		if len(got) != 1 {
			t.Fatalf("got %d agents, want 1", len(got))
		}
		if got[0].name != "necessity" {
			t.Errorf("got %s, want necessity", got[0].name)
		}
	})

	t.Run("both flags — both flagged agents", func(t *testing.T) {
		got := filterFlaggedAgents(agents, map[string]bool{"bare-necessities": true, "deep-dive": true})
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
	})

	t.Run("nil flags — no agents", func(t *testing.T) {
		got := filterFlaggedAgents(agents, nil)
		if len(got) != 0 {
			t.Fatalf("got %d agents, want 0", len(got))
		}
	})

	t.Run("unrelated flag — no agents", func(t *testing.T) {
		got := filterFlaggedAgents(agents, map[string]bool{"thorough": true})
		if len(got) != 0 {
			t.Fatalf("got %d agents, want 0", len(got))
		}
	})
}

func TestLoadAgents_ParsesFrontmatter(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	os.WriteFile(filepath.Join(dir, "gated.md"), []byte("---\nflag: my-flag\n---\nReview {{.PRURL}}"), 0o644)
	os.WriteFile(filepath.Join(dir, "normal.md"), []byte("Review {{.PRURL}}"), 0o644)

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}

	var gated, normal agentFile
	for _, a := range agents {
		if a.name == "gated" {
			gated = a
		}
		if a.name == "normal" {
			normal = a
		}
	}

	if gated.flag != "my-flag" {
		t.Errorf("gated.flag = %q, want %q", gated.flag, "my-flag")
	}
	if normal.flag != "" {
		t.Errorf("normal.flag = %q, want empty", normal.flag)
	}

	result, err := renderAgent(gated, promptData{PRURL: "http://test"})
	if err != nil {
		t.Fatalf("renderAgent: %v", err)
	}
	if strings.Contains(result, "---") {
		t.Error("frontmatter should be stripped from rendered output")
	}
	if !strings.Contains(result, "http://test") {
		t.Error("rendered output should contain PRURL")
	}
}

func TestLoadAgents_RealNecessityAgent(t *testing.T) {
	old := agentsDir
	agentsDir = "agents"
	defer func() { agentsDir = old }()

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}

	var necessity agentFile
	found := false
	for _, a := range agents {
		if a.name == "necessity" {
			necessity = a
			found = true
		}
	}
	if !found {
		t.Fatal("necessity agent not found")
	}
	if necessity.flag != "bare-necessities" {
		t.Errorf("necessity.flag = %q, want %q", necessity.flag, "bare-necessities")
	}

	withFlag := filterAgents(agents, map[string]bool{"bare-necessities": true})
	withoutFlag := filterAgents(agents, map[string]bool{})

	hasNecessity := func(list []agentFile) bool {
		for _, a := range list {
			if a.name == "necessity" {
				return true
			}
		}
		return false
	}

	if !hasNecessity(withFlag) {
		t.Error("necessity should be included with --bare-necessities flag")
	}
	if hasNecessity(withoutFlag) {
		t.Error("necessity should be excluded without --bare-necessities flag")
	}
}

func TestExtractPerspectiveScore(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantReview string
		wantScore  int
		wantConf   int
	}{
		{
			name: "valid score block",
			input: "## Review\nLooks good.\n\n```\n{\"score\":85,\"confidence\":90,\"rationale\":\"solid code\"}\n```",
			wantReview: "## Review\nLooks good.",
			wantScore:  85,
			wantConf:   90,
		},
		{
			name:       "no score block",
			input:      "## Review\nJust a review with no score.",
			wantReview: "## Review\nJust a review with no score.",
			wantScore:  0,
			wantConf:   0,
		},
		{
			name: "score with extra whitespace",
			input: "Review text here.\n\n```\n{ \"score\": 72, \"confidence\": 60, \"rationale\": \"missing tests\" }\n```\n",
			wantReview: "Review text here.",
			wantScore:  72,
			wantConf:   60,
		},
		{
			name:       "json fence variant",
			input:      "Review.\n\n```json\n{\"score\":80,\"confidence\":75,\"rationale\":\"good\"}\n```",
			wantReview: "Review.",
			wantScore:  80,
			wantConf:   75,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			review, ps := extractPerspectiveScore("test-agent", tt.input)
			if strings.TrimSpace(review) != tt.wantReview {
				t.Errorf("review = %q, want %q", strings.TrimSpace(review), tt.wantReview)
			}
			if ps.Score != tt.wantScore {
				t.Errorf("score = %d, want %d", ps.Score, tt.wantScore)
			}
			if ps.Confidence != tt.wantConf {
				t.Errorf("confidence = %d, want %d", ps.Confidence, tt.wantConf)
			}
			if ps.Agent != "test-agent" {
				t.Errorf("agent = %q, want test-agent", ps.Agent)
			}
		})
	}
}

func TestExtractPerspectiveScore_PreservesAgentName(t *testing.T) {
	input := "review\n```\n{\"score\":50,\"confidence\":50,\"rationale\":\"ok\"}\n```"
	_, ps := extractPerspectiveScore("go-expert", input)
	if ps.Agent != "go-expert" {
		t.Errorf("agent = %q, want go-expert", ps.Agent)
	}
}

func TestDiffLines(t *testing.T) {
	tests := []struct {
		name string
		diff string
		want int
	}{
		{"empty", "", 0},
		{"one line no trailing newline", "hello", 0},
		{"one line with newline", "hello\n", 1},
		{"multiple lines", "a\nb\nc\n", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := diffLines(tt.diff); got != tt.want {
				t.Errorf("diffLines() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestUsageStats_AddAgent(t *testing.T) {
	stats := &UsageStats{}
	stats.AddAgent("correctness", 1.05, 90*time.Second, 12, 50)
	stats.AddAgent("design", 0.83, 75*time.Second, 8, 50)

	if len(stats.AgentMetrics) != 2 {
		t.Fatalf("got %d metrics, want 2", len(stats.AgentMetrics))
	}
	if stats.AgentMetrics[0].Name != "correctness" {
		t.Errorf("first agent = %q, want correctness", stats.AgentMetrics[0].Name)
	}
	if stats.AgentMetrics[1].CostUSD != 0.83 {
		t.Errorf("second cost = %f, want 0.83", stats.AgentMetrics[1].CostUSD)
	}
}

func TestUsageStats_MetricsSummary(t *testing.T) {
	stats := &UsageStats{}
	stats.TotalCostUSD = 4.50
	stats.AgentMetrics = []AgentMetric{
		{Name: "correctness", CostUSD: 1.10, Duration: 114 * time.Second, Turns: 15, MaxTurns: 50},
		{Name: "validator", CostUSD: 0.47, Duration: 83 * time.Second, Turns: 3, MaxTurns: 0},
	}

	summary := stats.MetricsSummary("claude-opus-4-6", "U123", "C456")

	checks := []string{
		"*Review Metrics*",
		"`claude-opus-4-6`",
		"<@U123>",
		"<#C456>",
		"$4.5000",
		"`correctness`",
		"$1.1000",
		"15/50 turns",
		"`validator`",
		"$0.4700",
		"3 turns",
	}
	for _, want := range checks {
		if !strings.Contains(summary, want) {
			t.Errorf("MetricsSummary missing %q\ngot: %s", want, summary)
		}
	}
}

func TestParseFlags_TestIsReserved(t *testing.T) {
	got := parseFlags("review https://github.com/org/repo/pull/1 --test")
	if len(got) != 0 {
		t.Errorf("--test should be reserved, got flags: %v", got)
	}
}

func TestUsageStats_AddAgent_ConcurrentSafe(t *testing.T) {
	stats := &UsageStats{}
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			stats.AddAgent(fmt.Sprintf("agent-%d", idx), float64(idx)*0.1, time.Duration(idx)*time.Second, idx, 50)
		}(i)
	}
	wg.Wait()
	if len(stats.AgentMetrics) != 10 {
		t.Errorf("got %d metrics, want 10", len(stats.AgentMetrics))
	}
}

func TestNopSlack_ImplementsInterface(t *testing.T) {
	var _ SlackAPI = &nopSlack{}
}

func TestNopSlack_AllMethodsReturnNil(t *testing.T) {
	n := &nopSlack{}
	if err := n.AddReaction("eyes", slack.ItemRef{}); err != nil {
		t.Errorf("AddReaction returned error: %v", err)
	}
	if err := n.RemoveReaction("eyes", slack.ItemRef{}); err != nil {
		t.Errorf("RemoveReaction returned error: %v", err)
	}
	_, _, err := n.PostMessage("C123")
	if err != nil {
		t.Errorf("PostMessage returned error: %v", err)
	}
	ch, _, _, err := n.OpenConversation(&slack.OpenConversationParameters{})
	if err != nil {
		t.Errorf("OpenConversation returned error: %v", err)
	}
	if ch == nil {
		t.Error("OpenConversation returned nil channel")
	}
	hist, err := n.GetConversationHistory(&slack.GetConversationHistoryParameters{})
	if err != nil {
		t.Errorf("GetConversationHistory returned error: %v", err)
	}
	if hist == nil {
		t.Error("GetConversationHistory returned nil")
	}
	msgs, _, _, err := n.GetConversationReplies(&slack.GetConversationRepliesParameters{})
	if err != nil {
		t.Errorf("GetConversationReplies returned error: %v", err)
	}
	if msgs != nil {
		t.Errorf("GetConversationReplies returned non-nil messages: %v", msgs)
	}
}

func TestMainCLI_NoArgs_StartsSlackMode(t *testing.T) {
	if len(os.Args) > 1 && os.Args[1] == "review" {
		t.Skip("already in CLI mode")
	}
}

func TestRunCLI_BadURL_Exits(t *testing.T) {
	if os.Getenv("TEST_CLI_BAD_URL") == "1" {
		runCLI([]string{"not-a-url"})
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=TestRunCLI_BadURL_Exits")
	cmd.Env = append(os.Environ(), "TEST_CLI_BAD_URL=1")
	err := cmd.Run()
	if exitErr, ok := err.(*exec.ExitError); ok {
		if exitErr.ExitCode() != 1 {
			t.Errorf("expected exit code 1, got %d", exitErr.ExitCode())
		}
		return
	}
	t.Errorf("expected exit error, got: %v", err)
}

func TestParseFlags_NoGitHubNotInFlags(t *testing.T) {
	got := parseFlags("review https://github.com/org/repo/pull/1 --no-github")
	if _, ok := got["no-github"]; ok {
		t.Error("--no-github should not appear in agent flags")
	}
}

func TestLoadProjectsConfig_ValidFile(t *testing.T) {
	dir := t.TempDir()
	old := projectsConfigPath
	projectsConfigPath = filepath.Join(dir, "projects.json")
	defer func() { projectsConfigPath = old }()

	os.WriteFile(projectsConfigPath, []byte(`{
		"projects": {
			"Qumulo/qompass": {"agents": ["correctness", "go-expert"]},
			"Qumulo/qatalyst": {"agents": ["correctness"]}
		},
		"default": {"agents": ["correctness", "design"]}
	}`), 0o644)

	cfg, err := loadProjectsConfig()
	if err != nil {
		t.Fatalf("loadProjectsConfig: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config")
	}
	if len(cfg.Projects) != 2 {
		t.Errorf("got %d projects, want 2", len(cfg.Projects))
	}
	if len(cfg.Default.Agents) != 2 {
		t.Errorf("got %d default agents, want 2", len(cfg.Default.Agents))
	}
}

func TestLoadProjectsConfig_MissingFile(t *testing.T) {
	old := projectsConfigPath
	projectsConfigPath = "/nonexistent/projects.json"
	defer func() { projectsConfigPath = old }()

	cfg, err := loadProjectsConfig()
	if err != nil {
		t.Fatalf("expected nil error for missing file, got: %v", err)
	}
	if cfg != nil {
		t.Error("expected nil config for missing file")
	}
}

func TestLoadProjectsConfig_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	old := projectsConfigPath
	projectsConfigPath = filepath.Join(dir, "projects.json")
	defer func() { projectsConfigPath = old }()

	os.WriteFile(projectsConfigPath, []byte(`{not valid json`), 0o644)

	_, err := loadProjectsConfig()
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
	if !strings.Contains(err.Error(), "parse projects config") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestAgentsForRepo_ExactMatch(t *testing.T) {
	cfg := &ProjectsConfig{
		Projects: map[string]ProjectAgentConfig{
			"Qumulo/qompass":   {Agents: []string{"correctness", "go-expert"}},
			"Qumulo/qatalyst":  {Agents: []string{"correctness"}},
		},
		Default: ProjectAgentConfig{Agents: []string{"correctness", "design"}},
	}

	got := cfg.agentsForRepo("Qumulo", "qompass")
	if len(got) != 2 || got[0] != "correctness" || got[1] != "go-expert" {
		t.Errorf("qompass agents = %v, want [correctness go-expert]", got)
	}

	got = cfg.agentsForRepo("Qumulo", "qatalyst")
	if len(got) != 1 || got[0] != "correctness" {
		t.Errorf("qatalyst agents = %v, want [correctness]", got)
	}
}

func TestAgentsForRepo_CaseInsensitive(t *testing.T) {
	cfg := &ProjectsConfig{
		Projects: map[string]ProjectAgentConfig{
			"Qumulo/qompass": {Agents: []string{"correctness", "go-expert"}},
		},
		Default: ProjectAgentConfig{Agents: []string{"design"}},
	}

	got := cfg.agentsForRepo("qumulo", "QOMPASS")
	if len(got) != 2 {
		t.Errorf("case-insensitive match failed: got %v", got)
	}
}

func TestAgentsForRepo_FallsBackToDefault(t *testing.T) {
	cfg := &ProjectsConfig{
		Projects: map[string]ProjectAgentConfig{
			"Qumulo/qompass": {Agents: []string{"correctness"}},
		},
		Default: ProjectAgentConfig{Agents: []string{"design", "pragmatic"}},
	}

	got := cfg.agentsForRepo("SomeOrg", "some-repo")
	if len(got) != 2 || got[0] != "design" || got[1] != "pragmatic" {
		t.Errorf("default fallback = %v, want [design pragmatic]", got)
	}
}

func TestAgentsForRepo_NilConfig(t *testing.T) {
	var cfg *ProjectsConfig
	got := cfg.agentsForRepo("Qumulo", "qompass")
	if got != nil {
		t.Errorf("nil config should return nil, got %v", got)
	}
}

func TestAgentsForRepo_NoDefaultNoMatch(t *testing.T) {
	cfg := &ProjectsConfig{
		Projects: map[string]ProjectAgentConfig{
			"Qumulo/qompass": {Agents: []string{"correctness"}},
		},
	}

	got := cfg.agentsForRepo("Other", "repo")
	if got != nil {
		t.Errorf("no default + no match should return nil, got %v", got)
	}
}

func TestFilterAgentsByProject(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "go-expert"},
		{name: "pragmatic"},
		{name: "necessity", flag: "bare-necessities"},
	}

	t.Run("nil allowed — returns all", func(t *testing.T) {
		got := filterAgentsByProject(agents, nil)
		if len(got) != 5 {
			t.Errorf("got %d agents, want 5", len(got))
		}
	})

	t.Run("empty allowed — returns all", func(t *testing.T) {
		got := filterAgentsByProject(agents, []string{})
		if len(got) != 5 {
			t.Errorf("got %d agents, want 5", len(got))
		}
	})

	t.Run("subset — filters correctly", func(t *testing.T) {
		got := filterAgentsByProject(agents, []string{"correctness", "go-expert"})
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
		if got[0].name != "correctness" || got[1].name != "go-expert" {
			t.Errorf("got %s, %s — want correctness, go-expert", got[0].name, got[1].name)
		}
	})

	t.Run("includes flagged agent if in allowed list", func(t *testing.T) {
		got := filterAgentsByProject(agents, []string{"correctness", "necessity"})
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
		if got[1].name != "necessity" {
			t.Errorf("got %s, want necessity", got[1].name)
		}
	})

	t.Run("nonexistent agent in allowed — ignored", func(t *testing.T) {
		got := filterAgentsByProject(agents, []string{"correctness", "nonexistent"})
		if len(got) != 1 {
			t.Fatalf("got %d agents, want 1", len(got))
		}
	})
}

func TestFilterAgentsByProject_ThenFilterAgents(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "go-expert"},
		{name: "necessity", flag: "bare-necessities"},
	}

	projected := filterAgentsByProject(agents, []string{"correctness", "go-expert", "necessity"})
	if len(projected) != 3 {
		t.Fatalf("project filter: got %d, want 3", len(projected))
	}

	withoutFlag := filterAgents(projected, map[string]bool{})
	if len(withoutFlag) != 2 {
		t.Fatalf("flag filter (no flag): got %d, want 2", len(withoutFlag))
	}
	for _, a := range withoutFlag {
		if a.name == "necessity" {
			t.Error("necessity should be excluded without flag")
		}
	}

	withFlag := filterAgents(projected, map[string]bool{"bare-necessities": true})
	if len(withFlag) != 3 {
		t.Fatalf("flag filter (with flag): got %d, want 3", len(withFlag))
	}
}

func TestParseOnlyAgents(t *testing.T) {
	tests := []struct {
		input string
		want  []string
	}{
		{"review https://github.com/org/repo/pull/1", nil},
		{"review https://github.com/org/repo/pull/1 --only correctness", []string{"correctness"}},
		{"review https://github.com/org/repo/pull/1 --only correctness,go-expert", []string{"correctness", "go-expert"}},
		{"review https://github.com/org/repo/pull/1 --only correctness,go-expert,pragmatic", []string{"correctness", "go-expert", "pragmatic"}},
		{"--only design https://github.com/org/repo/pull/1", []string{"design"}},
	}
	for _, tt := range tests {
		got := parseOnlyAgents(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseOnlyAgents(%q) = %v, want %v", tt.input, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseOnlyAgents(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestFilterOnlyAgents(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "go-expert"},
		{name: "pragmatic"},
	}

	t.Run("nil only returns all", func(t *testing.T) {
		got := filterOnlyAgents(agents, nil)
		if len(got) != 4 {
			t.Fatalf("got %d agents, want 4", len(got))
		}
	})

	t.Run("empty only returns all", func(t *testing.T) {
		got := filterOnlyAgents(agents, []string{})
		if len(got) != 4 {
			t.Fatalf("got %d agents, want 4", len(got))
		}
	})

	t.Run("single agent", func(t *testing.T) {
		got := filterOnlyAgents(agents, []string{"correctness"})
		if len(got) != 1 || got[0].name != "correctness" {
			t.Fatalf("got %v, want [correctness]", got)
		}
	})

	t.Run("multiple agents", func(t *testing.T) {
		got := filterOnlyAgents(agents, []string{"correctness", "go-expert"})
		if len(got) != 2 {
			t.Fatalf("got %d agents, want 2", len(got))
		}
		if got[0].name != "correctness" || got[1].name != "go-expert" {
			t.Errorf("got [%s, %s], want [correctness, go-expert]", got[0].name, got[1].name)
		}
	})

	t.Run("nonexistent agent returns empty", func(t *testing.T) {
		got := filterOnlyAgents(agents, []string{"nonexistent"})
		if len(got) != 0 {
			t.Fatalf("got %d agents, want 0", len(got))
		}
	})

	t.Run("mix of valid and invalid", func(t *testing.T) {
		got := filterOnlyAgents(agents, []string{"correctness", "nonexistent"})
		if len(got) != 1 || got[0].name != "correctness" {
			t.Fatalf("got %v, want [correctness]", got)
		}
	})
}

func TestFilterOnlyAgents_WithFlagFilter(t *testing.T) {
	agents := []agentFile{
		{name: "correctness"},
		{name: "design"},
		{name: "necessity", flag: "bare-necessities"},
	}

	filtered := filterAgents(agents, map[string]bool{"bare-necessities": true})
	got := filterOnlyAgents(filtered, []string{"correctness", "necessity"})
	if len(got) != 2 {
		t.Fatalf("got %d agents, want 2", len(got))
	}
}

func TestLoadProjectsConfig_RealFile(t *testing.T) {
	old := projectsConfigPath
	projectsConfigPath = "projects.json"
	defer func() { projectsConfigPath = old }()

	cfg, err := loadProjectsConfig()
	if err != nil {
		t.Fatalf("loadProjectsConfig on real file: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected non-nil config from projects.json")
	}

	qompass := cfg.agentsForRepo("Qumulo", "qompass")
	if len(qompass) == 0 {
		t.Error("expected agents for Qumulo/qompass")
	}

	qatalyst := cfg.agentsForRepo("Qumulo", "qatalyst")
	if len(qatalyst) == 0 {
		t.Error("expected agents for Qumulo/qatalyst")
	}

	dflt := cfg.agentsForRepo("unknown", "repo")
	if len(dflt) == 0 {
		t.Error("expected default agents for unknown repo")
	}

	if len(qompass) == len(qatalyst) {
		allSame := true
		for i := range qompass {
			if qompass[i] != qatalyst[i] {
				allSame = false
				break
			}
		}
		if allSame {
			t.Error("qompass and qatalyst should have different agent configs")
		}
	}
}

func TestSaveAndLoadReviewMemory_RoundTrip(t *testing.T) {
	tmpDir := t.TempDir()
	owner := "testowner"
	repo := "testrepo"
	prNum := "42"

	memDir := filepath.Join(tmpDir, owner, repo, "pr-"+prNum)
	if err := os.MkdirAll(memDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// Override reviewMemoryPath by writing/reading directly via the path logic
	// Instead, we test save+load by using a temp HOME
	origHome := os.Getenv("HOME")
	fakeCacheDir := filepath.Join(tmpDir, "fakehome")
	os.Setenv("HOME", fakeCacheDir)
	defer os.Setenv("HOME", origHome)

	mem := &ReviewMemory{
		PRURL:        "https://github.com/testowner/testrepo/pull/42",
		LastReviewed: time.Date(2026, 5, 7, 10, 30, 0, 0, time.UTC),
		ReviewCount:  2,
		Mode:         "initial",
		Score:        72,
		Verdict:      "Request Changes",
		AgentSummaries: []AgentSummary{
			{Agent: "go-expert", KeyFindings: "missing error check", Score: 85},
			{Agent: "correctness", KeyFindings: "race condition in handler", Score: 60},
		},
		CriticalIssues: []string{"race condition in handleReq", "missing auth check"},
		ResolvedIssues: []string{},
		FilesReviewed:  []string{"handler.go", "main.go"},
		DiffStats:      ReviewMemoryDiffSt{Files: 5, Additions: 120, Deletions: 30},
	}

	if err := saveReviewMemory(owner, repo, prNum, mem); err != nil {
		t.Fatalf("saveReviewMemory: %v", err)
	}

	loaded, err := loadReviewMemory(owner, repo, prNum)
	if err != nil {
		t.Fatalf("loadReviewMemory: %v", err)
	}
	if loaded == nil {
		t.Fatal("loadReviewMemory returned nil")
	}

	if loaded.PRURL != mem.PRURL {
		t.Errorf("PRURL = %q, want %q", loaded.PRURL, mem.PRURL)
	}
	if loaded.ReviewCount != mem.ReviewCount {
		t.Errorf("ReviewCount = %d, want %d", loaded.ReviewCount, mem.ReviewCount)
	}
	if loaded.Score != mem.Score {
		t.Errorf("Score = %d, want %d", loaded.Score, mem.Score)
	}
	if loaded.Verdict != mem.Verdict {
		t.Errorf("Verdict = %q, want %q", loaded.Verdict, mem.Verdict)
	}
	if len(loaded.AgentSummaries) != 2 {
		t.Fatalf("AgentSummaries len = %d, want 2", len(loaded.AgentSummaries))
	}
	if loaded.AgentSummaries[0].Agent != "go-expert" {
		t.Errorf("AgentSummaries[0].Agent = %q, want %q", loaded.AgentSummaries[0].Agent, "go-expert")
	}
	if len(loaded.CriticalIssues) != 2 {
		t.Errorf("CriticalIssues len = %d, want 2", len(loaded.CriticalIssues))
	}
	if len(loaded.FilesReviewed) != 2 {
		t.Errorf("FilesReviewed len = %d, want 2", len(loaded.FilesReviewed))
	}
	if loaded.DiffStats.Additions != 120 {
		t.Errorf("DiffStats.Additions = %d, want 120", loaded.DiffStats.Additions)
	}
}

func TestLoadReviewMemory_NonexistentReturnsNil(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	mem, err := loadReviewMemory("noowner", "norepo", "999")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mem != nil {
		t.Errorf("expected nil for nonexistent memory, got %+v", mem)
	}
}

func TestFormatPriorContext_Nil(t *testing.T) {
	result := formatPriorContext(nil)
	if result != "" {
		t.Errorf("expected empty string for nil memory, got %q", result)
	}
}

func TestFormatPriorContext_FullMemory(t *testing.T) {
	mem := &ReviewMemory{
		PRURL:        "https://github.com/owner/repo/pull/1",
		LastReviewed: time.Date(2026, 5, 7, 10, 30, 0, 0, time.UTC),
		ReviewCount:  3,
		Mode:         "initial",
		Score:        72,
		Verdict:      "Request Changes",
		AgentSummaries: []AgentSummary{
			{Agent: "go-expert", KeyFindings: "missing error check", Score: 85},
		},
		CriticalIssues: []string{"race condition in handleReq"},
		ResolvedIssues: []string{"fixed nil pointer"},
		FilesReviewed:  []string{"main.go", "handler.go"},
		DiffStats:      ReviewMemoryDiffSt{Files: 2, Additions: 50, Deletions: 10},
	}

	result := formatPriorContext(mem)

	checks := []struct {
		desc string
		want string
	}{
		{"header", "## Prior Review Context"},
		{"review count", "reviewed 3 time(s)"},
		{"score", "**Previous score:** 72/100"},
		{"verdict", "**Verdict:** Request Changes"},
		{"critical issue", "race condition in handleReq"},
		{"resolved issue", "fixed nil pointer"},
		{"agent summary", "**go-expert** (score: 85)"},
		{"files reviewed count", "**Files reviewed:** 2"},
		{"diff additions", "+50/-10"},
	}

	for _, c := range checks {
		if !strings.Contains(result, c.want) {
			t.Errorf("%s: result does not contain %q\nfull result:\n%s", c.desc, c.want, result)
		}
	}
}

func TestFormatPriorContext_NoOptionalSections(t *testing.T) {
	mem := &ReviewMemory{
		PRURL:        "https://github.com/owner/repo/pull/1",
		LastReviewed: time.Date(2026, 5, 7, 10, 0, 0, 0, time.UTC),
		ReviewCount:  1,
		Score:        90,
		Verdict:      "Approve",
		DiffStats:    ReviewMemoryDiffSt{Files: 1, Additions: 5, Deletions: 0},
	}

	result := formatPriorContext(mem)

	if strings.Contains(result, "Critical issues") {
		t.Error("should not contain critical issues section when empty")
	}
	if strings.Contains(result, "Resolved issues") {
		t.Error("should not contain resolved issues section when empty")
	}
	if strings.Contains(result, "Agent findings") {
		t.Error("should not contain agent findings section when empty")
	}
	if !strings.Contains(result, "**Verdict:** Approve") {
		t.Error("should contain verdict")
	}
}

func TestPriorContext_RendersInAgentTemplate(t *testing.T) {
	old := agentsDir
	dir := t.TempDir()
	agentsDir = dir
	defer func() { agentsDir = old }()

	content := `You are a reviewer. {{.PRURL}}
{{if .PriorContext}}
{{.PriorContext}}
{{end}}
` + "```diff\n{{.Diff}}\n```"
	if err := os.WriteFile(filepath.Join(dir, "test-agent.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	t.Run("with prior context", func(t *testing.T) {
		data := promptData{
			PRURL:        "https://github.com/o/r/pull/1",
			Diff:         "+added line",
			PriorContext: "## Prior Review Context\nPreviously reviewed 2 time(s).",
		}
		result, err := renderAgent(agents[0], data)
		if err != nil {
			t.Fatalf("renderAgent: %v", err)
		}
		if !strings.Contains(result, "Prior Review Context") {
			t.Error("prior context not rendered in template output")
		}
		if !strings.Contains(result, "Previously reviewed 2 time(s).") {
			t.Error("prior context details not rendered")
		}
	})

	t.Run("without prior context", func(t *testing.T) {
		data := promptData{
			PRURL: "https://github.com/o/r/pull/1",
			Diff:  "+added line",
		}
		result, err := renderAgent(agents[0], data)
		if err != nil {
			t.Fatalf("renderAgent: %v", err)
		}
		if strings.Contains(result, "Prior Review Context") {
			t.Error("prior context should not appear when PriorContext is empty")
		}
	})
}

func TestExtractVerdict(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"request changes", "## Verdict\n\n**Request Changes** — several issues need attention", "Request Changes"},
		{"approve", "## Verdict: Approve\nAll looks good.", "Approve"},
		{"approve lowercase", "I'd recommend we approve this PR.", "Approve"},
		{"request changes lowercase", "Verdict: request changes", "Request Changes"},
		{"unknown", "## Summary\nThis PR adds a feature.", "Unknown"},
		{"empty", "", "Unknown"},
		{"both prefers request changes", "We could approve but request changes on auth", "Request Changes"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractVerdict(tt.input)
			if got != tt.want {
				t.Errorf("extractVerdict = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractCriticalIssues(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{
			"h2 critical section",
			"## Summary\nok\n\n## Critical Issues\n- race condition in handleReq\n- missing auth check\n\n## Design\nfine",
			[]string{"race condition in handleReq", "missing auth check"},
		},
		{
			"h3 critical section",
			"### Critical Issues\n- nil pointer\n\n### Suggestions\n- add tests",
			[]string{"nil pointer"},
		},
		{
			"no critical section",
			"## Summary\nLooks good.\n## Suggestions\n- minor naming",
			nil,
		},
		{
			"empty critical section",
			"## Critical Issues\n\n## Design",
			nil,
		},
		{
			"critical at end of doc",
			"## Summary\nok\n## Critical Issues\n- last issue",
			[]string{"last issue"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractCriticalIssues(tt.input)
			if len(got) != len(tt.want) {
				t.Fatalf("got %d issues %v, want %d %v", len(got), got, len(tt.want), tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("issue[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestComputeDiffStats(t *testing.T) {
	diff := `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -10,3 +10,5 @@
 unchanged
-removed line 1
-removed line 2
+added line 1
+added line 2
+added line 3
diff --git a/handler.go b/handler.go
--- a/handler.go
+++ b/handler.go
@@ -1,2 +1,3 @@
 unchanged
+new line
`
	stats := computeDiffStats(diff)

	if stats.Files != 2 {
		t.Errorf("Files = %d, want 2", stats.Files)
	}
	if stats.Additions != 4 {
		t.Errorf("Additions = %d, want 4", stats.Additions)
	}
	if stats.Deletions != 2 {
		t.Errorf("Deletions = %d, want 2", stats.Deletions)
	}
}

func TestBuildReviewMemory(t *testing.T) {
	tmpDir := t.TempDir()
	origHome := os.Getenv("HOME")
	os.Setenv("HOME", tmpDir)
	defer os.Setenv("HOME", origHome)

	req := ReviewRequest{
		PRURL: "https://github.com/owner/repo/pull/5",
		Owner: "owner",
		Repo:  "repo",
		PRNum: "5",
		Mode:  ModeInitial,
		Diff: `diff --git a/main.go b/main.go
--- a/main.go
+++ b/main.go
@@ -1,2 +1,3 @@
 existing
+new line
`,
	}

	mergedText := "## Summary\nLooks okay.\n\n## Critical Issues\n- missing nil check\n\n## Verdict\nRequest Changes"
	score := &ScoreResult{Overall: 65}
	perspectives := []PerspectiveScore{
		{Agent: "correctness", Score: 60, Rationale: "found nil issue"},
		{Agent: "design", Score: 70, Rationale: "clean structure"},
	}

	mem := buildReviewMemory(req, mergedText, score, perspectives)

	if mem.PRURL != req.PRURL {
		t.Errorf("PRURL = %q, want %q", mem.PRURL, req.PRURL)
	}
	if mem.ReviewCount != 1 {
		t.Errorf("ReviewCount = %d, want 1", mem.ReviewCount)
	}
	if mem.Score != 65 {
		t.Errorf("Score = %d, want 65", mem.Score)
	}
	if mem.Verdict != "Request Changes" {
		t.Errorf("Verdict = %q, want %q", mem.Verdict, "Request Changes")
	}
	if len(mem.CriticalIssues) != 1 || mem.CriticalIssues[0] != "missing nil check" {
		t.Errorf("CriticalIssues = %v, want [missing nil check]", mem.CriticalIssues)
	}
	if len(mem.AgentSummaries) != 2 {
		t.Fatalf("AgentSummaries len = %d, want 2", len(mem.AgentSummaries))
	}
	if mem.AgentSummaries[0].Agent != "correctness" {
		t.Errorf("AgentSummaries[0].Agent = %q, want %q", mem.AgentSummaries[0].Agent, "correctness")
	}
	if len(mem.FilesReviewed) != 1 || mem.FilesReviewed[0] != "main.go" {
		t.Errorf("FilesReviewed = %v, want [main.go]", mem.FilesReviewed)
	}
}

func TestMinInt(t *testing.T) {
	tests := []struct {
		a, b, want int
	}{
		{5, 10, 5},
		{10, 5, 5},
		{7, 7, 7},
		{0, 5, 0},
		{12, 40, 12},
	}
	for _, tt := range tests {
		got := minInt(tt.a, tt.b)
		if got != tt.want {
			t.Errorf("minInt(%d, %d) = %d, want %d", tt.a, tt.b, got, tt.want)
		}
	}
}

func TestDiffManifest_FormatForValidator_Empty(t *testing.T) {
	m := DiffManifest{}
	got := m.FormatForValidator()
	if got != "" {
		t.Errorf("empty manifest should return empty string, got %q", got)
	}
}

func TestDiffManifest_FormatForValidator_WithCommits(t *testing.T) {
	m := DiffManifest{
		Commits: []CommitEntry{
			{SHA: "abc123def456", Subject: "feat: add handler", Files: []string{"main.go", "handler.go"}},
			{SHA: "def789abc012", Subject: "test: add handler test", Files: []string{"main_test.go"}},
		},
		Files: []string{"main.go", "handler.go", "main_test.go"},
	}
	got := m.FormatForValidator()

	if !strings.Contains(got, "## PR Commit Manifest") {
		t.Error("should contain manifest header")
	}
	if !strings.Contains(got, "`abc123def456`") {
		t.Error("should contain first commit SHA")
	}
	if !strings.Contains(got, "feat: add handler") {
		t.Error("should contain first commit subject")
	}
	if !strings.Contains(got, "  - main.go") {
		t.Error("should contain file listing for first commit")
	}
	if !strings.Contains(got, "`def789abc012`") {
		t.Error("should contain second commit SHA")
	}
	if !strings.Contains(got, "  - main_test.go") {
		t.Error("should contain file listing for second commit")
	}
	if !strings.Contains(got, "**Total files changed in PR:** 3") {
		t.Error("should contain total file count")
	}
}

func TestDiffManifest_FormatForValidator_NoFiles(t *testing.T) {
	m := DiffManifest{
		Commits: []CommitEntry{
			{SHA: "abc123", Subject: "empty commit", Files: nil},
		},
		Files: nil,
	}
	got := m.FormatForValidator()
	if !strings.Contains(got, "`abc123`") {
		t.Error("should contain commit SHA")
	}
	if !strings.Contains(got, "**Total files changed in PR:** 0") {
		t.Error("should show 0 total files")
	}
}

func TestCommitEntry_JSONRoundTrip(t *testing.T) {
	entry := CommitEntry{
		SHA:     "abc123def456",
		Subject: "fix: handle nil",
		Files:   []string{"main.go", "util.go"},
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got CommitEntry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.SHA != entry.SHA {
		t.Errorf("SHA = %q, want %q", got.SHA, entry.SHA)
	}
	if got.Subject != entry.Subject {
		t.Errorf("Subject = %q, want %q", got.Subject, entry.Subject)
	}
	if len(got.Files) != 2 || got.Files[0] != "main.go" || got.Files[1] != "util.go" {
		t.Errorf("Files = %v, want [main.go util.go]", got.Files)
	}
}

func TestDiffManifest_JSONRoundTrip(t *testing.T) {
	m := DiffManifest{
		Commits: []CommitEntry{
			{SHA: "abc123", Subject: "first", Files: []string{"a.go"}},
			{SHA: "def456", Subject: "second", Files: []string{"b.go", "c.go"}},
		},
		Files: []string{"a.go", "b.go", "c.go"},
	}
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got DiffManifest
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Commits) != 2 {
		t.Fatalf("Commits len = %d, want 2", len(got.Commits))
	}
	if got.Commits[0].SHA != "abc123" {
		t.Errorf("Commits[0].SHA = %q, want %q", got.Commits[0].SHA, "abc123")
	}
	if len(got.Files) != 3 {
		t.Errorf("Files len = %d, want 3", len(got.Files))
	}
}

func TestFilterAgentsByDiff(t *testing.T) {
	agents := []agentFile{
		{name: "general"},
		{name: "clickhouse", diffMatch: `(?i)(clickhouse|MergeTree|CREATE\s+TABLE)`},
		{name: "sql-only", diffMatch: `\.sql`},
		{name: "bad-regex", diffMatch: `[invalid`},
	}

	tests := []struct {
		name string
		diff string
		want []string
	}{
		{
			name: "no diff_match agents always included",
			diff: "just some go code",
			want: []string{"general"},
		},
		{
			name: "clickhouse keyword matches",
			diff: "import clickhouse-go driver",
			want: []string{"general", "clickhouse"},
		},
		{
			name: "MergeTree matches case insensitive",
			diff: "ENGINE = ReplacingMergeTree(ver)",
			want: []string{"general", "clickhouse"},
		},
		{
			name: "CREATE TABLE matches",
			diff: "CREATE TABLE events (",
			want: []string{"general", "clickhouse"},
		},
		{
			name: "sql file matches",
			diff: "--- a/migrations/001.sql\n+++ b/migrations/001.sql",
			want: []string{"general", "sql-only"},
		},
		{
			name: "multiple agents match",
			diff: "CREATE TABLE events in migrations/001.sql",
			want: []string{"general", "clickhouse", "sql-only"},
		},
		{
			name: "nothing matches",
			diff: "func handleHTTP(w http.ResponseWriter)",
			want: []string{"general"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterAgentsByDiff(agents, tt.diff)
			var names []string
			for _, a := range got {
				names = append(names, a.name)
			}
			if len(names) != len(tt.want) {
				t.Fatalf("got %v, want %v", names, tt.want)
			}
			for i, name := range names {
				if name != tt.want[i] {
					t.Errorf("got[%d] = %q, want %q", i, name, tt.want[i])
				}
			}
		})
	}
}

func TestDiffLocationAt(t *testing.T) {
	diff := `diff --git a/cmd/server.go b/cmd/server.go
--- a/cmd/server.go
+++ b/cmd/server.go
@@ -10,3 +10,4 @@
 existing line
+new line with MergeTree
 another line
diff --git a/internal/storage/migrations/001.sql b/internal/storage/migrations/001.sql
--- a/internal/storage/migrations/001.sql
+++ b/internal/storage/migrations/001.sql
@@ -1,2 +1,3 @@
+CREATE TABLE events ENGINE = ReplacingMergeTree
`

	tests := []struct {
		name     string
		substr  string
		wantFile string
	}{
		{"match in go file", "MergeTree", "cmd/server.go"},
		{"match in sql file", "ReplacingMergeTree", "internal/storage/migrations/001.sql"},
		{"match before any file header", "diff --git", "(unknown)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			offset := strings.Index(diff, tt.substr)
			if offset < 0 {
				t.Fatalf("substring %q not found in diff", tt.substr)
			}
			file, line := diffLocationAt(diff, offset)
			if file != tt.wantFile {
				t.Errorf("file = %q, want %q", file, tt.wantFile)
			}
			if line < 1 {
				t.Errorf("line = %d, want >= 1", line)
			}
		})
	}
}

func TestPRBase_ParsesJSON(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    prBase
		wantErr bool
	}{
		{
			name:  "main branch",
			input: `{"baseRefName":"main","baseRefOid":"abc123def456"}`,
			want:  prBase{Name: "main", OID: "abc123def456"},
		},
		{
			name:  "feature branch (stacked PR)",
			input: `{"baseRefName":"feat/parent-branch","baseRefOid":"deadbeef0123456789"}`,
			want:  prBase{Name: "feat/parent-branch", OID: "deadbeef0123456789"},
		},
		{
			name:    "invalid JSON",
			input:   `not json`,
			wantErr: true,
		},
		{
			name:  "empty fields",
			input: `{"baseRefName":"","baseRefOid":""}`,
			want:  prBase{Name: "", OID: ""},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got prBase
			err := json.Unmarshal([]byte(tt.input), &got)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// --- ReviewQueue tests ---

func TestReviewQueue_ExecutesJobs(t *testing.T) {
	q := NewReviewQueue(2, 5)
	var count atomic.Int64

	for range 5 {
		q.Submit(func() {
			count.Add(1)
		})
	}

	q.Drain()
	if got := count.Load(); got != 5 {
		t.Errorf("executed %d jobs, want 5", got)
	}
}

func TestReviewQueue_RespectsWorkerLimit(t *testing.T) {
	q := NewReviewQueue(2, 10)
	var concurrent atomic.Int64
	var maxConcurrent atomic.Int64

	for range 6 {
		q.Submit(func() {
			cur := concurrent.Add(1)
			for {
				old := maxConcurrent.Load()
				if cur <= old || maxConcurrent.CompareAndSwap(old, cur) {
					break
				}
			}
			time.Sleep(50 * time.Millisecond)
			concurrent.Add(-1)
		})
	}

	q.Drain()
	if got := maxConcurrent.Load(); got > 2 {
		t.Errorf("max concurrent = %d, want <= 2", got)
	}
}

func TestReviewQueue_DropsWhenFull(t *testing.T) {
	q := NewReviewQueue(1, 2)
	blocker := make(chan struct{})

	q.Submit(func() { <-blocker })
	q.Submit(func() { <-blocker })
	q.Submit(func() { <-blocker })

	ok := q.Submit(func() {})
	if ok {
		t.Error("expected Submit to return false when queue full")
	}

	_, _, _, dropped := q.Stats()
	if dropped < 1 {
		t.Errorf("dropped = %d, want >= 1", dropped)
	}

	close(blocker)
	q.Drain()
}

func TestReviewQueue_Stats(t *testing.T) {
	q := NewReviewQueue(1, 5)
	done := make(chan struct{})

	q.Submit(func() {
		q.completed.Add(1)
		<-done
	})
	q.Submit(func() {
		q.completed.Add(1)
	})

	time.Sleep(20 * time.Millisecond)
	pending, _, _, _ := q.Stats()
	if pending < 1 {
		t.Errorf("pending = %d, want >= 1", pending)
	}

	close(done)
	q.Drain()

	_, _, completed, _ := q.Stats()
	if completed != 2 {
		t.Errorf("completed = %d, want 2", completed)
	}
}

// --- parseWatchedChannels tests ---

func TestParseWatchedChannels_Single(t *testing.T) {
	got := parseWatchedChannels("C123")
	if !got["C123"] || len(got) != 1 {
		t.Errorf("got %v, want {C123: true}", got)
	}
}

func TestParseWatchedChannels_Multiple(t *testing.T) {
	got := parseWatchedChannels("C123,C456,C789")
	if len(got) != 3 || !got["C123"] || !got["C456"] || !got["C789"] {
		t.Errorf("got %v, want 3 channels", got)
	}
}

func TestParseWatchedChannels_Wildcard(t *testing.T) {
	got := parseWatchedChannels("*")
	if got != nil {
		t.Errorf("got %v, want nil (all channels)", got)
	}
}

func TestParseWatchedChannels_TrimSpaces(t *testing.T) {
	got := parseWatchedChannels(" C123 , C456 ")
	if len(got) != 2 || !got["C123"] || !got["C456"] {
		t.Errorf("got %v, want {C123, C456}", got)
	}
}

// --- isWatchedChannel tests ---

func TestIsWatchedChannel_NilAllowsAll(t *testing.T) {
	if !isWatchedChannel("C999", nil) {
		t.Error("nil map should allow all channels")
	}
}

func TestIsWatchedChannel_InSet(t *testing.T) {
	m := map[string]bool{"C123": true}
	if !isWatchedChannel("C123", m) {
		t.Error("should match channel in set")
	}
}

func TestIsWatchedChannel_NotInSet(t *testing.T) {
	m := map[string]bool{"C123": true}
	if isWatchedChannel("C999", m) {
		t.Error("should not match channel outside set")
	}
}

// --- envIntOr tests ---

func TestEnvIntOr_Fallback(t *testing.T) {
	os.Unsetenv("TEST_ENVINT_UNSET")
	if got := envIntOr("TEST_ENVINT_UNSET", 42); got != 42 {
		t.Errorf("got %d, want 42", got)
	}
}

func TestEnvIntOr_ValidValue(t *testing.T) {
	t.Setenv("TEST_ENVINT_VALID", "7")
	if got := envIntOr("TEST_ENVINT_VALID", 42); got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

func TestEnvIntOr_InvalidFallback(t *testing.T) {
	t.Setenv("TEST_ENVINT_BAD", "notanumber")
	if got := envIntOr("TEST_ENVINT_BAD", 42); got != 42 {
		t.Errorf("got %d, want 42 (fallback)", got)
	}
}

// --- Health server tests ---

func TestPostReviewEndpoint_QueuesReview(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	var called atomic.Bool
	handler := func(pr string, flags string) {
		called.Store(true)
		if pr != "https://github.com/org/repo/pull/42" {
			t.Errorf("pr = %q, want https://github.com/org/repo/pull/42", pr)
		}
		if flags != "--quick" {
			t.Errorf("flags = %q, want --quick", flags)
		}
	}

	srv := startHealthServer("0", q, NewLogBuffer(100), handler)
	defer srv.Close()

	body := `{"pr_url":"https://github.com/org/repo/pull/42","flags":"--quick"}`
	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusAccepted {
		t.Errorf("status = %d, want 202", resp.StatusCode)
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result["status"] != "queued" {
		t.Errorf("status = %v, want queued", result["status"])
	}

	time.Sleep(50 * time.Millisecond)
	if !called.Load() {
		t.Error("handler was not called")
	}
}

func TestPostReviewEndpoint_RejectsBadJSON(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json", strings.NewReader("not json"))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostReviewEndpoint_RejectsMissingURL(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json", strings.NewReader(`{"flags":"--quick"}`))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostReviewEndpoint_RejectsInvalidURL(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json",
		strings.NewReader(`{"pr_url":"https://example.com/not-a-pr"}`))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestPostReviewEndpoint_RejectsGET(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Get("http://" + srv.Addr + "/review")
	if err != nil {
		t.Fatalf("GET /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", resp.StatusCode)
	}
}

func TestPostReviewEndpoint_QueueFull(t *testing.T) {
	q := NewReviewQueue(1, 1)

	blocker := make(chan struct{})
	workerBusy := make(chan struct{})
	q.Submit(func() { close(workerBusy); <-blocker })
	<-workerBusy
	q.Submit(func() { <-blocker })

	handler := func(string, string) {}
	srv := startHealthServer("0", q, NewLogBuffer(100), handler)
	defer srv.Close()

	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json",
		strings.NewReader(`{"pr_url":"https://github.com/org/repo/pull/1"}`))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}

	close(blocker)
	q.Drain()
}

func TestPostReviewEndpoint_NoHandler(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Post("http://"+srv.Addr+"/review", "application/json",
		strings.NewReader(`{"pr_url":"https://github.com/org/repo/pull/1"}`))
	if err != nil {
		t.Fatalf("POST /review: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", resp.StatusCode)
	}
}

func TestHealthEndpoint(t *testing.T) {
	q := NewReviewQueue(1, 5)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Get("http://" + srv.Addr + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Errorf("body = %s, want status ok", body)
	}
}

func TestMetricsEndpoint(t *testing.T) {
	q := NewReviewQueue(1, 5)
	q.completed.Store(3)
	q.dropped.Store(1)
	defer q.Drain()

	srv := startHealthServer("0", q, NewLogBuffer(100), nil)
	defer srv.Close()

	resp, err := http.Get("http://" + srv.Addr + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	var data map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&data); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := data["reviews_done"]; got != float64(3) {
		t.Errorf("reviews_done = %v, want 3", got)
	}
	if got := data["reviews_dropped"]; got != float64(1) {
		t.Errorf("reviews_dropped = %v, want 1", got)
	}
}

func TestParsePRRef(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantOK  bool
		wantRef prRef
	}{
		{
			name:    "github PR URL",
			input:   "https://github.com/Qumulo/qompass/pull/316",
			wantOK:  true,
			wantRef: prRef{URL: "https://github.com/Qumulo/qompass/pull/316", Owner: "Qumulo", Repo: "qompass", Num: "316"},
		},
		{
			name:    "graphite PR URL",
			input:   "https://app.graphite.com/github/pr/Qumulo/qompass/316",
			wantOK:  true,
			wantRef: prRef{URL: "https://app.graphite.com/github/pr/Qumulo/qompass/316", Owner: "Qumulo", Repo: "qompass", Num: "316"},
		},
		{
			name:    "github and graphite produce same owner/repo/num",
			input:   "https://app.graphite.com/github/pr/org/repo/42",
			wantOK:  true,
			wantRef: prRef{URL: "https://app.graphite.com/github/pr/org/repo/42", Owner: "org", Repo: "repo", Num: "42"},
		},
		{
			name:    "slack-wrapped github URL",
			input:   "<https://github.com/org/repo/pull/99>",
			wantOK:  true,
			wantRef: prRef{URL: "<https://github.com/org/repo/pull/99>", Owner: "org", Repo: "repo", Num: "99"},
		},
		{
			name:    "slack-wrapped graphite URL",
			input:   "<https://app.graphite.com/github/pr/org/repo/99>",
			wantOK:  true,
			wantRef: prRef{URL: "<https://app.graphite.com/github/pr/org/repo/99>", Owner: "org", Repo: "repo", Num: "99"},
		},
		{
			name:    "graphite URL with trailing path segments",
			input:   "https://app.graphite.com/github/pr/Qumulo/qompass/316/some-branch-name",
			wantOK:  true,
			wantRef: prRef{URL: "https://app.graphite.com/github/pr/Qumulo/qompass/316/some-branch-name", Owner: "Qumulo", Repo: "qompass", Num: "316"},
		},
		{
			name:   "not a PR URL",
			input:  "https://example.com/foo/bar",
			wantOK: false,
		},
		{
			name:   "empty string",
			input:  "",
			wantOK: false,
		},
		{
			name:    "URL embedded in text",
			input:   "please review https://github.com/org/repo/pull/5 thanks",
			wantOK:  true,
			wantRef: prRef{URL: "https://github.com/org/repo/pull/5", Owner: "org", Repo: "repo", Num: "5"},
		},
		{
			name:    "graphite URL embedded in text",
			input:   "check https://app.graphite.com/github/pr/org/repo/5 please",
			wantOK:  true,
			wantRef: prRef{URL: "https://app.graphite.com/github/pr/org/repo/5", Owner: "org", Repo: "repo", Num: "5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ref, ok := parsePRRef(tt.input)
			if ok != tt.wantOK {
				t.Fatalf("parsePRRef(%q) ok = %v, want %v", tt.input, ok, tt.wantOK)
			}
			if !ok {
				return
			}
			if ref.Owner != tt.wantRef.Owner || ref.Repo != tt.wantRef.Repo || ref.Num != tt.wantRef.Num {
				t.Errorf("parsePRRef(%q) = {Owner:%q Repo:%q Num:%q}, want {Owner:%q Repo:%q Num:%q}",
					tt.input, ref.Owner, ref.Repo, ref.Num, tt.wantRef.Owner, tt.wantRef.Repo, tt.wantRef.Num)
			}
			if ref.URL != tt.wantRef.URL {
				t.Errorf("parsePRRef(%q) URL = %q, want %q", tt.input, ref.URL, tt.wantRef.URL)
			}
		})
	}
}

func TestFindPRRefs(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantLen  int
		wantNums []string
	}{
		{
			name:     "two github URLs",
			input:    "review https://github.com/org/repo/pull/1 and https://github.com/org/repo/pull/2",
			wantLen:  2,
			wantNums: []string{"1", "2"},
		},
		{
			name:     "mixed github and graphite URLs",
			input:    "https://github.com/org/repo/pull/1 https://app.graphite.com/github/pr/org/repo/2",
			wantLen:  2,
			wantNums: []string{"1", "2"},
		},
		{
			name:     "only graphite URLs",
			input:    "https://app.graphite.com/github/pr/org/repo/10 https://app.graphite.com/github/pr/org/repo/11",
			wantLen:  2,
			wantNums: []string{"10", "11"},
		},
		{
			name:    "no URLs",
			input:   "just some text",
			wantLen: 0,
		},
		{
			name:     "duplicate PR via both URL formats deduped",
			input:    "https://github.com/org/repo/pull/5 https://app.graphite.com/github/pr/org/repo/5",
			wantLen:  1,
			wantNums: []string{"5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			refs := findPRRefs(tt.input)
			if len(refs) != tt.wantLen {
				t.Fatalf("findPRRefs(%q) returned %d refs, want %d: %+v", tt.input, len(refs), tt.wantLen, refs)
			}
			for i, wantNum := range tt.wantNums {
				if refs[i].Num != wantNum {
					t.Errorf("refs[%d].Num = %q, want %q", i, refs[i].Num, wantNum)
				}
			}
		})
	}
}

func TestDiscoverStack(t *testing.T) {
	tests := []struct {
		name     string
		startNum int
		prs      map[int]ghPull
		wantNums []int
		wantErr  bool
	}{
		{
			name:     "linear 3-PR stack",
			startNum: 3,
			prs: map[int]ghPull{
				3: {Number: 3, BaseRefName: "feat/step-2", HeadRefName: "feat/step-3"},
				2: {Number: 2, BaseRefName: "feat/step-1", HeadRefName: "feat/step-2"},
				1: {Number: 1, BaseRefName: "main", HeadRefName: "feat/step-1"},
			},
			wantNums: []int{1, 2, 3},
		},
		{
			name:     "single PR off main",
			startNum: 10,
			prs: map[int]ghPull{
				10: {Number: 10, BaseRefName: "main", HeadRefName: "feat/only"},
			},
			wantNums: []int{10},
		},
		{
			name:     "stack with merged middle PR",
			startNum: 3,
			prs: map[int]ghPull{
				3: {Number: 3, BaseRefName: "feat/step-2", HeadRefName: "feat/step-3"},
				// PR 2 is merged/deleted — no open PR with head=feat/step-2
				// so lookup for base "feat/step-2" finds nothing
			},
			wantNums: []int{3},
		},
		{
			name:     "start from middle of stack",
			startNum: 2,
			prs: map[int]ghPull{
				3: {Number: 3, BaseRefName: "feat/step-2", HeadRefName: "feat/step-3"},
				2: {Number: 2, BaseRefName: "feat/step-1", HeadRefName: "feat/step-2"},
				1: {Number: 1, BaseRefName: "main", HeadRefName: "feat/step-1"},
			},
			wantNums: []int{1, 2},
		},
		{
			name:     "default branch is master",
			startNum: 1,
			prs: map[int]ghPull{
				1: {Number: 1, BaseRefName: "master", HeadRefName: "feat/thing"},
			},
			wantNums: []int{1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fetcher := &mockPRFetcher{prs: tt.prs}
			nums, err := discoverStack(context.Background(), fetcher, "org", "repo", tt.startNum)
			if (err != nil) != tt.wantErr {
				t.Fatalf("discoverStack() err = %v, wantErr = %v", err, tt.wantErr)
			}
			if !tt.wantErr && !equalInts(nums, tt.wantNums) {
				t.Errorf("discoverStack() = %v, want %v", nums, tt.wantNums)
			}
		})
	}
}

type mockPRFetcher struct {
	prs map[int]ghPull
}

func (m *mockPRFetcher) fetchPR(_ context.Context, _, _ string, num int) (ghPull, error) {
	pr, ok := m.prs[num]
	if !ok {
		return ghPull{}, fmt.Errorf("PR #%d not found", num)
	}
	return pr, nil
}

func (m *mockPRFetcher) findPRByHead(_ context.Context, _, _, headRef string) (int, bool) {
	for _, pr := range m.prs {
		if pr.HeadRefName == headRef {
			return pr.Number, true
		}
	}
	return 0, false
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestIsPRURL(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"https://github.com/org/repo/pull/1", true},
		{"https://app.graphite.com/github/pr/org/repo/1", true},
		{"<https://github.com/org/repo/pull/1>", true},
		{"<https://app.graphite.com/github/pr/org/repo/1>", true},
		{"https://example.com/foo", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			if got := isPRURL(tt.input); got != tt.want {
				t.Errorf("isPRURL(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestBuildValidatorPrompt_ContainsCrossDependencyRule(t *testing.T) {
	prompt := buildValidatorPrompt(4, "", "", "diff content", "review content")
	if !strings.Contains(prompt, "cross-file dependenc") {
		t.Error("validator prompt should contain cross-file dependency verification rule")
	}
	if !strings.Contains(prompt, "migration") {
		t.Error("validator prompt should mention migrations as an example of cross-file dependencies")
	}
}

func TestBuildValidatorPrompt_PreservesExistingRules(t *testing.T) {
	prompt := buildValidatorPrompt(3, "## Manifest\n", "", "diff", "reviews")
	checks := []string{
		"VERIFIED",
		"INVALID: NOT IN DIFF",
		"OVERSTATED",
		"Verify counts",
		"Trace callees for severity",
		"Verify scope attribution",
	}
	for _, want := range checks {
		if !strings.Contains(prompt, want) {
			t.Errorf("validator prompt missing existing rule: %q", want)
		}
	}
}

func TestBuildMergerPrompt_ContainsOutOfScopeRule(t *testing.T) {
	prompt := buildMergerPrompt("reviews", "validated", ModeInitial, "", "", nil, DiffManifest{})
	if !strings.Contains(prompt, "OUT OF SCOPE") || !strings.Contains(prompt, "out-of-scope") {
		t.Error("merger prompt should contain out-of-scope filtering rule")
	}
}

func TestBuildMergerPrompt_PreservesExistingRules(t *testing.T) {
	prompt := buildMergerPrompt("reviews", "validated", ModeInitial, "", "", nil, DiffManifest{})
	checks := []string{
		"GO-EXPERT",
		"Deduplicate",
		"Summary scope rule",
		"No review history claims",
	}
	for _, want := range checks {
		if !strings.Contains(prompt, want) {
			t.Errorf("merger prompt missing existing rule: %q", want)
		}
	}
}

func TestBuildMergerPrompt_ModeRules(t *testing.T) {
	final := buildMergerPrompt("r", "v", ModeFinal, "", "", nil, DiffManifest{})
	if !strings.Contains(final, "FINAL REVIEW RULES") {
		t.Error("final mode should include final review rules")
	}

	reReview := buildMergerPrompt("r", "v", ModeReReview, "", "", nil, DiffManifest{})
	if !strings.Contains(reReview, "RE-REVIEW RULES") {
		t.Error("re-review mode should include re-review rules")
	}
}

func TestAgentPrompts_ContainAntiHallucinationRule(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	// Copy real agent files to temp dir
	realAgentsDir := "agents"
	entries, err := os.ReadDir(realAgentsDir)
	if err != nil {
		t.Skipf("cannot read agents dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(realAgentsDir, e.Name()))
		os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644)
	}

	agents, err := loadAgents()
	if err != nil {
		t.Fatalf("loadAgents: %v", err)
	}

	coreAgents := map[string]bool{"correctness": true, "go-expert": true, "pragmatic": true}
	for _, a := range agents {
		if !coreAgents[a.name] {
			continue
		}
		rendered, err := renderAgent(a, promptData{Diff: "test"})
		if err != nil {
			t.Fatalf("renderAgent %s: %v", a.name, err)
		}
		if !strings.Contains(rendered, "not visible in the diff") {
			t.Errorf("agent %q should contain anti-hallucination rule about not asserting content beyond the diff", a.name)
		}
	}
}

func TestAgentPrompts_ContainLineNumberGuidance(t *testing.T) {
	dir := t.TempDir()
	old := agentsDir
	agentsDir = dir
	defer func() { agentsDir = old }()

	realAgentsDir := "agents"
	entries, err := os.ReadDir(realAgentsDir)
	if err != nil {
		t.Skipf("cannot read agents dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, _ := os.ReadFile(filepath.Join(realAgentsDir, e.Name()))
		os.WriteFile(filepath.Join(dir, e.Name()), data, 0o644)
	}

	ethos := loadEthos()
	if !strings.Contains(ethos, "line number") {
		t.Error("ethos should contain line number accuracy guidance")
	}
}

func TestParseAutoReviewRepos(t *testing.T) {
	tests := []struct {
		input string
		want  []repoRef
	}{
		{"", nil},
		{"Qumulo/qompass", []repoRef{{"Qumulo", "qompass"}}},
		{"Qumulo/qompass,Qumulo/qatalyst", []repoRef{{"Qumulo", "qompass"}, {"Qumulo", "qatalyst"}}},
		{" Qumulo/qompass , Qumulo/qatalyst ", []repoRef{{"Qumulo", "qompass"}, {"Qumulo", "qatalyst"}}},
		{"bad-format", nil},
		{"Qumulo/qompass,bad,Qumulo/qatalyst", []repoRef{{"Qumulo", "qompass"}, {"Qumulo", "qatalyst"}}},
	}
	for _, tt := range tests {
		got := parseAutoReviewRepos(tt.input)
		if len(got) != len(tt.want) {
			t.Errorf("parseAutoReviewRepos(%q) = %d repos, want %d", tt.input, len(got), len(tt.want))
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("parseAutoReviewRepos(%q)[%d] = %v, want %v", tt.input, i, got[i], tt.want[i])
			}
		}
	}
}

func TestParseOpenPRsJSON(t *testing.T) {
	jsonData := `[
		{"number": 42, "url": "https://github.com/Qumulo/qompass/pull/42", "author": {"login": "alice"}, "isDraft": false},
		{"number": 99, "url": "https://github.com/Qumulo/qompass/pull/99", "author": {"login": "bob"}, "isDraft": true}
	]`
	prs, err := parseOpenPRsJSON([]byte(jsonData))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 2 {
		t.Fatalf("got %d PRs, want 2", len(prs))
	}
	if prs[0].Number != 42 || prs[0].Author != "alice" || prs[0].IsDraft {
		t.Errorf("pr[0] = %+v", prs[0])
	}
	if prs[1].Number != 99 || prs[1].Author != "bob" || !prs[1].IsDraft {
		t.Errorf("pr[1] = %+v", prs[1])
	}
}

func TestFilterUnreviewedPRs_SkipsDrafts(t *testing.T) {
	prs := []openPR{
		{Number: 1, URL: "u1", Author: "alice", IsDraft: false},
		{Number: 2, URL: "u2", Author: "bob", IsDraft: true},
		{Number: 3, URL: "u3", Author: "charlie", IsDraft: false},
	}
	got := filterUnreviewedPRs(prs, map[int]bool{})
	if len(got) != 2 {
		t.Fatalf("got %d unreviewed, want 2 (draft should be filtered)", len(got))
	}
	if got[0].Number != 1 || got[1].Number != 3 {
		t.Errorf("wrong PRs: %+v", got)
	}
}

func TestParseOpenPRsJSON_Empty(t *testing.T) {
	prs, err := parseOpenPRsJSON([]byte(`[]`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 0 {
		t.Fatalf("got %d PRs, want 0", len(prs))
	}
}

func TestHasCommentByUser(t *testing.T) {
	tests := []struct {
		name     string
		json     string
		botLogin string
		want     bool
	}{
		{
			name:     "bot has commented",
			json:     `{"comments": [{"author": {"login": "alice"}}, {"author": {"login": "qompass-pr-review-bot"}}]}`,
			botLogin: "qompass-pr-review-bot",
			want:     true,
		},
		{
			name:     "bot has not commented",
			json:     `{"comments": [{"author": {"login": "alice"}}, {"author": {"login": "bob"}}]}`,
			botLogin: "qompass-pr-review-bot",
			want:     false,
		},
		{
			name:     "no comments",
			json:     `{"comments": []}`,
			botLogin: "qompass-pr-review-bot",
			want:     false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasCommentByUser([]byte(tt.json), tt.botLogin)
			if got != tt.want {
				t.Errorf("hasCommentByUser() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilterUnreviewedPRs(t *testing.T) {
	prs := []openPR{
		{Number: 1, URL: "https://github.com/Qumulo/qompass/pull/1", Author: "alice"},
		{Number: 2, URL: "https://github.com/Qumulo/qompass/pull/2", Author: "bob"},
		{Number: 3, URL: "https://github.com/Qumulo/qompass/pull/3", Author: "charlie"},
	}
	reviewed := map[int]bool{2: true}
	got := filterUnreviewedPRs(prs, reviewed)
	if len(got) != 2 {
		t.Fatalf("got %d unreviewed, want 2", len(got))
	}
	if got[0].Number != 1 || got[1].Number != 3 {
		t.Errorf("wrong PRs filtered: %+v", got)
	}
}

func TestTimeUntilNext(t *testing.T) {
	loc := time.UTC
	tests := []struct {
		name string
		now  time.Time
		want time.Duration
	}{
		{
			name: "before midnight",
			now:  time.Date(2026, 5, 29, 22, 0, 0, 0, loc),
			want: 2 * time.Hour,
		},
		{
			name: "just after midnight",
			now:  time.Date(2026, 5, 29, 0, 0, 1, 0, loc),
			want: 24*time.Hour - time.Second,
		},
		{
			name: "at noon",
			now:  time.Date(2026, 5, 29, 12, 0, 0, 0, loc),
			want: 12 * time.Hour,
		},
		{
			name: "exactly midnight schedules next day",
			now:  time.Date(2026, 5, 29, 0, 0, 0, 0, loc),
			want: 24 * time.Hour,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := timeUntilNext(0, 0, tt.now)
			if got != tt.want {
				t.Errorf("timeUntilNext(0, 0, %v) = %v, want %v", tt.now, got, tt.want)
			}
		})
	}
}

func TestParseStalePRCandidates(t *testing.T) {
	data := `[
		{"number":10,"url":"https://github.com/O/R/pull/10","title":"old PR","body":"## Summary\nFixes the auth bug","author":{"login":"alice"},"updatedAt":"2026-05-01T00:00:00Z","isDraft":false},
		{"number":20,"url":"https://github.com/O/R/pull/20","title":"fresh PR","body":"","author":{"login":"bob"},"updatedAt":"2026-05-28T00:00:00Z","isDraft":true},
		{"number":30,"url":"https://github.com/O/R/pull/30","title":"ancient PR","body":"Long description here","author":{"login":"charlie"},"updatedAt":"2026-04-01T00:00:00Z","isDraft":false}
	]`
	prs, err := parseStalePRCandidates([]byte(data))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("got %d PRs, want 3", len(prs))
	}
	if prs[0].Title != "old PR" || prs[0].Author != "alice" || prs[0].IsDraft {
		t.Errorf("pr[0] = %+v", prs[0])
	}
	if prs[0].Body != "## Summary\nFixes the auth bug" {
		t.Errorf("pr[0].Body = %q", prs[0].Body)
	}
	if prs[1].UpdatedAt.IsZero() {
		t.Error("updatedAt should be parsed")
	}
	if !prs[1].IsDraft {
		t.Error("pr[1] should be draft")
	}
}

func TestFilterStalePRs(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	prs := []stalePRCandidate{
		{Number: 10, Title: "old", Author: "alice", Body: "Fixes stuff", UpdatedAt: now.Add(-15 * 24 * time.Hour)},
		{Number: 20, Title: "fresh", Author: "bob", UpdatedAt: now.Add(-3 * 24 * time.Hour)},
		{Number: 30, URL: "u", Title: "ancient", Author: "charlie", Body: "Big refactor", IsDraft: true, UpdatedAt: now.Add(-60 * 24 * time.Hour)},
	}
	stale := filterStalePRs(prs, 14, now)
	if len(stale) != 2 {
		t.Fatalf("got %d stale, want 2", len(stale))
	}
	if stale[0].Number != 30 {
		t.Errorf("expected most stale first, got #%d", stale[0].Number)
	}
	if stale[0].Body != "Big refactor" {
		t.Errorf("stale[0].Body = %q, want %q", stale[0].Body, "Big refactor")
	}
	if !stale[0].IsDraft {
		t.Error("stale[0] should preserve IsDraft=true from candidate")
	}
	if stale[1].Number != 10 {
		t.Errorf("expected second most stale, got #%d", stale[1].Number)
	}
}

func TestFilterStalePRs_NoneStale(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	prs := []stalePRCandidate{
		{Number: 1, UpdatedAt: now.Add(-1 * 24 * time.Hour)},
	}
	stale := filterStalePRs(prs, 14, now)
	if len(stale) != 0 {
		t.Fatalf("got %d stale, want 0", len(stale))
	}
}

func TestTruncateBody(t *testing.T) {
	tests := []struct {
		name  string
		input string
		max   int
		want  string
	}{
		{"short", "hello", 200, "hello"},
		{"empty", "", 200, ""},
		{"exact limit", "abc", 3, "abc"},
		{"truncated", "abcdef", 4, "abcd..."},
		{"strips markdown headers", "## Summary\nFixes bug", 200, "Fixes bug"},
		{"strips multiple headers", "## What\n### Why\nBecause reasons", 200, "Because reasons"},
		{"multiline keeps first meaningful", "## Summary\n\nAdds new feature\n\nMore details here", 200, "Adds new feature"},
		{"strips images", "![screenshot](http://img.png)\nActual text", 200, "Actual text"},
		{"strips checkboxes", "- [x] done\n- [ ] todo\nReal content", 200, "Real content"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := truncateBody(tt.input, tt.max)
			if got != tt.want {
				t.Errorf("truncateBody(%q, %d) = %q, want %q", tt.input, tt.max, got, tt.want)
			}
		})
	}
}

func TestFormatStalePRReport(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	stale := []stalePR{
		{Number: 10, URL: "https://github.com/O/R/pull/10", Title: "old PR", Author: "alice", DaysStale: 15, Body: "Fixes auth"},
		{Number: 30, URL: "https://github.com/O/R/pull/30", Title: "ancient PR", Author: "charlie", DaysStale: 60, Body: "Big refactor of the core module", IsDraft: true},
	}
	report := formatStalePRReport("Org/Repo", stale, now)
	if !strings.Contains(report, "Org/Repo") {
		t.Error("report should contain repo name")
	}
	if !strings.Contains(report, "old PR") {
		t.Error("report should contain PR title")
	}
	if !strings.Contains(report, "60d") {
		t.Error("report should contain days stale")
	}
	if !strings.Contains(report, "2 stale") {
		t.Error("report should contain count")
	}
	if !strings.Contains(report, "Fixes auth") {
		t.Error("report should contain PR body summary")
	}
	if !strings.Contains(report, "Big refactor") {
		t.Error("report should contain PR body summary")
	}
	if !strings.Contains(report, "[Draft]") {
		t.Error("report should annotate draft PRs with [Draft]")
	}
}

func TestFormatStalePRReport_Empty(t *testing.T) {
	report := formatStalePRReport("Org/Repo", nil, time.Now())
	if report != "" {
		t.Errorf("empty stale list should produce empty report, got %q", report)
	}
}

func TestStalePRNumbersSet(t *testing.T) {
	now := time.Date(2026, 5, 29, 0, 0, 0, 0, time.UTC)
	prs := []stalePRCandidate{
		{Number: 10, UpdatedAt: now.Add(-20 * 24 * time.Hour)},
		{Number: 20, UpdatedAt: now.Add(-3 * 24 * time.Hour)},
		{Number: 30, UpdatedAt: now.Add(-60 * 24 * time.Hour)},
	}
	stale := filterStalePRs(prs, 14, now)
	nums := stalePRNumbers(stale)
	if !nums[10] || !nums[30] {
		t.Errorf("expected 10 and 30 in stale set, got %v", nums)
	}
	if nums[20] {
		t.Error("20 should not be stale")
	}
}

func TestFilterUnreviewedPRs_SkipsStale(t *testing.T) {
	prs := []openPR{
		{Number: 1, URL: "u1"},
		{Number: 2, URL: "u2"},
		{Number: 3, URL: "u3"},
	}
	skip := map[int]bool{1: true, 3: true}
	got := filterUnreviewedPRs(prs, skip)
	if len(got) != 1 || got[0].Number != 2 {
		t.Errorf("expected only PR #2, got %+v", got)
	}
}

func TestParseStalePRDays(t *testing.T) {
	tests := []struct {
		input string
		want  int
	}{
		{"", 14},
		{"7", 7},
		{"30", 30},
		{"bad", 14},
		{"0", 14},
		{"-5", 14},
	}
	for _, tt := range tests {
		got := parseStalePRDays(tt.input)
		if got != tt.want {
			t.Errorf("parseStalePRDays(%q) = %d, want %d", tt.input, got, tt.want)
		}
	}
}

func TestSubmitReview_QueuedGetsClaudeReaction(t *testing.T) {
	mock := &mockSlack{}
	queue := NewReviewQueue(1, 5)
	defer queue.Drain()

	done := make(chan struct{})
	submitReview(mock, queue, "C123", "1234.5678", func() {
		close(done)
	})

	<-done
	time.Sleep(10 * time.Millisecond)

	mock.mu.Lock()
	defer mock.mu.Unlock()

	if len(mock.reactions) < 1 {
		t.Fatal("expected at least 1 reaction, got 0")
	}
	if mock.reactions[0] != (reactionCall{action: "add", name: "claude"}) {
		t.Errorf("first reaction = %+v, want add:claude", mock.reactions[0])
	}
	if mock.reactions[1] != (reactionCall{action: "remove", name: "claude"}) {
		t.Errorf("second reaction = %+v, want remove:claude", mock.reactions[1])
	}
}

func TestSubmitReview_QueueFullGetsEmoji(t *testing.T) {
	mock := &mockSlack{}
	queue := NewReviewQueue(1, 0)

	block := make(chan struct{})
	queue.Submit(func() { <-block })

	submitReview(mock, queue, "C123", "1234.5678", func() {})

	close(block)
	queue.Drain()

	mock.mu.Lock()
	defer mock.mu.Unlock()

	wantReactions := []reactionCall{
		{action: "add", name: "claude"},
		{action: "remove", name: "claude"},
		{action: "add", name: "hourglass_flowing_sand"},
	}
	if len(mock.reactions) != len(wantReactions) {
		t.Fatalf("got %d reactions, want %d: %+v", len(mock.reactions), len(wantReactions), mock.reactions)
	}
	for i, want := range wantReactions {
		if mock.reactions[i] != want {
			t.Errorf("reaction[%d] = %+v, want %+v", i, mock.reactions[i], want)
		}
	}

	if len(mock.messages) != 1 {
		t.Fatalf("expected 1 message, got %d", len(mock.messages))
	}
}
