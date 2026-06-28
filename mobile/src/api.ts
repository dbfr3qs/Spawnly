import { config } from './config';
import { accessToken } from './auth';

// A pending CIBA spawn-consent request, as returned by the gateway (proxied from
// the orchestrator's user-scoped endpoints).
export interface ConsentRequest {
  id: string;
  agentId?: string;
  parentType: string;
  childType: string;
  scopes: string[];
  bindingMessage?: string;
  status: string;
  createdAt: string;
}

// A standing consent grant (the management view).
export interface ConsentRecord {
  id: string;
  parentType: string;
  childType: string;
  scopes?: string[];
  grantedAt?: string;
  expiresAt?: string;
  revoked?: boolean;
}

async function authedFetch(path: string, init: RequestInit = {}): Promise<Response> {
  const token = await accessToken();
  if (!token) throw new Error('not authenticated');
  const headers = new Headers(init.headers);
  headers.set('Authorization', `Bearer ${token}`);
  if (init.body) headers.set('Content-Type', 'application/json');
  const resp = await fetch(`${config.gatewayUrl}${path}`, { ...init, headers });
  return resp;
}

export async function listPending(): Promise<ConsentRequest[]> {
  const resp = await authedFetch('/me/consent-requests?status=pending');
  if (!resp.ok) throw new Error(`list pending: ${resp.status}`);
  return resp.json();
}

// getRequest re-fetches a single request's authoritative state — the app never
// trusts the push payload; it always reads the request over this channel.
export async function getRequest(id: string): Promise<ConsentRequest> {
  const resp = await authedFetch(`/me/consent-requests/${encodeURIComponent(id)}`);
  if (resp.status === 404) throw new AlreadyHandledError();
  if (!resp.ok) throw new Error(`get request: ${resp.status}`);
  return resp.json();
}

// approve resolves a request. When scopes is provided it narrows the grant to
// that subset (the gateway forwards it to the orchestrator's approve endpoint).
export async function approve(id: string, scopes?: string[]): Promise<void> {
  const resp = await authedFetch(`/me/consent-requests/${encodeURIComponent(id)}/approve`, {
    method: 'POST',
    body: scopes ? JSON.stringify({ scopes }) : undefined,
  });
  if (!resp.ok) throw new Error(`approve: ${resp.status}`);
}

export async function deny(id: string): Promise<void> {
  const resp = await authedFetch(`/me/consent-requests/${encodeURIComponent(id)}/deny`, {
    method: 'POST',
  });
  if (!resp.ok) throw new Error(`deny: ${resp.status}`);
}

export async function listConsents(): Promise<ConsentRecord[]> {
  const resp = await authedFetch('/me/consents');
  if (!resp.ok) throw new Error(`list consents: ${resp.status}`);
  return resp.json();
}

export async function revokeConsent(id: string): Promise<void> {
  const resp = await authedFetch(`/me/consents/${encodeURIComponent(id)}/revoke`, { method: 'POST' });
  if (!resp.ok) throw new Error(`revoke: ${resp.status}`);
}

export async function registerDevice(platform: 'ios' | 'android', pushToken: string): Promise<void> {
  const resp = await authedFetch('/me/devices', {
    method: 'POST',
    body: JSON.stringify({ platform, pushToken }),
  });
  if (!resp.ok) throw new Error(`register device: ${resp.status}`);
}

// AlreadyHandledError signals a request that is no longer pending (resolved on
// the dashboard or another device) — the UI shows "already handled".
export class AlreadyHandledError extends Error {
  constructor() {
    super('This request has already been handled.');
    this.name = 'AlreadyHandledError';
  }
}
