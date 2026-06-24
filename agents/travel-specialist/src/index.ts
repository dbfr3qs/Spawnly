import express from "express";
import { v4 as uuidv4 } from "uuid";
import {
  DefaultRequestHandler,
  InMemoryTaskStore,
  type AgentExecutor,
  type ExecutionEventBus,
  type RequestContext,
} from "@a2a-js/sdk/server";
import { jsonRpcHandler, agentCardHandler, UserBuilder } from "@a2a-js/sdk/server/express";
import type { AgentCard, Message } from "@a2a-js/sdk";
import { postEvent, TokenClient } from "@spawnly/sdk";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

// A deterministic MCP-client agent. It holds ONE narrow capability: mint a token
// scoped to exactly MCP_SCOPE (its IdP client only allows that scope) and call the
// single travel-tools MCP tool MCP_TOOL. The provider keys live in the MCP server,
// never here — this agent only ever holds a scope-limited Spawnly token.
const agentId = process.env.AGENT_ID ?? "unknown";
const registryUrl = process.env.REGISTRY_URL ?? "http://registry:8080";
const sidecarUrl = process.env.SIDECAR_URL ?? "http://localhost:8089";
const mcpUrl = process.env.MCP_URL ?? "http://travel-tools/mcp";
const mcpTool = process.env.MCP_TOOL ?? ""; // e.g. search_flights
const mcpScope = process.env.MCP_SCOPE ?? ""; // e.g. flights:read

const tokens = new TokenClient(sidecarUrl);

// Call the travel-tools MCP tool with a freshly-minted, scope-limited token.
async function callMcpTool(args: Record<string, unknown>): Promise<unknown> {
  // Request ONLY the scope — never an explicit audience. The token's aud is
  // derived from the scope's ApiResource (=> travel-tools), and crucially the
  // sidecar routes a no-audience request through its consent (CIBA) path; passing
  // an explicit audience would bypass the spawn-consent gate (phase 4).
  const token = await tokens.getToken(mcpScope);
  const client = new Client({ name: `travel-specialist:${mcpTool}`, version: "1.0.0" });
  const transport = new StreamableHTTPClientTransport(new URL(mcpUrl), {
    requestInit: { headers: { Authorization: `Bearer ${token}` } },
  });
  try {
    await client.connect(transport);
    const res = await client.callTool({ name: mcpTool, arguments: args });
    const first = Array.isArray(res.content) ? res.content[0] : undefined;
    const text = first && first.type === "text" ? (first.text as string) : JSON.stringify(res);
    if (res.isError) throw new Error(text);
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  } finally {
    await client.close().catch(() => {});
  }
}

// The orchestrator passes the tool arguments as JSON. Contract for the phase-4
// caller: either set A2A message `metadata.params` to the args object (preferred),
// OR send a single text part whose text is the JSON args. Anything else yields {}
// and the tool is called with no args (and will fail its input validation).
function extractParams(message: Message | undefined): Record<string, unknown> {
  if (!message) return {};
  const meta = message.metadata?.params;
  if (meta && typeof meta === "object") return meta as Record<string, unknown>;
  for (const part of message.parts ?? []) {
    if (part.kind === "text" && typeof part.text === "string") {
      try {
        return JSON.parse(part.text) as Record<string, unknown>;
      } catch {
        /* not JSON — ignore */
      }
    }
  }
  return {};
}

const agentCard: AgentCard = {
  name: `travel-specialist:${mcpTool}`,
  description: `Calls the travel-tools "${mcpTool}" MCP tool using a ${mcpScope}-scoped token`,
  url: `http://${agentId}-svc:8080`,
  version: "1.0.0",
  protocolVersion: "0.2.6",
  capabilities: { streaming: false, pushNotifications: false, stateTransitionHistory: false },
  defaultInputModes: ["text/plain"],
  defaultOutputModes: ["text/plain"],
  skills: [
    {
      id: "search",
      name: "search",
      description: `Run ${mcpTool} with the given JSON parameters and return the result`,
      tags: ["travel", mcpTool],
    },
  ],
};

class SpecialistExecutor implements AgentExecutor {
  async execute(requestContext: RequestContext, eventBus: ExecutionEventBus): Promise<void> {
    try {
      const params = extractParams(requestContext.userMessage);
      const result = await callMcpTool(params);
      await postEvent(registryUrl, agentId, "tool_result", { tool: mcpTool, result });

      eventBus.publish({
        kind: "message",
        messageId: uuidv4(),
        role: "agent",
        parts: [{ kind: "text", text: JSON.stringify(result) }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      } satisfies Message);
      eventBus.finished();
    } catch (err) {
      const msg = err instanceof Error ? err.message : String(err);
      console.error(`[travel-specialist:${mcpTool}] error:`, msg);
      await postEvent(registryUrl, agentId, "agent_error", { error: msg });
      eventBus.publish({
        kind: "message",
        messageId: uuidv4(),
        role: "agent",
        parts: [{ kind: "text", text: JSON.stringify({ error: msg }) }],
        contextId: requestContext.contextId,
        taskId: requestContext.taskId,
      } satisfies Message);
      eventBus.finished();
    }
  }

  async cancelTask(_taskId: string, eventBus: ExecutionEventBus): Promise<void> {
    eventBus.finished();
  }
}

const requestHandler = new DefaultRequestHandler(agentCard, new InMemoryTaskStore(), new SpecialistExecutor());

const app = express();
app.use(express.json());
app.use("/.well-known/agent.json", agentCardHandler({ agentCardProvider: requestHandler }));
app.use("/.well-known/agent-card.json", agentCardHandler({ agentCardProvider: requestHandler }));
app.post("/", jsonRpcHandler({ requestHandler, userBuilder: UserBuilder.noAuthentication }));

app.listen(8080, () => {
  console.log(`[travel-specialist] ${agentId} tool=${mcpTool} scope=${mcpScope} listening on :8080`);
});
