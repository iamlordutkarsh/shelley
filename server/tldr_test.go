package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/llm"
)

func TestNeedsTLDR(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"empty", "", false},
		{"short", "Done.", false},
		{"many short sentences", "One. Two. Three. Four. Five.", false},
		{"long no punctuation", strings.Repeat("a", 300), true},
		{"long with whitespace padding", "  " + strings.Repeat("x", 241) + "  ", true},
		{"just under threshold", strings.Repeat("a", 240), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := needsTLDR(tc.in); got != tc.want {
				t.Errorf("needsTLDR(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestPickTLDRTarget(t *testing.T) {
	t.Parallel()

	makeAgentMsg := func(id, text string) generated.Message {
		llmMsg := llm.Message{
			Role:    llm.MessageRoleAssistant,
			Content: []llm.Content{{Type: llm.ContentTypeText, Text: text}},
		}
		b, _ := json.Marshal(llmMsg)
		s := string(b)
		return generated.Message{
			MessageID: id,
			Type:      string(db.MessageTypeAgent),
			LlmData:   &s,
		}
	}
	makeToolOnly := func(id string) generated.Message {
		llmMsg := llm.Message{
			Role: llm.MessageRoleAssistant,
			Content: []llm.Content{{
				Type:     llm.ContentTypeToolUse,
				ToolName: "bash",
			}},
		}
		b, _ := json.Marshal(llmMsg)
		s := string(b)
		return generated.Message{
			MessageID: id,
			Type:      string(db.MessageTypeAgent),
			LlmData:   &s,
		}
	}

	t.Run("picks newest with text (input is newest-first)", func(t *testing.T) {
		msgs := []generated.Message{
			makeAgentMsg("newest", "All done. Tests pass."),
			makeAgentMsg("older", "Working on it."),
		}
		m, text := pickTLDRTarget(msgs)
		if m == nil || m.MessageID != "newest" {
			t.Fatalf("want newest, got %+v", m)
		}
		if text != "All done. Tests pass." {
			t.Fatalf("unexpected text %q", text)
		}
	})

	t.Run("skips tool-only newest", func(t *testing.T) {
		msgs := []generated.Message{
			makeToolOnly("newest"),
			makeAgentMsg("older", "Real text here."),
		}
		m, text := pickTLDRTarget(msgs)
		if m == nil || m.MessageID != "older" {
			t.Fatalf("want older, got %+v", m)
		}
		if text != "Real text here." {
			t.Fatalf("unexpected text %q", text)
		}
	})

	t.Run("all tool only", func(t *testing.T) {
		msgs := []generated.Message{makeToolOnly("a"), makeToolOnly("b")}
		m, _ := pickTLDRTarget(msgs)
		if m != nil {
			t.Fatalf("want nil, got %+v", m)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		if m, _ := pickTLDRTarget(nil); m != nil {
			t.Fatalf("want nil")
		}
	})
}

func TestAttachTLDR_MergesAndBroadcasts(t *testing.T) {
	t.Parallel()
	srv, database, _ := newTestServer(t)

	ctx := context.Background()
	conv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}

	// Existing user_data has some other key; tldr must be merged in.
	llmMsg := llm.Message{
		Role:    llm.MessageRoleAssistant,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: "Long agent response."}},
	}
	msg, err := database.CreateMessage(ctx, db.CreateMessageParams{
		ConversationID: conv.ConversationID,
		Type:           db.MessageTypeAgent,
		LLMData:        llmMsg,
		UserData:       map[string]any{"foo": "bar"},
	})
	if err != nil {
		t.Fatalf("create message: %v", err)
	}

	if err := srv.attachTLDR(ctx, conv.ConversationID, msg.MessageID, "Short and sweet."); err != nil {
		t.Fatalf("attachTLDR: %v", err)
	}

	got, err := database.GetMessageByID(ctx, msg.MessageID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.UserData == nil {
		t.Fatal("user_data nil")
	}
	var data map[string]any
	if err := json.Unmarshal([]byte(*got.UserData), &data); err != nil {
		t.Fatalf("unmarshal user_data: %v", err)
	}
	if data["tldr"] != "Short and sweet." {
		t.Errorf("tldr = %v, want %q", data["tldr"], "Short and sweet.")
	}
	if data["foo"] != "bar" {
		t.Errorf("merge lost existing key: foo = %v", data["foo"])
	}
}

func TestEndOfTurnTLDR_AttachedForLongResponse(t *testing.T) {
	t.Skip("TL;DR generation is currently disabled at the call site in server.go")
	t.Parallel()
	h := NewTestHarness(t)
	long := strings.Repeat("This is a long sentence that takes a fair bit of space. ", 8)
	h.NewConversation("echo: "+long, "/tmp").WaitResponse()

	// The TL;DR is attached asynchronously after the turn ends.
	deadline := time.Now().Add(5 * time.Second)
	var found string
	for time.Now().Before(deadline) {
		var messages []generated.Message
		if err := h.db.Queries(context.Background(), func(q *generated.Queries) error {
			var qerr error
			messages, qerr = q.ListMessages(context.Background(), h.convID)
			return qerr
		}); err != nil {
			t.Fatalf("list messages: %v", err)
		}
		for _, m := range messages {
			if m.Type != string(db.MessageTypeAgent) || m.UserData == nil {
				continue
			}
			var ud map[string]any
			if err := json.Unmarshal([]byte(*m.UserData), &ud); err != nil {
				continue
			}
			if v, ok := ud["tldr"].(string); ok && v != "" {
				found = v
				break
			}
		}
		if found != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if found == "" {
		t.Fatalf("expected tldr to be attached to an agent message; none found")
	}
}
