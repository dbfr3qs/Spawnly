import express from "express";
import { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { requireBearerAuth } from "@modelcontextprotocol/sdk/server/auth/middleware/bearerAuth.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { SpawnlyTokenVerifier } from "./auth.js";
import { registerCurrencyTool } from "./tools/currency.js";

const PORT = Number(process.env.PORT ?? 8080);
const ISSUER = process.env.IS_ISSUER ?? "http://identity-server:8080";
const JWKS_URL =
  process.env.IS_JWKS_URL ?? "http://identity-server:8080/.well-known/openid-configuration/jwks";
// The RFC 8707 resource identifier this server accepts tokens for (token `aud`).
const AUDIENCE = process.env.RESOURCE_AUDIENCE ?? "travel-tools";

const verifier = new SpawnlyTokenVerifier({ issuer: ISSUER, jwksUrl: JWKS_URL, audience: AUDIENCE });

// A fresh server per request (stateless transport) lets each tool handler close
// over THIS request's validated AuthInfo, so per-tool scope checks are exact and
// there is no cross-request session state to manage.
function buildServer(auth: AuthInfo | undefined): McpServer {
  const server = new McpServer({ name: "travel-tools", version: "1.0.0" });
  registerCurrencyTool(server, auth);
  // Phase 2 adds search_flights / search_hotels here.
  return server;
}

const app = express();
// Explicit, small body cap (MCP tool calls are tiny); don't rely on the default.
app.use(express.json({ limit: "64kb" }));

app.get("/healthz", (_req, res) => {
  res.json({ status: "ok" });
});

// Every MCP request requires a valid Spawnly bearer (signature/iss/aud/exp);
// per-tool scope is enforced inside each tool handler.
app.post("/mcp", requireBearerAuth({ verifier }), async (req, res) => {
  const server = buildServer(req.auth);
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  res.on("close", () => {
    transport.close();
    server.close();
  });
  try {
    await server.connect(transport);
    await transport.handleRequest(req, res, req.body);
  } catch (err) {
    console.error("[travel-tools] request handling failed:", err);
    if (!res.headersSent) {
      res.status(500).json({
        jsonrpc: "2.0",
        error: { code: -32603, message: "internal error" },
        id: null,
      });
    }
  }
});

app.listen(PORT, () => {
  console.log(`travel-tools MCP server listening on :${PORT} (aud=${AUDIENCE}, iss=${ISSUER})`);
});
