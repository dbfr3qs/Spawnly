export const triggers = { webhook: true };

interface ChatPayload {
  message: string;
  sessionId?: string;
}

export default async function ({ payload }: { payload: ChatPayload }) {
  return {
    response: `[Weather Monitor] Received: "${payload.message}". I'm running on a loop — full weather checking coming soon!`,
    agentId: process.env.AGENT_ID ?? "unknown",
    timestamp: new Date().toISOString(),
  };
}
