import { readFileSync } from 'node:fs';
import path from 'node:path';
import { expect, type APIRequestContext, type Page } from '@playwright/test';

// Template-lifecycle helpers for the control-plane API the dashboard proxies to
// the orchestrator (which forwards to the registry):
//   POST   /api/templates                 — register a full template JSON
//   PATCH  /api/templates/{agentType}     — { status: 'active' | 'disabled' }
//   DELETE /api/templates/{agentType}     — only when disabled (else 409)
//   GET    /api/templates                 — spawnable type names; disabled excluded
//
// All routes are authenticated; pass a request context that carries the cached
// session (e.g. `page.request`, which shares baseURL + cookies).

// A registry template document. We only type the few fields the helpers touch;
// the rest is forwarded verbatim, so keep it open via an index signature.
export interface Template {
  agentType: string;
  meta?: { displayName?: string; description?: string };
  [key: string]: unknown;
}

// Register a template. Asserts the POST succeeded (~201).
export async function registerTemplate(
  request: APIRequestContext,
  template: Template,
): Promise<void> {
  const resp = await request.post('/api/templates', { data: template });
  expect(
    resp.ok(),
    `register ${template.agentType} failed: ${resp.status()} ${await resp.text()}`,
  ).toBeTruthy();
}

// PATCH a template's status. Returns the response so the caller asserts the
// status code (200 ok, 404 unknown, 400 invalid status).
export async function setTemplateStatus(
  request: APIRequestContext,
  agentType: string,
  status: 'active' | 'disabled',
) {
  return request.patch(`/api/templates/${agentType}`, { data: { status } });
}

// DELETE a template. Returns the response so the caller asserts the status code
// (204 deleted, 404 unknown, 409 if not currently disabled).
export async function deleteTemplate(request: APIRequestContext, agentType: string) {
  return request.delete(`/api/templates/${agentType}`);
}

// Repo root is two levels up from e2e/helpers/. Resolve from this module's
// directory (Playwright transpiles specs to CJS, so __dirname is available) so
// the path holds regardless of the process cwd Playwright launches with.
const REPO_ROOT = path.resolve(__dirname, '..', '..');

// Read a real, schema-valid template (weather-monitor) and clone it under a
// fresh agentType so it registers as a brand-new throwaway type. displayName is
// tweaked too so the clone is identifiable in the UI.
export function cloneSampleTemplate(agentType: string): Template {
  const raw = readFileSync(
    path.join(REPO_ROOT, 'agents', 'weather-monitor', 'template.json'),
    'utf8',
  );
  const tmpl = JSON.parse(raw) as Template;
  tmpl.agentType = agentType;
  tmpl.meta = {
    ...tmpl.meta,
    displayName: `Disposable Worker (${agentType})`,
  };
  return tmpl;
}

// Whether the spawn modal offers `agentType`. Opens the modal (mirroring
// helpers/dashboard.ts) and checks the #f-type <select>, which the page
// populates from GET /api/templates — so a disabled/deleted type is absent.
export async function isTypeOffered(page: Page, agentType: string): Promise<boolean> {
  await page.locator('#open-modal-btn').click();
  const option = page.locator(`#f-type option[value="${agentType}"]`);
  // The list loads asynchronously after the modal opens; give it a moment to
  // settle so a present option is reliably seen before we count.
  await page
    .locator('#f-type option')
    .first()
    .waitFor({ state: 'attached', timeout: 15_000 })
    .catch(() => {
      /* no options yet — count below still resolves to 0 */
    });
  return (await option.count()) > 0;
}
