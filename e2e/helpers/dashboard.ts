import { expect, type Page } from '@playwright/test';

// Shape of an agent as returned by GET /api/agents (proxied to the orchestrator).
export interface AgentSummary {
  agentId: string;
  agentType?: string;
  status?: string;
  tenantId?: string;
  userId?: string;
  parentId?: string;
  lifecycle?: string;
  supportsChat?: boolean;
}

export interface SpawnOpts {
  /** Free-text task; only some agent types use it. */
  task?: string;
}

// Open the spawn modal, choose an agent type, optionally set a task, and spawn.
// Actions go through the real UI (click → select → click); the agentId is read
// from the /api/spawn response the page issues, which is exact and race-free.
// Returns the spawned agent's id (workloadName).
export async function spawn(page: Page, agentType: string, opts: SpawnOpts = {}): Promise<string> {
  await page.locator('#open-modal-btn').click();

  // Type options are populated asynchronously from /api/templates on load.
  const select = page.locator('#f-type');
  await expect(select.locator(`option[value="${agentType}"]`)).toHaveCount(1, { timeout: 15_000 });
  await select.selectOption(agentType);

  if (opts.task !== undefined) {
    await page.locator('#f-task').fill(opts.task);
  }

  const [resp] = await Promise.all([
    page.waitForResponse(
      (r) => r.url().includes('/api/spawn') && r.request().method() === 'POST',
    ),
    page.locator('#spawn-btn').click(),
  ]);

  expect(resp.ok(), `spawn ${agentType} failed: ${resp.status()} ${await resp.text()}`).toBeTruthy();
  const body = await resp.json();
  expect(body.workloadName, 'spawn response missing workloadName').toBeTruthy();
  return body.workloadName as string;
}

// Wait until an agent's own status badge reaches `status` (e.g. "completed",
// "active", "revoked"). The `> .agent-row` combinator scopes to this card's own
// row so nested child cards' badges are not matched.
export async function waitForStatus(
  page: Page,
  agentId: string,
  status: string,
  timeout = 180_000,
): Promise<void> {
  const badge = page.locator(`[id="card-${agentId}"] > .agent-row [data-testid="status"]`);
  await expect(badge).toHaveText(status, { timeout });
}

// Current agent list (top-level + children), straight from the API the page polls.
export async function listAgents(page: Page): Promise<AgentSummary[]> {
  const resp = await page.request.get('/api/agents');
  if (!resp.ok()) return [];
  return resp.json();
}

// Send one chat message to a long-lived agent and return its reply text. Opens
// the chat panel only if it isn't already open (so repeated calls don't toggle
// it shut), and waits for a NEW agent bubble to appear — counting the existing
// non-typing replies first, then waiting for the count to grow — so a second
// prompt never matches the first prompt's stale reply.
//
// Just after an agent reaches `active` (registered) there is a brief window
// where its HTTP listener / Service endpoint isn't routable yet, so the
// orchestrator returns and the dashboard renders a persistent
// "[Error: agent unreachable]" bubble that never self-heals. This is more
// likely for heavier agents (e.g. pi-worker's large module graph slows its cold
// start). To stay robust we treat that specific error as a readiness signal and
// re-send after a short backoff, up to `timeout`. Any other error bubble fails
// immediately (it's a real error, not warm-up).
export async function chat(
  page: Page,
  agentId: string,
  message: string,
  timeout = 150_000,
): Promise<string> {
  const input = page.locator(`[id="chatinput-${agentId}"]`);
  if (!(await input.isVisible().catch(() => false))) {
    await page.locator(`[id="chatbtn-${agentId}"]`).click();
    await expect(input).toBeVisible({ timeout: 60_000 });
  }

  // The typing bubble shares .chat-msg.agent — exclude it to count real replies.
  const replies = page.locator(`[id="chatlog-${agentId}"] .chat-msg.agent:not(.typing)`);
  const deadline = Date.now() + timeout;
  let attempt = 0;

  while (true) {
    const before = await replies.count();
    await input.fill(message);
    await input.press('Enter');
    await expect(replies).toHaveCount(before + 1, { timeout });

    const reply = replies.nth(before);
    const text = (await reply.textContent())?.trim() ?? '';

    if (/^\[Error: agent unreachable/.test(text) && Date.now() < deadline) {
      attempt++;
      await page.waitForTimeout(5_000); // warm-up backoff, then re-send
      continue;
    }

    expect(text, `agent ${agentId} reply was an error after ${attempt} retr${attempt === 1 ? 'y' : 'ies'}`).not.toMatch(/^\[Error:/);
    return text;
  }
}

function childrenOf(agents: AgentSummary[]): Map<string, AgentSummary[]> {
  const m = new Map<string, AgentSummary[]>();
  for (const a of agents) {
    if (!a.parentId) continue;
    const arr = m.get(a.parentId) ?? [];
    arr.push(a);
    m.set(a.parentId, arr);
  }
  return m;
}

// All descendants of `rootId` (depth-first), excluding the node itself.
export function descendants(rootId: string, agents: AgentSummary[]): AgentSummary[] {
  const kids = childrenOf(agents);
  const out: AgentSummary[] = [];
  const walk = (id: string) => {
    for (const c of kids.get(id) ?? []) {
      out.push(c);
      walk(c.agentId);
    }
  };
  walk(rootId);
  return out;
}
