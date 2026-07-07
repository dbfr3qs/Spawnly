import { test, expect, type BrowserContext, type Page } from '@playwright/test';
import { contextFor } from '../helpers/auth';
import { spawn, listAgents } from '../helpers/dashboard';
import { killAgent } from '../helpers/cleanup';

// Scenario — org membership self-service + removed-member disposition (spec
// criterion 8), proven through the real dashboard → orchestrator → registry path.
// acme: alice (org admin) + viewer (member). The org admin adds/removes/promotes
// members; a removed member loses access; and BOTH removal dispositions are
// observed end-to-end:
//   • auto-revoke   → the ex-member's agent flips to `revoked`;
//   • orphan-to-admin → the agent keeps running and the org admin still manages it.
//
// This file runs before orgs.spec.ts (alphabetical, single worker) and RESTORES
// viewer's acme membership + the default disposition on the way out, so it leaves
// the seed untouched for the rest of the suite.

interface MemberRow {
  userId: string;
  username?: string;
  role?: string;
}

async function members(page: Page): Promise<MemberRow[]> {
  const r = await page.request.get('/api/org/members');
  expect(r.ok(), `list members: ${r.status()}`).toBeTruthy();
  return r.json();
}

async function setDisposition(alice: Page, disposition: 'auto-revoke' | 'orphan-to-admin') {
  const r = await alice.request.patch('/api/org', { data: { removalDisposition: disposition } });
  expect(r.ok(), `set disposition ${disposition}: ${r.status()}`).toBeTruthy();
}

async function addMember(alice: Page, user: string, role: 'member' | 'admin' = 'member') {
  const r = await alice.request.post('/api/org/members', { data: { user, role } });
  expect(r.ok(), `add member ${user}: ${r.status()} ${await r.text()}`).toBeTruthy();
}

async function waitActive(page: Page, agentId: string, timeout = 120_000) {
  await expect
    .poll(async () => (await listAgents(page)).find((a) => a.agentId === agentId)?.status ?? '', { timeout })
    .toMatch(/active/i);
}

test.describe('org membership self-service', () => {
  let aliceCtx: BrowserContext;
  let viewerCtx: BrowserContext;
  let alice: Page;
  let viewer: Page;
  const spawned: string[] = [];

  test.beforeAll(async ({ browser }) => {
    aliceCtx = await contextFor(browser, 'alice', 'acme');
    viewerCtx = await contextFor(browser, 'viewer', 'acme');
    alice = await aliceCtx.newPage();
    viewer = await viewerCtx.newPage();
    await Promise.all([alice.goto('/'), viewer.goto('/')]);
  });

  test.afterAll(async () => {
    // Restore the seed: viewer is a member of acme, disposition back to default.
    await addMember(alice, 'viewer', 'member').catch(() => {});
    await setDisposition(alice, 'orphan-to-admin').catch(() => {});
    // Sweep any agents this file spawned (alice is org admin → can kill them all).
    for (const id of spawned) await killAgent(alice, id).catch(() => {});
    await aliceCtx?.close();
    await viewerCtx?.close();
  });

  test('the Members page renders for the org admin', async () => {
    await alice.goto('/');
    await expect(alice.locator('[data-testid="open-members"]')).toBeVisible({ timeout: 30_000 });
    await alice.locator('[data-testid="open-members"]').click();
    await expect(alice.locator('[data-testid="members-view"]')).toBeVisible();
    // The table lists at least alice + viewer.
    await expect(alice.locator('.members-table')).toContainText('alice', { timeout: 15_000 });
    await expect(alice.locator('.members-table')).toContainText('viewer');
  });

  test('the org admin adds, promotes, demotes and removes a member', async () => {
    const temp = `e2e-temp-${Date.now()}`;
    // Add as member.
    await addMember(alice, temp, 'member');
    let row = (await members(alice)).find((m) => m.userId === temp);
    expect(row?.role, 'newly added member role').toBe('member');

    // Promote → admin.
    let r = await alice.request.patch(`/api/org/members/${temp}`, { data: { role: 'admin' } });
    expect(r.ok(), `promote: ${r.status()}`).toBeTruthy();
    row = (await members(alice)).find((m) => m.userId === temp);
    expect(row?.role, 'after promote').toBe('admin');

    // Demote → member.
    r = await alice.request.patch(`/api/org/members/${temp}`, { data: { role: 'member' } });
    expect(r.ok(), `demote: ${r.status()}`).toBeTruthy();
    row = (await members(alice)).find((m) => m.userId === temp);
    expect(row?.role, 'after demote').toBe('member');

    // Remove.
    r = await alice.request.delete(`/api/org/members/${temp}`);
    expect(r.ok(), `remove: ${r.status()}`).toBeTruthy();
    expect((await members(alice)).some((m) => m.userId === temp), 'removed member gone').toBe(false);
  });

  test('auto-revoke: a removed member’s agent flips to revoked and they lose access', async () => {
    await setDisposition(alice, 'auto-revoke');

    // viewer (member) spawns an agent in acme, then reaches active (SELF-registered,
    // owner/org tuples written).
    const agentId = await spawn(viewer, 'weather-monitor');
    spawned.push(agentId);
    await waitActive(viewer, agentId);

    // alice removes viewer → the disposition fires.
    const rm = await alice.request.delete('/api/org/members/viewer');
    expect(rm.ok(), `remove viewer: ${rm.status()}`).toBeTruthy();

    // The agent flips to revoked (observed from the admin's list, which sees all
    // acme agents).
    await expect
      .poll(async () => (await listAgents(alice)).find((a) => a.agentId === agentId)?.status ?? '', {
        timeout: 30_000,
      })
      .toMatch(/revoked/i);

    // The ex-member has lost access: her owner tuple is dropped with her
    // membership, so a direct read AND a mutate on the agent both 404 (no
    // owner-based backdoor after removal).
    await expect
      .poll(async () => (await viewer.request.get(`/api/agents/${agentId}/events`)).status(), { timeout: 30_000 })
      .toBe(404);
    expect((await viewer.request.post(`/api/agents/${agentId}/revoke`)).status(), 'ex-member cannot mutate').toBe(404);

    // Restore viewer's membership for the next test.
    await addMember(alice, 'viewer', 'member');
  });

  test('orphan-to-admin: a removed member’s agent keeps running and the admin still manages it', async () => {
    await setDisposition(alice, 'orphan-to-admin');

    const agentId = await spawn(viewer, 'weather-monitor');
    spawned.push(agentId);
    await waitActive(viewer, agentId);

    const rm = await alice.request.delete('/api/org/members/viewer');
    expect(rm.ok(), `remove viewer: ${rm.status()}`).toBeTruthy();

    // The agent stays active (not revoked) and the org admin still manages it: a
    // revoke/resume round-trip by alice succeeds.
    const row = (await listAgents(alice)).find((a) => a.agentId === agentId);
    expect(row?.status, 'orphaned agent stays running').toMatch(/active/i);
    expect(row?.canManage, 'admin still manages the orphaned agent').toBeTruthy();
    const revoke = await alice.request.post(`/api/agents/${agentId}/revoke`);
    expect(revoke.ok(), `admin revoke orphaned agent: ${revoke.status()}`).toBeTruthy();
    const resume = await alice.request.post(`/api/agents/${agentId}/resume`);
    expect(resume.ok(), `admin resume orphaned agent: ${resume.status()}`).toBeTruthy();

    // The ex-member has lost access even though the agent still runs: her owner
    // tuple was dropped on removal, so she can no longer read or mutate it.
    await expect
      .poll(async () => (await viewer.request.get(`/api/agents/${agentId}/events`)).status(), { timeout: 30_000 })
      .toBe(404);

    // Restore viewer's membership (seed hygiene for the rest of the suite).
    await addMember(alice, 'viewer', 'member');
  });
});
