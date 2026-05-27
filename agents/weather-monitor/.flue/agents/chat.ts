import { TokenClient, createAuthenticatedFetch } from "@agent-platform/sdk";

export const triggers = { webhook: true };

const agentId = process.env.AGENT_ID ?? "unknown";
const sampleApiUrl = process.env.SAMPLE_API_URL ?? "http://sample-api:8080";

const tokenClient = new TokenClient();
const apiFetch = createAuthenticatedFetch(sampleApiUrl, "sample-api", tokenClient);

interface ChatPayload {
  message: string;
  sessionId?: string;
}

export default async function ({ payload }: { payload: ChatPayload }) {
  return {
    response: `[Weather Monitor] Received: "${payload.message}". I'm running on a loop — full weather checking coming soon!`,
    agentId,
    timestamp: new Date().toISOString(),
  };
}
