import { TokenClient, tenantHeader, postEvent, spawn } from '@spawnly/sdk';

// Identity and platform endpoints are injected by the operator (see
// internal/operator/reconciler.go buildPod). The sidecar runs in the same pod
// and exposes /token on localhost:8089.
const agentId         = process.env.AGENT_ID         ?? 'unknown';
const agentType       = process.env.AGENT_TYPE       ?? 'chain-worker';
const parentId        = process.env.PARENT_ID        || '';
const tenantId        = process.env.TENANT_ID        || undefined;
const registryUrl     = process.env.REGISTRY_URL     ?? 'http://registry:8080';
const orchestratorUrl = process.env.ORCHESTRATOR_URL ?? 'http://orchestrator:8080';
const sidecarUrl      = process.env.SIDECAR_URL      ?? 'http://localhost:8089';
const apiUrl          = process.env.API_A_URL        ?? 'http://sample-api-a:8080';
const scope           = process.env.SCOPE            ?? 'sample-api-a:read';
const workIntervalMs  = Number(process.env.WORK_INTERVAL_MS ?? 3000);

const tokens = new TokenClient(sidecarUrl);

const sleep = (ms: number) => new Promise<void>((resolve) => setTimeout(resolve, ms));

// Extend the chain by one: spawn a single child of our own type, parented to us.
// The platform's spawn-policy caps total chain length (the template's maxDepth),
// so the deepest worker's request is denied (403) and the chain stops growing —
// that denial is expected and just marks the end of the chain.
async function spawnChild(): Promise<void> {
  try {
    // The orchestrator derives userId/parentId/tenantId from our spawn token +
    // the registry, so we only send agentType.
    const r = await spawn(orchestratorUrl, tokens, agentType);
    if (r.ok) {
      console.log(`[chain-worker] spawned child ${r.workloadName}`);
      await postEvent(registryUrl, agentId, 'child_spawned', { childId: r.workloadName });
    } else if (r.status === 403) {
      // Depth cap reached — we are the last link in the chain.
      console.log('[chain-worker] depth cap reached; no child spawned');
      await postEvent(registryUrl, agentId, 'chain_end', { reason: r.body });
    } else {
      console.warn(`[chain-worker] spawn failed: ${r.status}`);
      await postEvent(registryUrl, agentId, 'child_spawn_error', { status: r.status, body: r.body });
    }
  } catch (err) {
    console.error('[chain-worker] spawn child error:', err);
    await postEvent(registryUrl, agentId, 'child_spawn_error', { error: String(err) });
  }
}

// One unit of work: call the protected sample API with our OWN access token.
// While authorized this returns 200; the moment our authorization is revoked
// (our SpiceDB work_on relation is dropped) the same call returns 403 — the
// visible, real-time effect of a cascading revoke, even though the pod is alive
// and still holds a cryptographically valid token.
async function doWork(): Promise<void> {
  let token: string;
  try {
    token = await tokens.getToken(scope);
  } catch (err) {
    await postEvent(registryUrl, agentId, 'work_error', { phase: 'token', error: String(err) });
    return;
  }
  try {
    const res = await fetch(`${apiUrl}/work`, {
      method: 'GET',
      headers: { Authorization: `Bearer ${token}`, ...tenantHeader(tenantId) },
    });
    if (res.ok) {
      console.log(`[chain-worker] work -> ${res.status}`);
      await postEvent(registryUrl, agentId, 'work_ok', { status: res.status });
    } else {
      console.log(`[chain-worker] work DENIED -> ${res.status}`);
      await postEvent(registryUrl, agentId, 'work_denied', { status: res.status });
    }
  } catch (err) {
    await postEvent(registryUrl, agentId, 'work_error', { phase: 'call', error: String(err) });
  }
}

async function main(): Promise<void> {
  console.log(`[chain-worker] ${agentId} starting (tenant=${tenantId ?? '-'}, parent=${parentId || 'root'})`);
  await postEvent(registryUrl, agentId, 'started', { agentId, parentId });

  // Grow the chain once at startup (best-effort; the depth cap stops runaway growth).
  await spawnChild();

  // Work forever until the pod is killed. Each tick is a 200 while authorized
  // and a 403 once this agent's authorization is revoked.
  for (;;) {
    await doWork();
    await sleep(workIntervalMs);
  }
}

main().catch(async (err) => {
  console.error('[chain-worker] fatal:', err);
  await postEvent(registryUrl, agentId, 'error', { error: String(err) });
  process.exit(1);
});
