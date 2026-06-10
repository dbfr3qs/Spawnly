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

// Wait for a pending consent prompt and resolve it through the real UI.
// Returns after the card disappears (the banner re-renders on the next poll).
export async function resolveConsentPrompt(
  page: Page,
  action: 'approve' | 'deny',
  timeout = 120_000,
): Promise<void> {
  const card = consentPrompts(page).first();
  await expect(card).toBeVisible({ timeout });
  const before = await consentPrompts(page).count();
  await card.locator(action === 'approve' ? '.btn-approve' : '.btn-deny').click();
  await expect(consentPrompts(page)).toHaveCount(before - 1, { timeout: 15_000 });
}
