// Pi → Spawnly event glue.
//
// This is the ONLY Pi-specific adapter in the agent, and it lives HERE in the
// agent — NOT in @spawnly/sdk. It taps Pi's in-process AgentSession event stream
// and forwards a NEUTRAL, framework-agnostic event vocabulary into the platform
// using the SDK's generic `postEvent`. The platform and the SDK are untouched:
// the same neutral vocabulary the dashboard already renders for Flue agents
// (run_start/run_end/model_turn/tool_start/tool_end/compaction/error/log).
//
// Structural typing only — we never import a Pi type, so the dependency arrow
// points one way (agent → SDK), exactly like the Flue agents.

import { postEvent } from '@spawnly/sdk';

// Any object exposing Pi's subscribe(listener) => unsubscribe satisfies this.
type PiSubscribable = { subscribe(listener: (event: any) => void): () => void };

const TRUNCATE_LIMIT = 2_000;

// Best-effort, never-throwing serialization capped at ~2000 chars.
function truncate(v: unknown): string {
  let s: string;
  try {
    s = JSON.stringify(v);
    if (s === undefined) s = String(v);
  } catch {
    try {
      s = String(v);
    } catch {
      s = '[unserializable]';
    }
  }
  return s.length > TRUNCATE_LIMIT ? s.slice(0, TRUNCATE_LIMIT) + '…[truncated]' : s;
}

/**
 * Subscribe to a Pi AgentSession's event stream and forward the platform's
 * neutral event vocabulary via postEvent. Never throws from the callback.
 * Returns Pi's unsubscribe function.
 */
export function instrumentPiSession(
  session: PiSubscribable,
  registryUrl: string,
  agentId: string,
): () => void {
  return session.subscribe((event: any) => {
    try {
      const type: string | undefined = event?.type;
      if (!type) return;

      switch (type) {
        case 'agent_start':
          void postEvent(registryUrl, agentId, 'run_start', { agentName: 'pi-worker' });
          return;
        case 'agent_end':
          void postEvent(registryUrl, agentId, 'run_end', {
            isError: false,
            willRetry: event.willRetry,
            messageCount: Array.isArray(event.messages) ? event.messages.length : undefined,
          });
          return;
        case 'turn_end':
          void postEvent(registryUrl, agentId, 'model_turn', {
            toolResults: Array.isArray(event.toolResults) ? event.toolResults.length : undefined,
          });
          return;
        case 'tool_execution_start':
          void postEvent(registryUrl, agentId, 'tool_start', {
            toolName: event.toolName,
            toolCallId: event.toolCallId,
            args: truncate(event.args),
          });
          return;
        case 'tool_execution_end':
          void postEvent(registryUrl, agentId, 'tool_end', {
            toolName: event.toolName,
            toolCallId: event.toolCallId,
            isError: event.isError,
            result: truncate(event.result),
          });
          return;
        case 'compaction_start':
          void postEvent(registryUrl, agentId, 'compaction', { phase: 'start', reason: event.reason });
          return;
        case 'compaction_end':
          void postEvent(registryUrl, agentId, 'compaction', {
            phase: 'end',
            reason: event.reason,
            aborted: event.aborted,
            error: truncate(event.errorMessage),
          });
          return;
        case 'auto_retry_start':
          void postEvent(registryUrl, agentId, 'log', {
            level: 'warn',
            message: `auto-retry ${event.attempt}/${event.maxAttempts} in ${event.delayMs}ms: ${event.errorMessage}`,
          });
          return;
        case 'auto_retry_end':
          void postEvent(registryUrl, agentId, 'log', {
            level: event.success ? 'info' : 'error',
            message: `auto-retry ${event.attempt} ${event.success ? 'succeeded' : 'failed'}${event.finalError ? `: ${event.finalError}` : ''}`,
          });
          return;
        case 'error':
          void postEvent(registryUrl, agentId, 'error', { detail: truncate(event) });
          return;

        // High-frequency / low-signal: drop to keep the timeline readable.
        case 'turn_start':
        case 'message_start':
        case 'message_update':
        case 'message_end':
        case 'tool_execution_update':
        case 'queue_update':
        case 'session_info_changed':
        case 'thinking_level_changed':
          return;

        default:
          // Forward any other event under its own (already neutral) name.
          void postEvent(registryUrl, agentId, type, truncate(event));
          return;
      }
    } catch (e) {
      // Must never break the agent.
      console.warn(`[pi-worker] instrument handler failed: ${e}`);
    }
  });
}
