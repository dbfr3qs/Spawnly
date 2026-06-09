import { test, expect } from '@playwright/test';
import { spawn, waitForStatus, chat } from '../helpers/dashboard';
import {
  expandEvents,
  newestEventTime,
  waitForEventType,
  waitForToolEnd,
} from '../helpers/events';
import { killTrees } from '../helpers/cleanup';

// Scenario — pi-worker: the first non-Flue agent (Pi coding harness). Proves the
// platform contract is framework-agnostic by exercising the same UI/chat/event
// path the Flue agents use, plus the two things that make this agent distinctive:
//   1. a coding task produces the NEUTRAL tool/model event timeline, and
//   2. an identity-backed tool calls the protected Sample API with the agent's
//      SPIFFE→OAuth token (proven via events, not the model's wording).
//
// One spawn, two sequential prompts — pi-worker is the heaviest agent to start,
// so we only pay the cold-start once. Assertions read the event timeline (the
// API the dashboard polls) so they don't depend on how the LLM phrases replies.
//
// Precondition: pi-worker pins AI_PROVIDER=openai, so the cluster's ai-provider
// Secret must carry a valid OpenAI key (unlike weather-chat, which defaults to
// Anthropic). See e2e/README.md.
test.describe('pi-worker', () => {
  // Pi's image is large and a coding task runs multiple tool/model turns; give
  // this spec a generous ceiling above the suite default.
  test.setTimeout(360_000);

  const spawned: string[] = [];

  test.afterEach(async ({ page }) => {
    await killTrees(page, spawned.splice(0));
  });

  test('runs a coding task and calls the protected API with its identity', async ({ page }) => {
    await page.goto('/');

    const id = await spawn(page, 'pi-worker');
    spawned.push(id);

    // Long-lived agent: wait until active, then until it has emitted a heartbeat
    // — the heartbeat fires once the agent process is running, narrowing the
    // cold-start window before the first chat (chat() additionally retries the
    // transient "agent unreachable" while the HTTP listener finishes warming).
    await waitForStatus(page, id, 'active');
    await expandEvents(page, id);
    await waitForEventType(page, id, 'heartbeat', { timeout: 120_000 });

    // --- Prompt A: a coding task → neutral tool/model timeline ----------------
    const sinceA = await newestEventTime(page, id);
    const replyA = await chat(
      page,
      id,
      'Create a file note.txt containing the text PI_E2E, then run "cat note.txt" and tell me what it printed.',
    );
    expect(replyA.length).toBeGreaterThan(0);

    // The Pi run is forwarded as the platform's neutral vocabulary.
    await waitForEventType(page, id, 'run_start', { since: sinceA });
    await waitForEventType(page, id, 'model_turn', { since: sinceA });
    await waitForEventType(page, id, 'tool_start', { since: sinceA });
    await waitForEventType(page, id, 'tool_end', { since: sinceA });

    // --- Prompt B: identity-backed protected-API tool -------------------------
    const sinceB = await newestEventTime(page, id);
    const replyB = await chat(
      page,
      id,
      'Use your check_protected_api tool now and report the result.',
    );
    expect(replyB.length).toBeGreaterThan(0);

    // The named tool ran successfully, and the sidecar minted a token for it —
    // together these prove the SPIFFE→OAuth→protected-API path end to end.
    await waitForToolEnd(page, id, 'check_protected_api', { since: sinceB });
    await waitForEventType(page, id, 'token_issued', { since: sinceB });
  });
});
