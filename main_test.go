package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

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
		{action: "remove", name: "eyes"},
		{action: "add", name: "x"},
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
		{action: "remove", name: "eyes"},
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

func TestSessionStore_GetSet(t *testing.T) {
	dir := t.TempDir()
	s := &SessionStore{
		path: filepath.Join(dir, "sessions.json"),
		data: make(map[string]string),
	}

	prURL := "https://github.com/org/repo/pull/42"

	if got := s.Get(prURL); got != "" {
		t.Errorf("Get on empty store = %q, want empty", got)
	}

	s.Set(prURL, "session-abc-123")
	if got := s.Get(prURL); got != "session-abc-123" {
		t.Errorf("Get after Set = %q, want %q", got, "session-abc-123")
	}
}

func TestSessionStore_Overwrites(t *testing.T) {
	dir := t.TempDir()
	s := &SessionStore{
		path: filepath.Join(dir, "sessions.json"),
		data: make(map[string]string),
	}

	prURL := "https://github.com/org/repo/pull/42"
	s.Set(prURL, "session-1")
	s.Set(prURL, "session-2")

	if got := s.Get(prURL); got != "session-2" {
		t.Errorf("Get after overwrite = %q, want %q", got, "session-2")
	}
}

func TestSessionStore_PersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	s := &SessionStore{
		path: path,
		data: make(map[string]string),
	}

	s.Set("https://github.com/org/repo/pull/1", "sess-aaa")
	s.Set("https://github.com/org/repo/pull/2", "sess-bbb")

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read persisted file: %v", err)
	}
	var ondisk map[string]string
	if err := json.Unmarshal(raw, &ondisk); err != nil {
		t.Fatalf("unmarshal persisted file: %v", err)
	}
	if ondisk["https://github.com/org/repo/pull/1"] != "sess-aaa" {
		t.Errorf("disk PR 1 = %q, want sess-aaa", ondisk["https://github.com/org/repo/pull/1"])
	}
	if ondisk["https://github.com/org/repo/pull/2"] != "sess-bbb" {
		t.Errorf("disk PR 2 = %q, want sess-bbb", ondisk["https://github.com/org/repo/pull/2"])
	}
}

func TestSessionStore_LoadsFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")

	seed := map[string]string{
		"https://github.com/org/repo/pull/99": "sess-from-disk",
	}
	raw, _ := json.Marshal(seed)
	os.WriteFile(path, raw, 0o644)

	s := &SessionStore{path: path, data: make(map[string]string)}
	if diskRaw, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(diskRaw, &s.data)
	}

	if got := s.Get("https://github.com/org/repo/pull/99"); got != "sess-from-disk" {
		t.Errorf("Get from loaded store = %q, want %q", got, "sess-from-disk")
	}
}

func TestSessionStore_MultiplePRs(t *testing.T) {
	dir := t.TempDir()
	s := &SessionStore{
		path: filepath.Join(dir, "sessions.json"),
		data: make(map[string]string),
	}

	s.Set("https://github.com/org/repo/pull/1", "sess-1")
	s.Set("https://github.com/org/repo/pull/2", "sess-2")
	s.Set("https://github.com/other/repo/pull/1", "sess-3")

	if got := s.Get("https://github.com/org/repo/pull/1"); got != "sess-1" {
		t.Errorf("PR 1 = %q, want sess-1", got)
	}
	if got := s.Get("https://github.com/org/repo/pull/2"); got != "sess-2" {
		t.Errorf("PR 2 = %q, want sess-2", got)
	}
	if got := s.Get("https://github.com/other/repo/pull/1"); got != "sess-3" {
		t.Errorf("other/repo PR 1 = %q, want sess-3", got)
	}
	if got := s.Get("https://github.com/org/repo/pull/999"); got != "" {
		t.Errorf("nonexistent PR = %q, want empty", got)
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

func TestModePreamble(t *testing.T) {
	if modePreamble(ModeInitial) != "" {
		t.Error("initial mode should have empty preamble")
	}
	if modePreamble(ModeQuick) != "" {
		t.Error("quick mode should have empty preamble")
	}
	if !strings.Contains(modePreamble(ModeReReview), "RE-REVIEW") {
		t.Error("re-review preamble should contain RE-REVIEW")
	}
	if !strings.Contains(modePreamble(ModeFinal), "FINAL REVIEW") {
		t.Error("final preamble should contain FINAL REVIEW")
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
		"design":      false,
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
		if !strings.Contains(rendered, data.PRURL) {
			t.Errorf("agent %s: rendered output missing PRURL", a.name)
		}
		if !strings.Contains(rendered, data.Diff) {
			t.Errorf("agent %s: rendered output missing Diff", a.name)
		}
		if strings.Contains(rendered, "{{") {
			t.Errorf("agent %s: unrendered template syntax in output", a.name)
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
