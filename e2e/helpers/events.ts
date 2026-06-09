import { expect, type Page } from '@playwright/test';

// A lifecycle/work event from GET /api/agents/{id}/events. The neutral `type`
// (e.g. "work_ok", "work_denied") is what tests assert on.
export interface AgentEvent {
  type: string;
  source?: string;
  timestamp?: string;
  payload?: unknown;
}

// Fetch an agent's event timeline via the same endpoint the page polls. Uses
// Playwright's request context (shares baseURL/cookies) — off-the-shelf, no
// extra deps. Returns [] on any non-OK response so callers can poll cleanly.
export async function getEvents(page: Page, agentId: string): Promise<AgentEvent[]> {
  const resp = await page.request.get(`/api/agents/${agentId}/events`);
  if (!resp.ok()) return [];
  return resp.json();
}

// Epoch-ms of the newest event, or 0 if none. Capture this before an action
// (e.g. a revoke) to use as a `since` baseline so subsequent waits only count
// events produced after the action — older work_ok rows never satisfy them.
export async function newestEventTime(page: Page, agentId: string): Promise<number> {
  let newest = 0;
  for (const e of await getEvents(page, agentId)) {
    const t = e.timestamp ? Date.parse(e.timestamp) : NaN;
    if (!isNaN(t) && t > newest) newest = t;
  }
  return newest;
}

export interface WaitEventOpts {
  /** Only count events strictly newer than this epoch-ms. Default 0 (any). */
  since?: number;
  timeout?: number;
}

// Poll the timeline until an event of `type` appears (optionally newer than
// `since`). Throws via expect on timeout.
export async function waitForEventType(
  page: Page,
  agentId: string,
  type: string,
  opts: WaitEventOpts = {},
): Promise<void> {
  const since = opts.since ?? 0;
  await expect
    .poll(
      async () => {
        const evts = await getEvents(page, agentId);
        return evts.some(
          (e) =>
            e.type === type &&
            (!since || (e.timestamp ? Date.parse(e.timestamp) : 0) > since),
        );
      },
      { timeout: opts.timeout ?? 60_000, intervals: [1000, 2000, 3000] },
    )
    .toBe(true);
}

// Click an agent card's "Events" toggle so the timeline panel/poller is
// exercised through the real UI (the assertions still read the API directly).
export async function expandEvents(page: Page, agentId: string): Promise<void> {
  await page.locator(`[id="evtbtn-${agentId}"]`).click();
}
