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
	"shelley.exe.dev/loop"
)

// subagentDoneFixture sets up a parent conversation with an active manager and
// a child subagent conversation whose manager has the onDone callback wired up
// by getOrCreateSubagentConversationManager. It records a final assistant text
// message into the subagent's DB so notifyParentSubagentDone has something to
// splice into the parent's history.
type subagentDoneFixture struct {
	t        *testing.T
	server   *Server
	database *db.DB
	llmSvc   *loop.PredictableService

	parentID    string
	parentMgr   *ConversationManager
	subagentID  string
	subagentMgr *ConversationManager
	subSlug     string
	subResponse string
}

func newSubagentDoneFixture(t *testing.T, subResponse string) *subagentDoneFixture {
	t.Helper()
	server, database, ps := newTestServer(t)

	ctx := context.Background()

	// Parent conversation.
	parentConv, err := database.CreateConversation(ctx, nil, true, nil, nil, db.ConversationOptions{})
	if err != nil {
		t.Fatalf("create parent: %v", err)
	}

	parentMgr, err := server.getOrCreateConversationManager(ctx, parentConv.ConversationID, "")
	if err != nil {
		t.Fatalf("get parent manager: %v", err)
	}

	// Subagent conversation, parented to the above. Use CreateSubagentConversation
	// so ParentConversationID is set; that's what notifyParentSubagentDone keys off.
	slug := "sub-test"
	subConv, err := database.CreateSubagentConversation(ctx, slug, parentConv.ConversationID, nil)
	if err != nil {
		t.Fatalf("create subagent conv: %v", err)
	}

	subagentMgr, err := server.getOrCreateSubagentConversationManager(ctx, subConv.ConversationID)
	if err != nil {
		t.Fatalf("get subagent manager: %v", err)
	}

	// Record a final assistant text message on the subagent so lastAssistantText
	// has something to read.
	assistantMsg := llm.Message{
		Role:      llm.MessageRoleAssistant,
		Content:   []llm.Content{{Type: llm.ContentTypeText, Text: subResponse}},
		EndOfTurn: true,
	}
	if err := server.recordMessage(ctx, subConv.ConversationID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record subagent assistant: %v", err)
	}

	return &subagentDoneFixture{
		t:           t,
		server:      server,
		database:    database,
		llmSvc:      ps,
		parentID:    parentConv.ConversationID,
		parentMgr:   parentMgr,
		subagentID:  subConv.ConversationID,
		subagentMgr: subagentMgr,
		subSlug:     slug,
		subResponse: subResponse,
	}
}

// parentMessages returns the list of persisted messages for the parent in DB
// order (ascending sequence id).
func (f *subagentDoneFixture) parentMessages() []generated.Message {
	f.t.Helper()
	var msgs []generated.Message
	err := f.database.Queries(context.Background(), func(q *generated.Queries) error {
		var qerr error
		msgs, qerr = q.ListMessages(context.Background(), f.parentID)
		return qerr
	})
	if err != nil {
		f.t.Fatalf("list parent messages: %v", err)
	}
	return msgs
}

// findSyntheticPair scans parent messages for a subagent tool_use immediately
// followed by a matching tool_result, returning both decoded llm.Message values
// and whether they were found.
func (f *subagentDoneFixture) findSyntheticPair() (use, result llm.Message, ok bool) {
	f.t.Helper()
	msgs := f.parentMessages()
	for i := 0; i+1 < len(msgs); i++ {
		if msgs[i].LlmData == nil || msgs[i+1].LlmData == nil {
			continue
		}
		var m1, m2 llm.Message
		if err := json.Unmarshal([]byte(*msgs[i].LlmData), &m1); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(*msgs[i+1].LlmData), &m2); err != nil {
			continue
		}
		var useID string
		for _, c := range m1.Content {
			if c.Type == llm.ContentTypeToolUse && c.ToolName == "subagent" {
				useID = c.ID
				break
			}
		}
		if useID == "" {
			continue
		}
		for _, c := range m2.Content {
			if c.Type == llm.ContentTypeToolResult && c.ToolUseID == useID {
				return m1, m2, true
			}
		}
	}
	return llm.Message{}, llm.Message{}, false
}

// fireOnDone simulates the agent transitioning from working to not working
// (which is what triggers the onDone callback wired in convo.go's
// SetAgentWorking). We toggle through true->false to exercise the real path.
func (f *subagentDoneFixture) fireOnDone() {
	f.subagentMgr.SetAgentWorking(true)
	f.subagentMgr.SetAgentWorking(false)
}

func TestSubagentDone(t *testing.T) {
	t.Run("HappyPath_WaitFalse_NotifiesIdleParent", testSubagentDone_HappyPath)
	t.Run("SuppressedWhileParentBusy", testSubagentDone_SuppressedParentBusy)
	t.Run("SuppressedAfterWaitResultRecorded", testSubagentDone_SuppressedAfterWaitResultRecorded)
	t.Run("TimeoutResultStillNotifiesOnCompletion", testSubagentDone_TimeoutResultStillNotifiesOnCompletion)
	t.Run("TimeoutMarkedBeforeToolResultStillNotifies", testSubagentDone_TimeoutMarkedBeforeToolResultStillNotifies)
	t.Run("LiteralTimeoutTextDoesNotCountAsTimeout", testSubagentDone_LiteralTimeoutTextDoesNotCountAsTimeout)
	t.Run("RenamedSlugTimeoutStillNotifies", testSubagentDone_RenamedSlugTimeoutStillNotifies)
	t.Run("LaterPromptDoesNotClearTimeoutMarker", testSubagentDone_LaterPromptDoesNotClearTimeoutMarker)
	t.Run("SynchronousCompletionClearsStaleTimeoutMarker", testSubagentDone_SynchronousCompletionClearsStaleTimeoutMarker)
	t.Run("QueuedDuringDistillation", testSubagentDone_QueuedDuringDistillation)
	t.Run("WakesIdleParentLoop", testSubagentDone_WakesIdleLoop)
	t.Run("ToolResultCorrectness", testSubagentDone_ToolResultCorrectness)
	t.Run("ConcurrentSubagentFinishes_BothPairsAtomic", testSubagentDone_ConcurrentFinishes)
	t.Run("LastMessageIsToolUse_FallsBackGracefully", testSubagentDone_LastMessageIsToolUse)
	t.Run("GitInfoMessageDoesNotLeakIntoNotification", testSubagentDone_GitInfoIgnored)
}

// 1. Happy path: parent is idle (agentWorking=false), subagent finishes; two
// new persisted messages appear (synthetic tool_use + matching tool_result),
// and the parent's agentWorking flips to true.
func testSubagentDone_HappyPath(t *testing.T) {
	f := newSubagentDoneFixture(t, "Subagent finished successfully.")

	before := len(f.parentMessages())
	if f.parentMgr.IsAgentWorking() {
		t.Fatalf("precondition: parent should not be working")
	}

	f.fireOnDone()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(f.parentMessages()) >= before+2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if n := len(f.parentMessages()); n < before+2 {
		t.Fatalf("timeout: parent had %d msgs (before=%d, want>=%d). messages:\n%s", n, before, before+2, dumpMessages(t, f.parentMessages()))
	}
	t.Logf("happy path: parent had %d msgs after (before=%d)", len(f.parentMessages()), before)

	use, result, ok := f.findSyntheticPair()
	if !ok {
		t.Fatalf("expected synthetic tool_use/tool_result pair in parent\nmessages: %s", dumpMessages(t, f.parentMessages()))
	}

	// Inspect tool_use
	var useID, toolName string
	for _, c := range use.Content {
		if c.Type == llm.ContentTypeToolUse {
			useID = c.ID
			toolName = c.ToolName
		}
	}
	if toolName != "subagent" {
		t.Errorf("expected tool name=subagent, got %q", toolName)
	}
	if useID == "" {
		t.Errorf("tool_use ID was empty")
	}

	// Inspect tool_result
	var gotText string
	var gotUseID string
	for _, c := range result.Content {
		if c.Type == llm.ContentTypeToolResult {
			gotUseID = c.ToolUseID
			for _, r := range c.ToolResult {
				if r.Type == llm.ContentTypeText {
					gotText = r.Text
				}
			}
		}
	}
	if gotUseID != useID {
		t.Errorf("tool_result ToolUseID=%q, want %q", gotUseID, useID)
	}
	if !strings.Contains(gotText, f.subResponse) {
		t.Errorf("tool_result text=%q does not contain subagent response %q", gotText, f.subResponse)
	}

	// Parent's loop should be driven: the predictable LLM service must see a
	// request whose last message is the synthetic tool_result. (Polling for
	// agentWorking==true is racy because the predictable model finishes the
	// turn faster than the test's polling interval.)
	waitFor(t, 5*time.Second, func() bool {
		last := f.llmSvc.GetLastRequest()
		if last == nil || len(last.Messages) < 2 {
			return false
		}
		cur := last.Messages[len(last.Messages)-1]
		for _, c := range cur.Content {
			if c.Type == llm.ContentTypeToolResult && strings.Contains(toolResultText(c), f.subResponse) {
				return true
			}
		}
		return false
	})
}

func toolResultText(c llm.Content) string {
	var sb strings.Builder
	for _, r := range c.ToolResult {
		if r.Type == llm.ContentTypeText {
			sb.WriteString(r.Text)
		}
	}
	return sb.String()
}

// 2. Parent busy: if parent.agentWorking is true at the moment the subagent
// finishes, no notification messages are added to the parent.
// SuppressedWhileParentBusy: the suppression rule is no longer "any
// parentBusy". It's specifically "the parent has a pending subagent tool
// call targeting THIS subagent" — i.e. the wait=true in-flight tool-call
// case where the parent's tool call will deliver the response on its own
// and a synthetic pair would duplicate. We simulate that here by
// recording an unanswered subagent tool_use targeting our subagent slug.
func testSubagentDone_SuppressedParentBusy(t *testing.T) {
	f := newSubagentDoneFixture(t, "Some response.")

	// Record a pending subagent tool_use against the parent.
	pendingInput, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	pendingMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        "toolu_pending_user_initiated",
			ToolName:  "subagent",
			ToolInput: pendingInput,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, pendingMsg, llm.Usage{}); err != nil {
		t.Fatalf("record pending tool_use: %v", err)
	}

	before := len(f.parentMessages())
	f.server.notifyParentSubagentDone(f.subagentID)
	if got := len(f.parentMessages()); got != before {
		t.Fatalf("expected no new parent messages while a subagent tool call is pending, got %d new", got-before)
	}
	if hasSyntheticDonePair(t, f.parentMessages()) {
		t.Fatalf("expected no synthetic pair when parent has a pending subagent tool call")
	}
}

func testSubagentDone_SuppressedAfterWaitResultRecorded(t *testing.T) {
	f := newSubagentDoneFixture(t, "Synchronous wait result.")

	toolUseID := "toolu_wait_done"
	pendingInput, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: pendingInput,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: "Subagent 'sub-test' response:\nSynchronous wait result.",
			}},
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, toolResultMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_result: %v", err)
	}

	before := len(f.parentMessages())
	f.server.notifyParentSubagentDone(f.subagentID)
	if got := len(f.parentMessages()); got != before {
		t.Fatalf("expected no new parent messages after wait=true already recorded a tool_result, got %d new", got-before)
	}
	if hasSyntheticDonePair(t, f.parentMessages()) {
		t.Fatalf("expected no synthetic subagent-done pair after wait=true already recorded a tool_result")
	}
}

func testSubagentDone_TimeoutResultStillNotifiesOnCompletion(t *testing.T) {
	f := newSubagentDoneFixture(t, "Finished after timeout.")

	toolUseID := "toolu_wait_timeout"
	input, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: input,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: "Subagent 'sub-test' response:\n[Subagent is still working (timeout reached). Progress summary:]\nStill working.",
			}},
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, toolResultMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_result: %v", err)
	}

	f.server.notifyParentSubagentDone(f.subagentID)

	waitFor(t, 5*time.Second, func() bool {
		return hasSyntheticDonePair(t, f.parentMessages())
	})
}

func testSubagentDone_TimeoutMarkedBeforeToolResultStillNotifies(t *testing.T) {
	f := newSubagentDoneFixture(t, "Finished after timeout before parent recorded the timeout result.")

	input, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        "toolu_wait_timeout_pending_result",
			ToolName:  "subagent",
			ToolInput: input,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	f.server.markSubagentWaitTimedOut(f.subagentID)

	f.server.notifyParentSubagentDone(f.subagentID)

	waitFor(t, 5*time.Second, func() bool {
		return hasSyntheticDonePair(t, f.parentMessages())
	})
}

func testSubagentDone_LiteralTimeoutTextDoesNotCountAsTimeout(t *testing.T) {
	f := newSubagentDoneFixture(t, "Already delivered, but mentioned timeout reached literally.")

	toolUseID := "toolu_wait_literal_timeout"
	input, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: input,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: "Subagent 'sub-test' response:\nThe phrase timeout reached appears in this real answer.",
			}},
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, toolResultMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_result: %v", err)
	}

	before := len(f.parentMessages())
	f.server.notifyParentSubagentDone(f.subagentID)
	if got := len(f.parentMessages()); got != before {
		t.Fatalf("expected no new parent messages for a real wait=true result mentioning timeout text, got %d new", got-before)
	}
}

func testSubagentDone_RenamedSlugTimeoutStillNotifies(t *testing.T) {
	f := newSubagentDoneFixture(t, "Finished after renamed-slug timeout.")

	toolUseID := "toolu_wait_renamed_timeout"
	input, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "go", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: input,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: "Subagent 'sub-test' response: (Note: slug was changed to 'sub-test' for uniqueness. Use 'sub-test' for future messages to this subagent.)\n[Subagent is still working (timeout reached). Progress summary:]\nStill working.",
			}},
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, toolResultMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_result: %v", err)
	}

	f.server.notifyParentSubagentDone(f.subagentID)

	waitFor(t, 5*time.Second, func() bool {
		return hasSyntheticDonePair(t, f.parentMessages())
	})
}

func testSubagentDone_LaterPromptDoesNotClearTimeoutMarker(t *testing.T) {
	f := newSubagentDoneFixture(t, "Earlier timed-out run finished.")

	f.server.markSubagentWaitTimedOut(f.subagentID)
	if err := f.server.recordMessage(context.Background(), f.parentID, llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        "toolu_later_wait_same_slug",
			ToolName:  "subagent",
			ToolInput: []byte(`{"slug":"sub-test","prompt":"new work","wait":true}`),
		}},
	}, llm.Usage{}); err != nil {
		t.Fatalf("record later tool_use: %v", err)
	}

	f.server.notifyParentSubagentDone(f.subagentID)

	waitFor(t, 5*time.Second, func() bool {
		return hasSyntheticDonePair(t, f.parentMessages())
	})
}

func testSubagentDone_SynchronousCompletionClearsStaleTimeoutMarker(t *testing.T) {
	f := newSubagentDoneFixture(t, "Synchronous later run already returned.")

	f.server.markSubagentWaitTimedOut(f.subagentID)
	f.server.clearSubagentWaitTimedOut(f.subagentID)
	toolUseID := "toolu_sync_after_stale_marker"
	input, _ := json.Marshal(map[string]any{"slug": f.subSlug, "prompt": "new work", "wait": true})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: input,
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, assistantMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use: %v", err)
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: "Subagent 'sub-test' response:\nSynchronous later run already returned.",
			}},
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.parentID, toolResultMsg, llm.Usage{}); err != nil {
		t.Fatalf("record tool_result: %v", err)
	}

	before := len(f.parentMessages())
	f.server.notifyParentSubagentDone(f.subagentID)
	if got := len(f.parentMessages()); got != before {
		t.Fatalf("expected no new parent messages after stale marker was cleared by synchronous completion, got %d new", got-before)
	}
}

func hasSyntheticDonePair(t *testing.T, msgs []generated.Message) bool {
	t.Helper()
	for i := 0; i+1 < len(msgs); i++ {
		if msgs[i].LlmData == nil || msgs[i+1].LlmData == nil {
			continue
		}
		var use, result llm.Message
		if err := json.Unmarshal([]byte(*msgs[i].LlmData), &use); err != nil {
			continue
		}
		if err := json.Unmarshal([]byte(*msgs[i+1].LlmData), &result); err != nil {
			continue
		}
		var useID string
		for _, c := range use.Content {
			if c.Type == llm.ContentTypeToolUse && c.ToolName == "subagent" && strings.HasPrefix(c.ID, "sa_done_") {
				useID = c.ID
			}
		}
		if useID == "" {
			continue
		}
		for _, c := range result.Content {
			if c.Type == llm.ContentTypeToolResult && c.ToolUseID == useID {
				return true
			}
		}
	}
	return false
}

// 3. Parent distilling: notification waits in the pending-batch queue until
// distillation ends, then drains. No more drop-on-distilling — the single
// queue serializes subagent-done batches with distillation just like it
// already does for user messages.
func testSubagentDone_QueuedDuringDistillation(t *testing.T) {
	f := newSubagentDoneFixture(t, "Some response.")

	f.parentMgr.SetDistilling(true)

	before := len(f.parentMessages())
	f.server.notifyParentSubagentDone(f.subagentID)

	// While distilling, the synthetic pair must NOT yet be persisted.
	if got := len(f.parentMessages()); got != before {
		t.Fatalf("expected no new parent messages while distilling, got %d new", got-before)
	}
	if _, _, ok := f.findSyntheticPair(); ok {
		t.Fatalf("unexpected synthetic pair recorded while distilling")
	}

	// End distillation — this is what runDistillNewGeneration's defer does
	// in real code (SetDistilling(false) then drainPendingMessages).
	f.parentMgr.SetDistilling(false)
	go f.parentMgr.drainPendingMessages(f.server)

	// Now the synthetic pair must show up.
	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := f.findSyntheticPair()
		return ok
	})
}

// 4. Wakes idle parent: tear down the parent's loop, finish the subagent.
// notifyParentSubagentDone must Hydrate + ensureLoop the parent back up,
// persist the synthetic pair to the DB, queue them onto the loop, and the
// loop must actually fire an LLM request whose last two history entries are
// the synthetic tool_use and tool_result.
func testSubagentDone_WakesIdleLoop(t *testing.T) {
	f := newSubagentDoneFixture(t, "Background work done.")

	// Bring the parent's loop up once with the predictable service so model is
	// recorded and toolset is built; then drop it as if the parent went idle.
	if err := f.parentMgr.ensureLoop(f.llmSvc, "predictable"); err != nil {
		t.Fatalf("ensureLoop initial: %v", err)
	}
	f.parentMgr.ResetLoop()

	f.parentMgr.mu.Lock()
	hasLoop := f.parentMgr.loop != nil
	f.parentMgr.mu.Unlock()
	if hasLoop {
		t.Fatalf("expected parent loop to be torn down")
	}

	f.llmSvc.ClearRequests()

	f.fireOnDone()

	// Persisted synthetic pair shows up.
	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := f.findSyntheticPair()
		return ok
	})

	// Parent loop should be running again.
	waitFor(t, 5*time.Second, func() bool {
		f.parentMgr.mu.Lock()
		defer f.parentMgr.mu.Unlock()
		return f.parentMgr.loop != nil
	})

	// And the predictable LLM service should have been invoked, with the last
	// two history entries being our synthetic tool_use and tool_result.
	waitFor(t, 5*time.Second, func() bool {
		reqs := f.llmSvc.GetRecentRequests()
		if len(reqs) == 0 {
			return false
		}
		last := reqs[len(reqs)-1]
		if len(last.Messages) < 2 {
			return false
		}
		n := len(last.Messages)
		prev := last.Messages[n-2]
		cur := last.Messages[n-1]
		var prevUseID string
		for _, c := range prev.Content {
			if c.Type == llm.ContentTypeToolUse && c.ToolName == "subagent" {
				prevUseID = c.ID
			}
		}
		if prevUseID == "" {
			return false
		}
		for _, c := range cur.Content {
			if c.Type == llm.ContentTypeToolResult && c.ToolUseID == prevUseID {
				return true
			}
		}
		return false
	})
}

// 5. Tool result correctness: tool_use ID matches tool_result ToolUseID, and
// long subagent text is truncated to 500 chars (plus suffix) in the tool_result.
func testSubagentDone_ToolResultCorrectness(t *testing.T) {
	long := strings.Repeat("x", 800)
	f := newSubagentDoneFixture(t, long)

	f.fireOnDone()

	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := f.findSyntheticPair()
		return ok
	})

	use, result, ok := f.findSyntheticPair()
	if !ok {
		t.Fatalf("missing synthetic pair")
	}

	var useID string
	for _, c := range use.Content {
		if c.Type == llm.ContentTypeToolUse {
			useID = c.ID
		}
	}
	if useID == "" || !strings.HasPrefix(useID, "sa_done_") {
		t.Errorf("unexpected tool_use ID: %q", useID)
	}

	var gotText, gotUseID string
	for _, c := range result.Content {
		if c.Type == llm.ContentTypeToolResult {
			gotUseID = c.ToolUseID
			for _, r := range c.ToolResult {
				if r.Type == llm.ContentTypeText {
					gotText = r.Text
				}
			}
		}
	}
	if gotUseID != useID {
		t.Errorf("ToolUseID mismatch: use=%q result=%q", useID, gotUseID)
	}

	// The full 800-char text must not be present verbatim; the truncated body
	// (first 500 "x"s + suffix) must be present.
	if strings.Contains(gotText, long) {
		t.Errorf("expected subagent text to be truncated, but full 800-char body is present")
	}
	if !strings.Contains(gotText, strings.Repeat("x", 500)) {
		t.Errorf("expected first 500 chars of subagent text to be present, got %q", truncForLog(gotText))
	}
	if !strings.Contains(gotText, "...") {
		t.Errorf("expected truncation suffix '...' in tool_result text")
	}
}

func truncForLog(s string) string {
	if len(s) > 120 {
		return s[:120] + "..."
	}
	return s
}

func dumpMessages(t *testing.T, msgs []generated.Message) string {
	t.Helper()
	var sb strings.Builder
	for i, m := range msgs {
		data := "<nil>"
		if m.LlmData != nil {
			data = *m.LlmData
		}
		sb.WriteString("[")
		b, _ := json.Marshal(i)
		sb.Write(b)
		sb.WriteString("]")
		sb.WriteString(" type=")
		sb.WriteString(m.Type)
		sb.WriteString(" data=")
		sb.WriteString(truncForLog(data))
		sb.WriteString("\n")
	}
	return sb.String()
}

// testSubagentDone_ConcurrentFinishes verifies that when two subagents under
// the same parent finish concurrently, both synthetic tool_use/tool_result
// pairs land in the parent's history and each pair is atomic: a tool_use is
// immediately followed by its matching tool_result with no interleaving.
// LLM APIs require tool_use blocks be paired with their tool_result before
// any other content; if the messages were globally racey we could end up
// with [useA, useB, resultA, resultB] which would be rejected.
func testSubagentDone_ConcurrentFinishes(t *testing.T) {
	f := newSubagentDoneFixture(t, "alpha done")

	ctx := context.Background()
	subConv2, err := f.database.CreateSubagentConversation(ctx, "sub-test-2", f.parentID, nil)
	if err != nil {
		t.Fatalf("create subagent 2: %v", err)
	}
	sub2Mgr, err := f.server.getOrCreateSubagentConversationManager(ctx, subConv2.ConversationID)
	if err != nil {
		t.Fatalf("get subagent 2 manager: %v", err)
	}
	if err := f.server.recordMessage(ctx, subConv2.ConversationID, llm.Message{
		Role:      llm.MessageRoleAssistant,
		Content:   []llm.Content{{Type: llm.ContentTypeText, Text: "beta done"}},
		EndOfTurn: true,
	}, llm.Usage{}); err != nil {
		t.Fatalf("record subagent 2 assistant: %v", err)
	}

	// Prime working=true so the false transition fires onDone.
	f.subagentMgr.SetAgentWorking(true)
	sub2Mgr.SetAgentWorking(true)

	go f.subagentMgr.SetAgentWorking(false)
	go sub2Mgr.SetAgentWorking(false)

	// Either both pairs land (each = 2 msgs), or the parent loop has woken
	// and started churning. We just want >=4 messages in parent.
	// Wait until we see two distinct synthetic tool_use blocks each followed
	// by their matching tool_result. Just waiting on a message count can
	// catch an intermediate state where the parent's loop has interleaved
	// its own LLM turn between the two pairs.
	waitFor(t, 5*time.Second, func() bool {
		msgs := f.parentMessages()
		found := 0
		for i, m := range msgs {
			if m.LlmData == nil {
				continue
			}
			var lm llm.Message
			if err := json.Unmarshal([]byte(*m.LlmData), &lm); err != nil {
				continue
			}
			var useID string
			for _, c := range lm.Content {
				if c.Type == llm.ContentTypeToolUse && c.ToolName == "subagent" && strings.HasPrefix(c.ID, "sa_done_") {
					useID = c.ID
				}
			}
			if useID == "" {
				continue
			}
			if i+1 >= len(msgs) || msgs[i+1].LlmData == nil {
				continue
			}
			var next llm.Message
			if err := json.Unmarshal([]byte(*msgs[i+1].LlmData), &next); err != nil {
				continue
			}
			for _, c := range next.Content {
				if c.Type == llm.ContentTypeToolResult && c.ToolUseID == useID {
					found++
				}
			}
		}
		return found >= 2
	})

	msgs := f.parentMessages()

	type found struct {
		useIdx int
		useID  string
		text   string
	}
	var pairs []found
	for i, m := range msgs {
		if m.LlmData == nil {
			continue
		}
		var lm llm.Message
		if err := json.Unmarshal([]byte(*m.LlmData), &lm); err != nil {
			continue
		}
		var useID string
		for _, c := range lm.Content {
			if c.Type == llm.ContentTypeToolUse && c.ToolName == "subagent" && strings.HasPrefix(c.ID, "sa_done_") {
				useID = c.ID
			}
		}
		if useID == "" {
			continue
		}
		if i+1 >= len(msgs) || msgs[i+1].LlmData == nil {
			t.Fatalf("tool_use at idx %d (id=%s) has no following message; messages:\n%s", i, useID, dumpMessages(t, msgs))
		}
		var next llm.Message
		if err := json.Unmarshal([]byte(*msgs[i+1].LlmData), &next); err != nil {
			t.Fatalf("unmarshal next: %v", err)
		}
		var matched bool
		var text string
		for _, c := range next.Content {
			if c.Type == llm.ContentTypeToolResult && c.ToolUseID == useID {
				matched = true
				text = toolResultText(c)
			}
		}
		if !matched {
			t.Fatalf("tool_use at idx %d (id=%s) NOT followed by matching tool_result; messages:\n%s", i, useID, dumpMessages(t, msgs))
		}
		pairs = append(pairs, found{useIdx: i, useID: useID, text: text})
	}

	// We expect at least 2 distinct subagent pairs (the parent's loop may
	// also produce additional turns, but those aren't sa_done_ pairs).
	if len(pairs) < 2 {
		t.Fatalf("expected >=2 synthetic subagent pairs, got %d. messages:\n%s", len(pairs), dumpMessages(t, msgs))
	}

	seenAlpha, seenBeta := false, false
	seenIDs := map[string]bool{}
	for _, p := range pairs {
		if seenIDs[p.useID] {
			t.Errorf("duplicate tool_use ID across pairs: %q", p.useID)
		}
		seenIDs[p.useID] = true
		if strings.Contains(p.text, "alpha done") {
			seenAlpha = true
		}
		if strings.Contains(p.text, "beta done") {
			seenBeta = true
		}
	}
	if !seenAlpha || !seenBeta {
		t.Errorf("expected both 'alpha done' and 'beta done' across synthetic pairs (alpha=%v, beta=%v); pairs=%+v", seenAlpha, seenBeta, pairs)
	}
}

// testSubagentDone_LastMessageIsToolUse exercises the case where the
// subagent's most recent persisted message has no text content (it's a
// tool_use only). lastAssistantText returns "" and notifyParentSubagentDone
// must fall back to "(no textual response)" without panicking, while still
// emitting a coherent synthetic pair.
func testSubagentDone_LastMessageIsToolUse(t *testing.T) {
	f := newSubagentDoneFixture(t, "earlier text that should be IGNORED")

	toolUseOnly := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        "toolu_subagent_did_a_thing",
			ToolName:  "bash",
			ToolInput: []byte(`{"command":"echo hi"}`),
		}},
	}
	if err := f.server.recordMessage(context.Background(), f.subagentID, toolUseOnly, llm.Usage{}); err != nil {
		t.Fatalf("record tool_use-only message: %v", err)
	}

	f.fireOnDone()

	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := f.findSyntheticPair()
		return ok
	})

	_, result, ok := f.findSyntheticPair()
	if !ok {
		t.Fatalf("expected synthetic pair even when last subagent msg has no text")
	}
	var gotText string
	for _, c := range result.Content {
		if c.Type == llm.ContentTypeToolResult {
			gotText = toolResultText(c)
		}
	}
	if !strings.Contains(gotText, "(no textual response)") {
		t.Errorf("expected fallback text '(no textual response)' in tool_result, got %q", truncForLog(gotText))
	}
	if strings.Contains(gotText, "earlier text that should be IGNORED") {
		t.Errorf("tool_result leaked text from an older assistant message: %q", truncForLog(gotText))
	}
}

// testSubagentDone_GitInfoIgnored verifies that a gitinfo message recorded
// after the subagent's last real LLM response does not get picked up as
// "the subagent's response" and forwarded to the parent. Gitinfo messages
// have role=Assistant in their llm_data (they describe a worktree state
// change) but Type=gitinfo and are user-visible only — never sent to the
// LLM. Before the lastAgentText fix, notifyParentSubagentDone would
// happily splice the gitinfo text (e.g. '… now at abc1234 "some commit"')
// into the parent's tool_result.
func testSubagentDone_GitInfoIgnored(t *testing.T) {
	f := newSubagentDoneFixture(t, "The subagent's actual final answer.")

	gitMsg := llm.Message{
		Role:    llm.MessageRoleAssistant,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: `~/exe-subagent-done (subagent-done-notify) now at 7b2a11b65 "shelley: notify parent agent when subagent finishes"`}},
	}
	if _, err := f.database.CreateMessage(context.Background(), db.CreateMessageParams{
		ConversationID: f.subagentID,
		Type:           db.MessageTypeGitInfo,
		LLMData:        gitMsg,
		UsageData:      llm.Usage{},
	}); err != nil {
		t.Fatalf("create gitinfo msg: %v", err)
	}

	f.fireOnDone()

	waitFor(t, 5*time.Second, func() bool {
		_, _, ok := f.findSyntheticPair()
		return ok
	})

	_, result, ok := f.findSyntheticPair()
	if !ok {
		t.Fatalf("expected synthetic pair")
	}
	var text string
	for _, c := range result.Content {
		if c.Type == llm.ContentTypeToolResult {
			text = toolResultText(c)
		}
	}
	if !strings.Contains(text, f.subResponse) {
		t.Errorf("expected subagent's real response %q in tool_result, got %q", f.subResponse, truncForLog(text))
	}
	if strings.Contains(text, "now at 7b2a11b65") {
		t.Errorf("gitinfo message text leaked into tool_result: %q", truncForLog(text))
	}
}
