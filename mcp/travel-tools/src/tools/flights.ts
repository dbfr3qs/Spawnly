import { z } from "zod";
import type { ToolConfig } from "../config.js";
import { SCOPE_FLIGHTS } from "../scopes.js";
import { searchFlights } from "../flights/index.js";
import { jsonResult, type TravelToolDef } from "./types.js";

const IATA = /^[A-Za-z]{3}$/;
const DATE = /^\d{4}-\d{2}-\d{2}$/;

/**
 * search_flights — real flight offers aggregated across one or more providers
 * (Duffel today), behind a provider-agnostic contract. Gated on flights:read.
 * Softly prefers NZ/AU-based carriers. If no provider is configured it returns a
 * clear, non-erroring "not configured" result so the rest of the demo still runs.
 */
export const flightsTool: TravelToolDef = {
  name: "search_flights",
  scope: SCOPE_FLIGHTS,
  config: {
    title: "Search flights",
    description:
      "Search real one-way flight offers between two airports, preferring New Zealand / Australia–based carriers. Requires the flights:read scope.",
    inputSchema: {
      origin: z.string().regex(IATA).describe("Origin airport IATA code, e.g. AKL"),
      destination: z.string().regex(IATA).describe("Destination airport IATA code, e.g. SYD"),
      departureDate: z.string().regex(DATE).describe("Departure date, YYYY-MM-DD"),
      adults: z.number().int().min(1).max(9).default(1).describe("Number of adult passengers"),
      cabin: z
        .enum(["economy", "premium_economy", "business", "first"])
        .default("economy")
        .describe("Cabin class"),
    },
  },
  async run(args, cfg: ToolConfig) {
    if (!cfg.duffel.apiKey) {
      return jsonResult({
        configured: false,
        message: "Flight search is not configured on this server.",
      });
    }

    const result = await searchFlights(
      {
        origin: (args.origin as string).toUpperCase(),
        destination: (args.destination as string).toUpperCase(),
        departureDate: args.departureDate as string,
        adults: (args.adults as number) ?? 1,
        cabin: (args.cabin as "economy" | "premium_economy" | "business" | "first") ?? "economy",
      },
      cfg,
    );

    return jsonResult({
      configured: true,
      auNzPreferred: result.auNzOnly,
      note: result.auNzOnly
        ? "Showing NZ/AU-based carriers."
        : "No NZ/AU-based carriers in the available results; showing all.",
      count: result.offers.length,
      offers: result.offers,
    });
  },
};
