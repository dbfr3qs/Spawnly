import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import type { ToolConfig } from "../config.js";
import { requireScope } from "../scopes.js";
import type { TravelToolDef } from "./types.js";
import { currencyTool } from "./currency.js";
import { flightsTool } from "./flights.js";
import { hotelsTool } from "./hotels.js";

// Every travel tool. Adding one here automatically gates it on its declared
// scope (see registerTools) — there is no way to register a tool without a gate.
export const TOOLS: TravelToolDef[] = [currencyTool, flightsTool, hotelsTool];

/**
 * Registers every tool on the server, wrapping each handler with a CENTRAL scope
 * check against the request's AuthInfo. The scope gate runs before the tool's
 * `run` (so no upstream work happens unauthorized), and because it lives in this
 * loop — not in each handler — a tool physically cannot be exposed ungated.
 */
export function registerTools(server: McpServer, auth: AuthInfo | undefined, cfg: ToolConfig): void {
  for (const def of TOOLS) {
    server.registerTool(def.name, def.config, async (args: Record<string, unknown>) => {
      requireScope(auth, def.scope);
      return def.run(args, cfg);
    });
  }
}
