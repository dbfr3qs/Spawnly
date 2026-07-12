import { expect } from '@playwright/test';

// Mailpit is the dev/e2e mail sink (deploy/manifests/mailpit.yaml), applied by
// scripts/bootstrap.sh and reached over its HTTP API which scripts/e2e.sh
// port-forwards to localhost:8025. Keycloak's realm SMTP points at it on kind,
// so the signup email-verification mail lands here where these helpers can poll
// for it and pull out the verification link. Override the base with MAILPIT_URL.
//
// API surface used (Mailpit v1.x):
//   GET    /api/v1/messages           → { messages: [{ ID, To:[{Address}], Created, Subject }] } (newest first)
//   GET    /api/v1/message/{ID}       → { Text, HTML, To, Subject, ... }
//   DELETE /api/v1/messages           → delete all (body {}) or a set ({IDs:[...]})
const MAILPIT_URL = process.env.MAILPIT_URL ?? 'http://localhost:8025';

interface MessageSummary {
  ID: string;
  Subject: string;
  Created: string;
  To: { Address: string }[];
}

interface MessageDetail {
  Text: string;
  HTML: string;
}

// listMessages returns the message summaries (newest first), optionally filtered
// to those addressed to `recipient` (case-insensitive exact match on any To).
export async function listMessages(recipient?: string): Promise<MessageSummary[]> {
  const resp = await fetch(`${MAILPIT_URL}/api/v1/messages`);
  if (!resp.ok) throw new Error(`mailpit list failed: ${resp.status} ${await resp.text()}`);
  const body = (await resp.json()) as { messages?: MessageSummary[] };
  const messages = body.messages ?? [];
  if (!recipient) return messages;
  const want = recipient.toLowerCase();
  return messages.filter((m) => (m.To ?? []).some((t) => t.Address.toLowerCase() === want));
}

// waitForMessage polls until a message addressed to `recipient` appears, then
// returns its full detail (Text + HTML). Fails the test (via expect.poll) if none
// arrives within the timeout — a delivered mail is the load-bearing signal that
// the SMTP path (Keycloak → Mailpit) actually works (criterion 1).
export async function waitForMessage(
  recipient: string,
  timeout = 30_000,
): Promise<MessageDetail> {
  let id = '';
  await expect
    .poll(
      async () => {
        const msgs = await listMessages(recipient).catch(() => []);
        if (msgs.length > 0) id = msgs[0].ID; // newest first
        return msgs.length;
      },
      {
        message: `no mail delivered to ${recipient} within ${timeout}ms — is Mailpit up and Keycloak SMTP wired?`,
        timeout,
      },
    )
    .toBeGreaterThan(0);

  const resp = await fetch(`${MAILPIT_URL}/api/v1/message/${id}`);
  if (!resp.ok) throw new Error(`mailpit detail failed: ${resp.status} ${await resp.text()}`);
  return (await resp.json()) as MessageDetail;
}

// verificationLink polls for the email-verification mail to `recipient` and pulls
// out Keycloak's action-token link (…/login-actions/action-token?key=…). Keycloak
// pins the frontend hostname to the dashboard origin (KC_HOSTNAME), so the link is
// an http://localhost:8090/realms/spawnly/… URL the Playwright browser can visit
// directly through the single-origin proxy.
export async function verificationLink(recipient: string, timeout = 30_000): Promise<string> {
  const detail = await waitForMessage(recipient, timeout);
  // Prefer the plaintext body; fall back to HTML. Decode &amp; (HTML bodies encode
  // the query-string ampersands) so the link is usable verbatim.
  const body = `${detail.Text ?? ''}\n${detail.HTML ?? ''}`.replace(/&amp;/g, '&');
  const match = body.match(/https?:\/\/[^\s"'<>]*login-actions\/action-token[^\s"'<>]*/);
  if (!match) {
    throw new Error(`no verification link found in mail to ${recipient}: ${body.slice(0, 500)}`);
  }
  return match[0];
}

// clearMessages deletes ALL messages in the sink. Call before a signup test so a
// stale verification mail from an earlier run can't be picked up in place of the
// fresh one (recipients are per-run unique too, but this keeps the sink tidy).
export async function clearMessages(): Promise<void> {
  const resp = await fetch(`${MAILPIT_URL}/api/v1/messages`, {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: '{}',
  });
  if (!resp.ok) throw new Error(`mailpit clear failed: ${resp.status} ${await resp.text()}`);
}
