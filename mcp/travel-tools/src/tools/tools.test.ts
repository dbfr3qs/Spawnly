import { describe, it, expect } from "vitest";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { registerTools, TOOLS } from "./index.js";
import { ScopeError } from "../scopes.js";
import { configFromEnv } from "../config.js";

type Handler = (args: Record<string, unknown>) => Promise<{ content: Array<{ text: string }> }>;

function captureServer() {
  const handlers: Record<string, Handler> = {};
  const server = {
    registerTool: (name: string, _config: unknown, handler: Handler) => {
      handlers[name] = handler;
    },
  };
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  return { server: server as any, handlers };
}

const auth = (scopes: string[]): AuthInfo => ({ token: "", clientId: "", scopes });
// Empty env => no provider keys, so flights/hotels return "not configured" with no network.
const cfg = configFromEnv({});

describe("tool registration + central scope gate", () => {
  it("registers exactly the declared tools", () => {
    const { server, handlers } = captureServer();
    registerTools(server, auth([]), cfg);
    expect(Object.keys(handlers).sort()).toEqual(TOOLS.map((t) => t.name).sort());
  });

  // The load-bearing invariant: no tool is callable without its declared scope.
  it("EVERY tool throws ScopeError when the token lacks its scope", async () => {
    const { server, handlers } = captureServer();
    registerTools(server, auth([]), cfg);
    for (const def of TOOLS) {
      await expect(handlers[def.name]({})).rejects.toThrow(ScopeError);
    }
  });

  it("passes the gate with the right scope (flights/hotels report not-configured w/o keys)", async () => {
    const { server, handlers } = captureServer();
    registerTools(server, auth(TOOLS.map((t) => t.scope)), cfg);

    const flights = await handlers["search_flights"]({
      origin: "AKL",
      destination: "SYD",
      departureDate: "2026-09-15",
      adults: 1,
      cabin: "economy",
    });
    expect(JSON.parse(flights.content[0].text).configured).toBe(false);

    const hotels = await handlers["search_hotels"]({
      cityName: "Auckland",
      countryCode: "NZ",
      checkIn: "2026-09-15",
      checkOut: "2026-09-17",
      adults: 2,
      currency: "NZD",
    });
    expect(JSON.parse(hotels.content[0].text).configured).toBe(false);
  });
});
