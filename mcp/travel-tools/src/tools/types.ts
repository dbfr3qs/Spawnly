import type { z } from "zod";
import type { CallToolResult } from "@modelcontextprotocol/sdk/types.js";
import type { ToolConfig } from "../config.js";

/** The MCP tool-result shape we return (the SDK's CallToolResult). */
export type ToolResult = CallToolResult;

/** Convenience: wrap a JSON-serializable value as a tool text result. */
export function jsonResult(value: unknown): ToolResult {
  return { content: [{ type: "text", text: JSON.stringify(value) }] };
}

/**
 * A travel tool declares the single scope that authorizes it. Scope enforcement
 * is centralized in the registration loop (registerTools), so a tool physically
 * cannot be exposed without its gate — `run` is only ever invoked after the
 * caller's token has been checked for `scope`.
 */
export interface TravelToolDef {
  name: string;
  scope: string;
  config: {
    title: string;
    description: string;
    inputSchema: z.ZodRawShape;
  };
  // args are already validated against inputSchema by the MCP SDK before run().
  run(args: Record<string, unknown>, cfg: ToolConfig): Promise<ToolResult>;
}
