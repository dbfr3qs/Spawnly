import { expect, type Locator, type Page } from '@playwright/test';

// A stored spawn consent from GET /api/consents (proxied to the registry,
// scoped server-side to the logged-in user).
export interface ConsentRecord {
  id: string;
  userId: string;
  parentType: string;
  childType: string;
  scopes?: string[];
  grantedAt?: string;
  expiresAt?: string;
  revoked?: boolean;
}

export async function listConsents(page: Page): Promise<ConsentRecord[]> {
  const resp = await page.request.get('/api/consents');
  if (!resp.ok()) return [];
  return resp.json();
}

// Revoke every live stored consent so a test starts from "never consented".
// Consents persist in the registry across tests on a shared cluster; without
// this, a previously approved edge auto-approves and no prompt ever appears.
export async function revokeAllConsents(page: Page): Promise<void> {
  for (const c of await listConsents(page)) {
    if (!c.revoked) {
      await page.request.post(`/api/consents/${c.id}/revoke`).catch(() => {});
    }
  }
}

// The pending consent prompt cards rendered in the dashboard banner.
export function consentPrompts(page: Page): Locator {
  return page.locator('[data-testid="consent-request"]');
}

export interface ResolveConsentOpts {
  /**
   * Target the prompt belonging to this specific agent. Without it the first
   * card is used — fine when only one prompt can exist, ambiguous otherwise
   * (e.g. a dying agent's renewal re-consent sharing the banner).
   */
  agentId?: string;
  timeout?: number;
}

// Wait for a pending consent prompt and resolve it through the real UI.
// Returns after the targeted card disappears (the banner re-renders on poll).
export async function resolveConsentPrompt(
  page: Page,
  action: 'approve' | 'deny',
  opts: ResolveConsentOpts = {},
): Promise<void> {
  const scope = opts.agentId
    ? page.locator(`[data-testid="consent-request"][data-agent-id="${opts.agentId}"]`)
    : consentPrompts(page);
  const card = scope.first();
  await expect(card).toBeVisible({ timeout: opts.timeout ?? 120_000 });
  await card.locator(action === 'approve' ? '.btn-approve' : '.btn-deny').click();
  await expect(scope).toHaveCount(0, { timeout: 15_000 });
}
