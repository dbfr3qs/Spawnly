import { z } from "zod";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { requireScope, SCOPE_FX } from "../scopes.js";
import { upstreamUnavailable } from "../upstream.js";

// Frankfurter publishes ECB reference rates — real data, no API key. Pinned to
// the canonical host (the legacy .app host now 301-redirects). Overridable for
// tests / air-gapped runs. The host is fixed and inputs are regex-validated, so
// the request URL is fully server-controlled (no SSRF surface).
const FRANKFURTER_URL = process.env.FRANKFURTER_URL ?? "https://api.frankfurter.dev/v1";
const ISO_4217 = /^[A-Za-z]{3}$/;
const UPSTREAM_TIMEOUT_MS = 8000;

interface FrankfurterLatest {
  amount: number;
  base: string;
  date: string;
  rates: Record<string, number>;
}

/**
 * Registers `convert_currency`. Gated on `fx:read`; on success it calls the real
 * Frankfurter API and returns the converted amount + the rate used. Input is
 * validated (positive amount, ISO-4217 codes) before any upstream call.
 */
export function registerCurrencyTool(server: McpServer, auth: AuthInfo | undefined): void {
  server.registerTool(
    "convert_currency",
    {
      title: "Convert currency",
      description:
        "Convert an amount from one ISO-4217 currency to another using live ECB reference rates. Requires the fx:read scope.",
      inputSchema: {
        amount: z.number().positive().describe("Amount in the source currency, e.g. 250"),
        from: z.string().regex(ISO_4217).describe("ISO-4217 source currency code, e.g. NZD"),
        to: z.string().regex(ISO_4217).describe("ISO-4217 target currency code, e.g. AUD"),
      },
    },
    async ({ amount, from, to }) => {
      requireScope(auth, SCOPE_FX);

      const f = from.toUpperCase();
      const t = to.toUpperCase();

      const result = (converted: number, rate: number, asOf: string) => ({
        content: [
          {
            type: "text" as const,
            text: JSON.stringify({
              amount,
              from: f,
              to: t,
              converted: Number(converted.toFixed(2)),
              rate: Number(rate.toFixed(6)),
              asOf,
              source: "ECB reference rates",
            }),
          },
        ],
      });

      // Same currency: no upstream call needed (and Frankfurter omits the self-rate).
      if (f === t) return result(amount, 1, new Date().toISOString().slice(0, 10));

      const url = `${FRANKFURTER_URL}/latest?amount=${encodeURIComponent(amount)}&from=${f}&to=${t}`;
      let data: FrankfurterLatest;
      try {
        // redirect:"error" — the host is pinned + canonical, so any redirect is
        // unexpected and must not silently pivot egress to another host.
        const res = await fetch(url, {
          redirect: "error",
          signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
        });
        if (!res.ok) throw new Error(`status ${res.status}`);
        data = (await res.json()) as FrankfurterLatest;
      } catch (err) {
        // Never leak the upstream URL/body/error to the caller.
        throw upstreamUnavailable("currency conversion", err);
      }

      const converted = data.rates?.[t];
      if (typeof converted !== "number") {
        throw upstreamUnavailable("currency conversion", `no rate for ${f}->${t}`);
      }
      return result(converted, converted / amount, data.date);
    },
  );
}
