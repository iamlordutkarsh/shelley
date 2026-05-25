// globalStream.ts — single long-lived EventSource for the whole UI.
//
// Subscribes to /api/stream2. The server delivers per-conversation events for
// ALL active conversations on a single connection, plus server-wide events
// (conversation_list_patch, notification_event, heartbeat). Per-conversation
// events are tagged with `conversation_id` for routing.
//
// This module fans out:
//   * persistent updates (messages, conversation, context_window_size,
//     conversation_state) → messageStore
//   * transient updates (tool_progress, stream_delta, agent_working) →
//     messageStore transient state
//   * list patches → onListPatch handler
//   * notification events → onNotificationEvent handler
//
// Components NEVER open their own EventSource; they subscribe to messageStore.

import type {
  ConversationListPatchEvent,
  NotificationEvent,
  StreamResponse,
  Message,
} from "../types";
import { api } from "./api";
import { messageStore } from "./messageStore";

export type StreamStatus = "connected" | "reconnecting" | "disconnected";

export interface GlobalStreamOptions {
  getHash: () => string | null;
  onListPatch: (event: ConversationListPatchEvent) => void;
  onNotificationEvent?: (event: NotificationEvent) => void;
  onStatusChange?: (status: StreamStatus) => void;
  /**
   * Called once after every successful re-establishment of the EventSource
   * (i.e. after at least one disconnect-then-connect transition; not on
   * the very first connect). Used by App to refresh the focused
   * conversation's history via REST, since any conversation may have
   * received new messages while we were disconnected.
   */
  onReconnect?: () => void;
}

export interface GlobalStreamHandle {
  close: () => void;
  forceReconnect: () => void;
}

function extractToolUseIds(msg: Message): string[] {
  if (msg.type !== "tool" && msg.type !== "user") return [];
  try {
    const raw = msg.llm_data;
    const llmData = raw
      ? typeof raw === "string"
        ? (JSON.parse(raw) as { Content?: Array<{ Type: number; ToolUseID?: string }> })
        : (raw as { Content?: Array<{ Type: number; ToolUseID?: string }> })
      : null;
    if (!llmData?.Content) return [];
    return llmData.Content.filter((c) => c.Type === 6 && c.ToolUseID)
      .map((c) => c.ToolUseID!)
      .filter(Boolean);
  } catch {
    return [];
  }
}

export function connectGlobalStream({
  getHash,
  onListPatch,
  onNotificationEvent,
  onStatusChange,
  onReconnect,
}: GlobalStreamOptions): GlobalStreamHandle {
  let closed = false;
  let eventSource: EventSource | null = null;
  let reconnectTimer: number | null = null;
  let heartbeatTimer: number | null = null;
  let attempts = 0;
  let lastStatus: StreamStatus | null = null;
  // True once we have successfully connected at least once. Used to
  // distinguish a true reconnect from the initial connect: only the former
  // triggers markAllStale() + onReconnect().
  let hasEverConnected = false;
  // True while the EventSource is in the middle of being re-established
  // after a disconnect. Set on error, cleared on the next successful open.
  let isReconnecting = false;

  const setStatus = (s: StreamStatus) => {
    if (s === lastStatus) return;
    lastStatus = s;
    onStatusChange?.(s);
  };

  const clearReconnect = () => {
    if (reconnectTimer !== null) {
      clearTimeout(reconnectTimer);
      reconnectTimer = null;
    }
  };

  const clearHeartbeat = () => {
    if (heartbeatTimer !== null) {
      clearTimeout(heartbeatTimer);
      heartbeatTimer = null;
    }
  };

  const resetHeartbeat = () => {
    clearHeartbeat();
    heartbeatTimer = window.setTimeout(() => {
      console.warn("globalStream: no heartbeat in 60s, forcing reconnect");
      eventSource?.close();
      eventSource = null;
      connect();
    }, 60000);
  };

  const handleEvent = (data: StreamResponse) => {
    if (data.conversation_list_patch) {
      onListPatch(data.conversation_list_patch);
    }
    if (data.notification_event && onNotificationEvent) {
      onNotificationEvent(data.notification_event);
    }

    const convId = data.conversation_id;
    if (!convId) return;

    // Persistent state
    if (data.messages && data.messages.length > 0) {
      // Clear streaming text / tool progress for tools that just produced results.
      const toolIds: string[] = [];
      let sawAgentMsg = false;
      let maxSeq = 0;
      for (const m of data.messages) {
        toolIds.push(...extractToolUseIds(m));
        if (m.type === "agent") sawAgentMsg = true;
        if (m.sequence_id > maxSeq) maxSeq = m.sequence_id;
      }
      if (toolIds.length > 0) messageStore.clearToolProgress(convId, toolIds);
      if (sawAgentMsg) messageStore.resetStreamingText(convId);
      messageStore.upsertMessages(convId, data.messages);
      if (maxSeq > 0) messageStore.setMaxSequenceIdKnown(convId, maxSeq);
    }
    if (data.conversation) {
      // NB: we deliberately do NOT mirror data.conversation.agent_working
      // into the transient store here. Per-conversation Conversation rows
      // arrive embedded in unrelated stream events (new-message broadcast,
      // git-state change, cwd change, etc.) and can carry a stale snapshot
      // taken before an in-flight SetConversationAgentWorking commit.
      // Authoritative agent_working sync happens via (a) conversation_state
      // events fired synchronously from SetAgentWorking, and (b) the
      // conversation_list_patch stream, whose updates are driven by the DB
      // commit hook and therefore strictly trail the matching write —
      // handled in App.handleConversationListPatch.
      messageStore.setConversation(convId, data.conversation);
    }
    if (typeof data.context_window_size === "number") {
      messageStore.setContextWindowSize(convId, data.context_window_size);
    }
    // NB: we deliberately do NOT mirror data.conversation_state.working
    // into messageStore here. The conversation_list_patch stream is the
    // single authoritative source of truth for agent_working:
    // server-side recomputeMu serializes patch emission so list patches
    // arrive in a strict old_hash→new_hash chain, while per-conversation
    // conversation_state events ride a separate streamPub fan-out and
    // can race with the list patches at the client. If we let both
    // update agentWorking, a stale state event from an earlier transition
    // can stomp a fresher list-patch value — the "thinking pill
    // stays on / flickers" symptom (iOS hit the mirror image of this
    // race and fixed it in a4ce86d1f the same way). List patches now
    // pump the authoritative value via App.handleConversationListPatch.
    //
    // We still leave conversation_state in the protocol because the
    // server's per-conversation /api/conversation/<id>/stream and legacy
    // iOS clients consume it; this client just no longer trusts it for
    // working state.

    // Transient state
    if (data.tool_progress) {
      messageStore.setToolProgress(convId, data.tool_progress);
    }
    if (data.stream_delta && data.stream_delta.type === "text") {
      messageStore.appendStreamDelta(convId, data.stream_delta.text);
    }
  };

  const connect = () => {
    if (closed) return;
    clearReconnect();
    eventSource?.close();
    eventSource = api.createStream({ conversationListHash: getHash() ?? undefined });

    const markConnected = () => {
      attempts = 0;
      setStatus("connected");
      if (isReconnecting) {
        // We just re-established after a disconnect. Any conversation could
        // have received new messages while we were down; flag every cached
        // record as needing a fresh REST backfill the next time it's focused,
        // and let the host refresh the currently-focused conversation now.
        isReconnecting = false;
        messageStore.markAllStale();
        onReconnect?.();
      }
      hasEverConnected = true;
    };

    eventSource.onopen = () => {
      markConnected();
      resetHeartbeat();
    };

    eventSource.onmessage = (ev) => {
      markConnected();
      resetHeartbeat();
      try {
        const data = JSON.parse(ev.data) as StreamResponse;
        handleEvent(data);
      } catch (err) {
        console.error("globalStream: failed to parse event:", err);
      }
    };

    eventSource.onerror = () => {
      if (closed) return;
      eventSource?.close();
      eventSource = null;
      clearHeartbeat();
      attempts += 1;
      // Only mark stale on the first reconnect attempt after a confirmed
      // disconnect, not on every retry.
      if (hasEverConnected) isReconnecting = true;
      setStatus(attempts > 3 ? "disconnected" : "reconnecting");
      const delay = attempts <= 1 ? 1000 : attempts === 2 ? 2000 : attempts === 3 ? 5000 : 30000;
      reconnectTimer = window.setTimeout(connect, delay);
    };
  };

  // On iOS Safari and other mobile browsers, EventSource may stay nominally
  // open while the underlying TCP connection has been killed by the OS
  // during background. Force a reconnect when the tab returns to the
  // foreground or the network comes back, so we resume quickly instead of
  // waiting for the next heartbeat to time out.
  const hiddenAtRef = { t: 0 };
  const onVisibility = () => {
    if (document.visibilityState === "hidden") {
      hiddenAtRef.t = Date.now();
      return;
    }
    const hiddenFor = hiddenAtRef.t ? Date.now() - hiddenAtRef.t : 0;
    hiddenAtRef.t = 0;
    if (closed) return;
    if (!eventSource || eventSource.readyState === 2 || hiddenFor > 5000) {
      if (hasEverConnected) isReconnecting = true;
      eventSource?.close();
      eventSource = null;
      clearHeartbeat();
      clearReconnect();
      connect();
    }
  };
  const onOnline = () => {
    if (closed) return;
    if (!eventSource || eventSource.readyState === 2) {
      if (hasEverConnected) isReconnecting = true;
      eventSource?.close();
      eventSource = null;
      clearHeartbeat();
      clearReconnect();
      connect();
    }
  };
  document.addEventListener("visibilitychange", onVisibility);
  window.addEventListener("online", onOnline);

  connect();

  return {
    close() {
      closed = true;
      clearReconnect();
      clearHeartbeat();
      eventSource?.close();
      eventSource = null;
      document.removeEventListener("visibilitychange", onVisibility);
      window.removeEventListener("online", onOnline);
    },
    forceReconnect() {
      attempts = 0;
      if (hasEverConnected) isReconnecting = true;
      eventSource?.close();
      eventSource = null;
      clearHeartbeat();
      connect();
    },
  };
}
