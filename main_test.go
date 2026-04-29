package main

import (
	"errors"
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
