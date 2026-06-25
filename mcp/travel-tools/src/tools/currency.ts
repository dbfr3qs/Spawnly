import { z } from "zod";
import type { ToolConfig } from "../config.js";
import { SCOPE_FX } from "../scopes.js";
import { upstreamUnavailable } from "../upstream.js";
import { jsonResult, type TravelToolDef } from "./types.js";

const ISO_4217 = /^[A-Za-z]{3}$/;
const UPSTREAM_TIMEOUT_MS = 8000;

interface FrankfurterLatest {
  amount: number;
  base: string;
  date: string;
  rates: Record<string, number>;
}

/**
 * convert_currency — real ECB reference rates via Frankfurter (no API key).
 * Gated on fx:read. Inputs are validated (positive amount, ISO-4217 codes)
 * before any upstream call; the host is fixed, so there is no SSRF surface.
 */
export const currencyTool: TravelToolDef = {
  name: "convert_currency",
  scope: SCOPE_FX,
  config: {
    title: "Convert currency",
    description:
      "Convert an amount from one ISO-4217 currency to another using live ECB reference rates. Requires the fx:read scope.",
    inputSchema: {
      amount: z.number().positive().describe("Amount in the source currency, e.g. 250"),
      from: z.string().regex(ISO_4217).describe("ISO-4217 source currency code, e.g. NZD"),
      to: z.string().regex(ISO_4217).describe("ISO-4217 target currency code, e.g. AUD"),
    },
  },
  async run(args, cfg: ToolConfig) {
    const amount = args.amount as number;
    const f = (args.from as string).toUpperCase();
    const t = (args.to as string).toUpperCase();

    const result = (converted: number, rate: number, asOf: string) =>
      jsonResult({
        amount,
        from: f,
        to: t,
        converted: Number(converted.toFixed(2)),
        rate: Number(rate.toFixed(6)),
        asOf,
        source: "ECB reference rates",
      });

    // Same currency: no upstream call needed (Frankfurter omits the self-rate).
    if (f === t) return result(amount, 1, new Date().toISOString().slice(0, 10));

    const url = `${cfg.frankfurterUrl}/latest?amount=${encodeURIComponent(amount)}&from=${f}&to=${t}`;
    let data: FrankfurterLatest;
    try {
      // redirect:"error" — the host is pinned + canonical, so any redirect is
      // unexpected and must not silently pivot egress to another host.
      const res = await cfg.fetch(url, {
        redirect: "error",
        signal: AbortSignal.timeout(UPSTREAM_TIMEOUT_MS),
      });
      if (!res.ok) throw new Error(`status ${res.status}`);
      data = (await res.json()) as FrankfurterLatest;
    } catch (err) {
      throw upstreamUnavailable("currency conversion", err);
    }

    const converted = data.rates?.[t];
    if (typeof converted !== "number") {
      throw upstreamUnavailable("currency conversion", `no rate for ${f}->${t}`);
    }
    return result(converted, converted / amount, data.date);
  },
};
