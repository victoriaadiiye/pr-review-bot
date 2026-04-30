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
