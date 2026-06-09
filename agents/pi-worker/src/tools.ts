// A Pi custom tool whose backing call is governed by the Spawnly platform.
//
// The point: a Pi-native tool (registered via Pi's own `defineTool`/`customTools`)
// reaches a protected service using THIS agent's platform identity — the
// SPIFFE-issued, sidecar-exchanged, scoped OAuth token — through the SDK's
// generic `TokenClient` + `createAuthenticatedFetch`. No platform changes, no
// bespoke wiring: Pi calls the same authz path every other agent uses.

import { defineTool, type ToolDefinition } from '@earendil-works/pi-coding-agent';
import { Type } from 'typebox';
import { TokenClient, createAuthenticatedFetch } from '@spawnly/sdk';

export interface ProtectedApiToolOptions {
  /** Base URL of the protected Sample API (e.g. http://sample-api-global). */
  sampleApiUrl: string;
  /** OAuth scope to request (e.g. sample-api-a:read). */
  scope: string;
  /** Sidecar /token endpoint. */
  sidecarUrl: string;
  /** Tenant id, or undefined for a global (tenant-less) agent. */
  tenantId?: string;
}

export function makeProtectedApiTool(opts: ProtectedApiToolOptions): ToolDefinition {
  const tokens = new TokenClient(opts.sidecarUrl);
  const authFetch = createAuthenticatedFetch(opts.sampleApiUrl, opts.scope, tokens, opts.tenantId);

  return defineTool({
    name: 'check_protected_api',
    label: 'Check Protected API',
    description:
      "Call the platform's protected Sample API (GET /work) using this agent's " +
      'Spawnly identity (a SPIFFE-issued, scoped OAuth token obtained via the sidecar). ' +
      'Use this when the user asks to call, check, or hit the protected/Sample API. ' +
      'Returns the HTTP status and the JSON the API responds with.',
    promptSnippet: 'Call the protected Sample API using this agent\'s Spawnly identity',
    promptGuidelines: [
      'When the user asks to call/check/hit the protected API or Sample API, use the check_protected_api tool — do not claim you lack API access.',
    ],
    parameters: Type.Object({}),
    async execute() {
      try {
        const res = await authFetch('/work', { method: 'GET' });
        let body: unknown;
        try {
          body = await res.json();
        } catch {
          body = await res.text().catch(() => '');
        }
        return {
          content: [
            { type: 'text', text: `Sample API GET /work -> ${res.status}\n${JSON.stringify(body)}` },
          ],
          details: {},
        };
      } catch (err) {
        const msg = err instanceof Error ? err.message : String(err);
        return {
          content: [{ type: 'text', text: `Sample API call failed: ${msg}` }],
          details: {},
        };
      }
    },
  });
}
