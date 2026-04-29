package main

import (
	"errors"
	"fmt"
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
	trackReview("ts123", func() { called = true })

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
