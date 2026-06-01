package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"shelley.exe.dev/claudetool"
	"shelley.exe.dev/db"
	"shelley.exe.dev/db/generated"
	"shelley.exe.dev/llm"
)

// SubagentRunner implements claudetool.SubagentRunner.
type SubagentRunner struct {
	server *Server
}

// NewSubagentRunner creates a new SubagentRunner.
func NewSubagentRunner(s *Server) *SubagentRunner {
	return &SubagentRunner{server: s}
}

// RunSubagent implements claudetool.SubagentRunner.
func (r *SubagentRunner) RunSubagent(ctx context.Context, conversationID, prompt string, wait bool, timeout time.Duration, modelID string) (string, error) {
	s := r.server

	// Notify the UI about the subagent conversation.
	// This ensures the sidebar shows the subagent even if it's a newly created conversation.
	go r.notifySubagentConversation(ctx, conversationID)

	// Run new-conversation hook for newly created subagent conversations.
	// We detect "new" by checking if the manager already exists.
	s.mu.Lock()
	_, alreadyActive := s.activeConversations[conversationID]
	s.mu.Unlock()
	if !alreadyActive {
		conv, convErr := s.db.GetConversationByID(ctx, conversationID)
		if convErr != nil {
			s.logger.Error("Failed to get conversation for new-conversation hook", "error", convErr, "conversationID", conversationID)
		} else if conv.ParentConversationID != nil {
			hookResult, hookErr := RunNewConversationHookIn(s.hooksDir, NewConversationHookInput{
				Prompt: prompt,
				Model:  modelID,
				Cwd:    derefString(conv.Cwd),
				Readonly: NewConversationReadonly{
					ConversationID: conversationID,
					IsSubagent:     true,
					ParentID:       *conv.ParentConversationID,
				},
			})
			if hookErr != nil {
				return "", fmt.Errorf("new-conversation hook: %w", hookErr)
			}
			if hookResult.Cwd != derefString(conv.Cwd) {
				if err := s.db.UpdateConversationCwd(ctx, conversationID, hookResult.Cwd); err != nil {
					s.logger.Error("Failed to update subagent cwd from hook", "error", err)
				}
			}
			if hookResult.Prompt != prompt {
				prompt = hookResult.Prompt
			}
			if hookResult.Model != modelID {
				if _, svcErr := s.llmManager.GetService(hookResult.Model); svcErr != nil {
					s.logger.Error("Hook returned unsupported model, keeping original", "hookModel", hookResult.Model, "error", svcErr)
				} else {
					modelID = hookResult.Model
				}
			}
		}
	}

	// Get or create conversation manager for the subagent, with incremented depth
	manager, err := s.getOrCreateSubagentConversationManager(ctx, conversationID)
	if err != nil {
		return "", fmt.Errorf("failed to get conversation manager: %w", err)
	}

	// Use the parent's model if provided, otherwise fall back to server
	// default (preferring a ready model from the catalog; see
	// effectiveDefaultModel).
	if modelID == "" {
		modelID = s.effectiveDefaultModel(s.getModelList())
	}

	// Persist model on the subagent conversation record
	// UpdateConversationModel only sets the model if it's NULL, so this is safe for re-sends
	if modelID != "" {
		if err := s.db.UpdateConversationModel(ctx, conversationID, modelID); err != nil {
			s.logger.Warn("Failed to persist model on subagent conversation", "error", err, "conversationID", conversationID)
		}
	}

	// Get LLM service
	llmService, err := s.llmManager.GetService(modelID)
	if err != nil {
		return "", fmt.Errorf("failed to get LLM service: %w", err)
	}

	// If the subagent is currently working, stop it first before sending new message.
	// A stopped timed-out run will not later complete, so discard any pending
	// async-completion marker for it.
	if manager.IsAgentWorking() {
		s.clearSubagentWaitTimedOut(conversationID)
		s.logger.Info("Subagent is working, stopping before sending new message", "conversationID", conversationID)
		if err := manager.CancelConversation(ctx); err != nil {
			s.logger.Error("Failed to cancel subagent conversation", "error", err)
			// Continue anyway - we still want to send the new message
		}
		// Re-hydrate the manager after cancellation
		if err := manager.Hydrate(ctx); err != nil {
			return "", fmt.Errorf("failed to hydrate after cancellation: %w", err)
		}
	}

	// Create user message
	userMessage := llm.Message{
		Role:    llm.MessageRoleUser,
		Content: []llm.Content{{Type: llm.ContentTypeText, Text: prompt}},
	}

	// Accept the user message (this starts processing)
	_, err = manager.AcceptUserMessage(ctx, llmService, modelID, userMessage)
	if err != nil {
		return "", fmt.Errorf("failed to accept user message: %w", err)
	}
	if wait && !manager.IsAgentWorking() {
		// The response completed synchronously inside AcceptUserMessage; any
		// stale timeout marker belongs to an earlier run and must not cause a
		// duplicate async completion for this one. If the new run is still
		// working, preserve the marker so the earlier timed-out run can still
		// report completion.
		s.clearSubagentWaitTimedOut(conversationID)
	}
	if !wait {
		return fmt.Sprintf("Subagent started processing. Conversation ID: %s", conversationID), nil
	}

	// Wait for the agent to finish (or timeout)
	return r.waitForResponse(ctx, conversationID, modelID, llmService, timeout)
}

func (r *SubagentRunner) waitForResponse(ctx context.Context, conversationID, modelID string, llmService llm.Service, timeout time.Duration) (string, error) {
	s := r.server

	deadline := time.Now().Add(timeout)
	pollInterval := 500 * time.Millisecond

	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}

		if time.Now().After(deadline) {
			// Timeout reached - generate a progress summary. Remember that
			// this subagent still needs an async completion notification later,
			// even if it finishes before the parent records the timeout tool_result.
			s.markSubagentWaitTimedOut(conversationID)
			return r.generateProgressSummary(ctx, conversationID, modelID, llmService)
		}

		// Check if agent is still working
		working, err := r.isAgentWorking(ctx, conversationID)
		if err != nil {
			return "", fmt.Errorf("failed to check agent status: %w", err)
		}

		if !working {
			// Agent is done and this wait=true call will return the final answer
			// synchronously, so any old timeout marker for this conversation must
			// not trigger a duplicate async completion later.
			s.clearSubagentWaitTimedOut(conversationID)
			return r.getLastAssistantResponse(ctx, conversationID)
		}

		// Wait before polling again
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}

		// Don't hog the conversation manager mutex
		s.mu.Lock()
		if mgr, ok := s.activeConversations[conversationID]; ok {
			mgr.Touch()
		}
		s.mu.Unlock()
	}
}

func (r *SubagentRunner) isAgentWorking(ctx context.Context, conversationID string) (bool, error) {
	s := r.server

	// Get the conversation manager - it tracks the working state
	s.mu.Lock()
	mgr, ok := s.activeConversations[conversationID]
	s.mu.Unlock()

	if !ok {
		// No active manager means the agent is not working
		return false, nil
	}

	return mgr.IsAgentWorking(), nil
}

func (r *SubagentRunner) getLastAssistantResponse(ctx context.Context, conversationID string) (string, error) {
	return r.server.lastAgentText(ctx, conversationID)
}

// generateProgressSummary makes a non-conversation LLM call to summarize the subagent's progress.
// This is called when the timeout is reached and the subagent is still working.
func (r *SubagentRunner) generateProgressSummary(ctx context.Context, conversationID, modelID string, llmService llm.Service) (string, error) {
	s := r.server

	// Get the conversation messages
	var messages []generated.Message
	err := s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		messages, err = q.ListMessages(ctx, conversationID)
		return err
	})
	if err != nil {
		s.logger.Error("Failed to get messages for progress summary", "error", err)
		return "[Subagent is still working (timeout reached). Failed to generate progress summary.]", nil
	}

	if len(messages) == 0 {
		return "[Subagent is still working (timeout reached). No messages yet.]", nil
	}

	// Build a summary of the conversation for the LLM
	conversationSummary := r.buildConversationSummary(messages)

	// Make a non-conversation LLM call to summarize progress
	summaryPrompt := `You are summarizing the current progress of a subagent task for a parent agent.

The subagent was given a task and has been working on it, but the timeout was reached before it completed.
Below is the conversation history showing what the subagent has done so far.

Please provide a brief, actionable summary (2-4 sentences) that tells the parent agent:
1. What the subagent has accomplished so far
2. What it appears to be currently working on
3. Whether it seems to be making progress or stuck

Conversation history:
` + conversationSummary + `

Provide your summary now:`

	req := &llm.Request{
		Messages: []llm.Message{
			{
				Role:    llm.MessageRoleUser,
				Content: []llm.Content{{Type: llm.ContentTypeText, Text: summaryPrompt}},
			},
		},
	}

	// Use a short timeout for the summary call
	summaryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	resp, err := llmService.Do(summaryCtx, req)
	if err != nil {
		s.logger.Error("Failed to generate progress summary via LLM", "error", err)
		return "[Subagent is still working (timeout reached). Failed to generate progress summary.]", nil
	}

	// Extract the summary text
	var summaryText string
	for _, content := range resp.Content {
		if content.Type == llm.ContentTypeText && content.Text != "" {
			summaryText = content.Text
			break
		}
	}

	if summaryText == "" {
		return "[Subagent is still working (timeout reached). No summary available.]", nil
	}

	return fmt.Sprintf("[Subagent is still working (timeout reached). Progress summary:]\n%s", summaryText), nil
}

// buildConversationSummary creates a text summary of the conversation messages for the LLM.
func (r *SubagentRunner) buildConversationSummary(messages []generated.Message) string {
	var sb strings.Builder

	for _, msg := range messages {
		// Skip system messages
		if msg.Type == "system" {
			continue
		}

		if msg.LlmData == nil {
			continue
		}

		var llmMsg llm.Message
		if err := json.Unmarshal([]byte(*msg.LlmData), &llmMsg); err != nil {
			continue
		}

		roleStr := "User"
		if llmMsg.Role == llm.MessageRoleAssistant {
			roleStr = "Assistant"
		}

		for _, content := range llmMsg.Content {
			switch content.Type {
			case llm.ContentTypeText:
				if content.Text != "" {
					// Truncate very long text
					text := content.Text
					if len(text) > 500 {
						text = text[:500] + "...[truncated]"
					}
					sb.WriteString(fmt.Sprintf("[%s]: %s\n\n", roleStr, text))
				}
			case llm.ContentTypeToolUse:
				// Truncate tool input if long
				inputStr := string(content.ToolInput)
				if len(inputStr) > 200 {
					inputStr = inputStr[:200] + "...[truncated]"
				}
				sb.WriteString(fmt.Sprintf("[%s used tool %s]: %s\n\n", roleStr, content.ToolName, inputStr))
			case llm.ContentTypeToolResult:
				// Summarize tool results
				resultText := ""
				for _, r := range content.ToolResult {
					if r.Type == llm.ContentTypeText && r.Text != "" {
						resultText = r.Text
						break
					}
				}
				if len(resultText) > 300 {
					resultText = resultText[:300] + "...[truncated]"
				}
				errorStr := ""
				if content.ToolError {
					errorStr = " (error)"
				}
				sb.WriteString(fmt.Sprintf("[Tool result%s]: %s\n\n", errorStr, resultText))
			}
		}
	}

	// Limit total size
	result := sb.String()
	if len(result) > 8000 {
		// Keep the last 8000 chars (most recent activity)
		result = "...[earlier messages truncated]...\n" + result[len(result)-8000:]
	}

	return result
}

// notifySubagentConversation fetches the subagent conversation and publishes it
// to all SSE streams so the UI can update the sidebar.
func (r *SubagentRunner) notifySubagentConversation(ctx context.Context, conversationID string) {
	s := r.server

	// Fetch the conversation from the database
	var conv generated.Conversation
	err := s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		conv, err = q.GetConversation(ctx, conversationID)
		return err
	})
	if err != nil {
		s.logger.Error("Failed to get subagent conversation for notification", "error", err, "conversationID", conversationID)
		return
	}

	// Only notify if this is actually a subagent (has parent)
	if conv.ParentConversationID == nil {
		return
	}

	// Publish the subagent conversation to all active streams
	s.publishConversationListUpdate(ConversationListUpdate{
		Type:         "update",
		Conversation: &conv,
	})

	s.logger.Debug("Notified UI about subagent conversation",
		"conversationID", conversationID,
		"parentID", *conv.ParentConversationID,
		"slug", conv.Slug)
}

// notifyParentSubagentDone enqueues a synthetic tool_use/tool_result pair
// onto the parent conversation's pending-batch queue when a subagent
// finishes, so the parent agent knows to check the results. Inspired by
// boldsoftware/shelley#200.
//
// All scheduling — wait for the current turn to end, wait for distillation
// setup to complete, cooperate with user-typed messages — is handled by the
// same drainPendingMessages path that user messages already use. We just
// drop a batch onto the queue and trust that machinery.
//
// The case we filter out here is wait=true: if the parent has a pending or
// already completed synchronous subagent tool call targeting this exact
// subagent, that tool call conveys the response and our synthetic pair would
// duplicate it.
func (s *Server) notifyParentSubagentDone(subagentConversationID string) {
	ctx := context.Background()

	var conv generated.Conversation
	err := s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		conv, err = q.GetConversation(ctx, subagentConversationID)
		return err
	})
	if err != nil || conv.ParentConversationID == nil {
		return
	}

	parentID := *conv.ParentConversationID
	slug := "unknown"
	if conv.Slug != nil {
		slug = *conv.Slug
	}

	s.mu.Lock()
	parentManager, ok := s.activeConversations[parentID]
	s.mu.Unlock()
	if !ok {
		return
	}

	// If a wait=true call timed out, the parent has only seen a progress
	// summary, so the eventual completion still needs to be delivered. This
	// in-memory bit closes the race where the subagent finishes before the
	// parent's timeout tool_result has been persisted.
	if s.subagentWaitTimedOut(subagentConversationID) {
		s.clearSubagentWaitTimedOut(subagentConversationID)
	} else if s.parentHasSynchronousSubagentResult(ctx, parentID, subagentConversationID) {
		return
	}

	parentManager.mu.Lock()
	parentModelID := parentManager.modelID
	parentManager.mu.Unlock()

	response, err := s.lastAgentText(ctx, subagentConversationID)
	if err != nil || response == "" {
		response = "(no textual response)"
	}
	// Cap the subagent text we splice into the parent's history. A runaway
	// subagent reply shouldn't dominate the parent's context window; the
	// parent can always read the full subagent conversation via the
	// dedicated subagent view.
	if len(response) > 500 {
		response = response[:500] + "..."
	}

	// Splice in a synthetic tool_use/tool_result pair as if the parent had
	// just called the subagent tool with wait=true. This gives the LLM the
	// information it needs in the tool_result channel (weaker prompt
	// authority than user-voice), avoids the extra round trip that a
	// "please call the subagent tool" nudge would require, and the result
	// is clearly attributed to the subagent.
	toolUseID := fmt.Sprintf("sa_done_%s", uuid.New().String())
	toolInput, _ := json.Marshal(map[string]any{
		"slug":   slug,
		"prompt": "(asynchronous completion notification)",
		"wait":   true,
	})
	assistantMsg := llm.Message{
		Role: llm.MessageRoleAssistant,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolUse,
			ID:        toolUseID,
			ToolName:  "subagent",
			ToolInput: toolInput,
			Display: claudetool.SubagentDisplayData{
				Slug:           slug,
				ConversationID: subagentConversationID,
			},
		}},
	}
	toolResultMsg := llm.Message{
		Role: llm.MessageRoleUser,
		Content: []llm.Content{{
			Type:      llm.ContentTypeToolResult,
			ToolUseID: toolUseID,
			ToolResult: []llm.Content{{
				Type: llm.ContentTypeText,
				Text: fmt.Sprintf(
					"[Subagent %q has finished asynchronously. "+
						"This tool call was synthesized by the system to surface the "+
						"result; you did not invoke it yourself. Please briefly acknowledge "+
						"the subagent's outcome to the user and decide whether any follow-up "+
						"work is needed.]\n\nSubagent response:\n%s",
					slug, response,
				),
			}},
			Display: claudetool.SubagentDisplayData{
				Slug:           slug,
				ConversationID: subagentConversationID,
			},
		}},
	}

	modelID := parentModelID
	if modelID == "" {
		modelID = s.defaultModel
	}

	// Enqueue onto the parent's pending-batch queue. drainPendingMessages
	// handles persistence, loop start/wake, and serialization with both
	// distillation and other queued work. We don't need to read or touch
	// the parent's agentWorking/distilling/loop state ourselves — the queue
	// is the single point of coordination.
	parentManager.EnqueueSubagentDone(s, modelID, assistantMsg, toolResultMsg)
	s.logger.Info("Queued subagent-done notification for parent", "subagent", slug, "parent", parentID)
}

func (s *Server) markSubagentWaitTimedOut(subagentConversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.subagentWaitTimeouts[subagentConversationID] = true
}

func (s *Server) clearSubagentWaitTimedOut(subagentConversationID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.subagentWaitTimeouts, subagentConversationID)
}

func (s *Server) subagentWaitTimedOut(subagentConversationID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subagentWaitTimeouts[subagentConversationID]
}

// parentHasSynchronousSubagentResult reports whether the parent has a
// wait=true subagent tool call targeting the just-finished subagent that
// either has not yet received its tool_result or has already received the
// subagent's final response. In both cases notifyParentSubagentDone must not
// synthesize another tool_result: the in-flight or recorded tool call is
// already the delivery path for the result.
//
// wait=true calls that timed out are different: their tool_result is only a
// progress summary, and the eventual completion should still be delivered
// asynchronously.
//
// We match on slug alone (not conversation ID) because the subagent tool
// enforces slug uniqueness within a parent conversation (see
// claudetool/subagent.go: "failed to create unique subagent slug"), so a
// slug uniquely identifies a subagent within its parent.
func (s *Server) parentHasSynchronousSubagentResult(ctx context.Context, parentID, subagentConversationID string) bool {
	var conv generated.Conversation
	if err := s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		conv, err = q.GetConversation(ctx, subagentConversationID)
		return err
	}); err != nil || conv.Slug == nil {
		return false
	}
	targetSlug := *conv.Slug

	var msgs []generated.Message
	if err := s.db.Queries(ctx, func(q *generated.Queries) error {
		var err error
		msgs, err = q.ListMessagesForContext(ctx, parentID)
		return err
	}); err != nil {
		return false
	}

	// Walk backwards to find the most recent subagent tool_use for this slug.
	// Its wait value tells us whether completion should be synchronous
	// (wait omitted/true) or asynchronous (wait=false).
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.LlmData == nil {
			continue
		}
		var lm llm.Message
		if err := json.Unmarshal([]byte(*m.LlmData), &lm); err != nil {
			continue
		}
		for _, c := range lm.Content {
			if c.Type != llm.ContentTypeToolUse || c.ToolName != "subagent" {
				continue
			}
			var input struct {
				Slug string `json:"slug"`
				Wait *bool  `json:"wait"`
			}
			if err := json.Unmarshal(c.ToolInput, &input); err != nil || input.Slug != targetSlug {
				continue
			}
			if input.Wait != nil && !*input.Wait {
				return false
			}
			return !subagentToolUseTimedOut(msgs[i+1:], c.ID, targetSlug)
		}
	}
	return false
}

func subagentToolUseTimedOut(msgs []generated.Message, toolUseID, slug string) bool {
	for _, m := range msgs {
		if m.LlmData == nil {
			continue
		}
		var lm llm.Message
		if err := json.Unmarshal([]byte(*m.LlmData), &lm); err != nil {
			continue
		}
		for _, c := range lm.Content {
			if c.Type != llm.ContentTypeToolResult || c.ToolUseID != toolUseID {
				continue
			}
			return isSubagentTimeoutResult(toolResultPlainText(c), slug)
		}
	}
	// Still pending: the synchronous tool call will deliver the response.
	return false
}

func isSubagentTimeoutResult(text, slug string) bool {
	prefix := fmt.Sprintf("Subagent '%s' response:", slug)
	if !strings.HasPrefix(text, prefix) {
		return false
	}
	_, result, ok := strings.Cut(text[len(prefix):], "\n")
	if !ok {
		return false
	}
	return strings.HasPrefix(result, "[Subagent is still working (timeout reached).") ||
		strings.Contains(result, ")\n[Subagent is still working (timeout reached).")
}

func toolResultPlainText(c llm.Content) string {
	var sb strings.Builder
	for _, r := range c.ToolResult {
		if r.Type == llm.ContentTypeText {
			sb.WriteString(r.Text)
		}
	}
	return sb.String()
}

// lastAgentText returns the concatenated text content of the most recent
// type=agent message in a conversation — specifically the latest such
// message, skipping non-agent rows (gitinfo, user, tool, system, error)
// that may have been appended after it.
//
// In particular gitinfo messages carry assistant-role llm_data and would
// otherwise be returned as "the subagent's response" when they're really
// user-visible git state notes Shelley itself injected.
//
// If the latest agent message has no text content (e.g. it's a pure
// tool_use), returns "" — we don't walk further back, because earlier
// agent turns are stale: their text was already conveyed via prior
// notifications or tool returns.
func (s *Server) lastAgentText(ctx context.Context, conversationID string) (string, error) {
	msgs, err := s.db.ListMessages(ctx, conversationID)
	if err != nil {
		return "", err
	}
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Type != string(db.MessageTypeAgent) {
			continue
		}
		if m.LlmData == nil {
			return "", nil
		}
		var llmMsg llm.Message
		if err := json.Unmarshal([]byte(*m.LlmData), &llmMsg); err != nil {
			return "", err
		}
		var texts []string
		for _, content := range llmMsg.Content {
			if content.Type == llm.ContentTypeText && content.Text != "" {
				texts = append(texts, content.Text)
			}
		}
		return strings.Join(texts, "\n"), nil
	}
	return "", nil
}

// Ensure SubagentRunner implements claudetool.SubagentRunner.
var _ claudetool.SubagentRunner = (*SubagentRunner)(nil)

// handleGetSubagents returns the list of subagents for a conversation.
func (s *Server) handleGetSubagents(w http.ResponseWriter, r *http.Request, conversationID string) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	subagents, err := s.db.GetSubagents(r.Context(), conversationID)
	if err != nil {
		s.logger.Error("Failed to get subagents", "conversationID", conversationID, "error", err)
		http.Error(w, "Failed to get subagents", 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(subagents)
}
