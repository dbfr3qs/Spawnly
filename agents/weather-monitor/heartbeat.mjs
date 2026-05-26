const REGISTRY_URL = process.env.REGISTRY_URL ?? 'http://registry:8080';
const AGENT_ID = process.env.AGENT_ID ?? 'unknown';

console.log(`[heartbeat] started for agent ${AGENT_ID}`);

async function beat() {
  try {
    const res = await fetch(`${REGISTRY_URL}/v1/agents/${AGENT_ID}/events`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        source: 'agent',
        type: 'heartbeat',
        payload: { status: 'running', timestamp: new Date().toISOString() },
      }),
    });
    console.log(`[heartbeat] posted — ${res.status}`);
  } catch (e) {
    console.warn(`[heartbeat] failed: ${e.message}`);
  }
}

setInterval(beat, 30_000);
