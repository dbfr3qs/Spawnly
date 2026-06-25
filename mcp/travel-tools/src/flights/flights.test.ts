import { describe, it, expect } from "vitest";
import type { ToolConfig } from "../config.js";
import { duffelProvider } from "./duffel.js";
import { searchFlights } from "./index.js";

function mockFetch(map: (url: string) => unknown, status = 200): typeof fetch {
  return (async (url: unknown) =>
    ({ ok: status < 400, status, json: async () => map(String(url)) }) as Response) as typeof fetch;
}

function cfg(fetchImpl: typeof fetch, duffelKey: string | undefined = "test"): ToolConfig {
  return {
    frankfurterUrl: "http://fr",
    duffel: { url: "http://duffel", apiKey: duffelKey, version: "v2" },
    liteapi: { url: "http://lite", apiKey: undefined },
    fetch: fetchImpl,
  };
}

const duffelResponse = (owners: Array<{ name: string; iata: string; amount: string }>) => ({
  data: {
    offers: owners.map((o) => ({
      total_amount: o.amount,
      total_currency: "AUD",
      owner: { name: o.name, iata_code: o.iata },
      slices: [
        {
          segments: [
            {
              operating_carrier: { name: o.name, iata_code: o.iata },
              operating_carrier_flight_number: "100",
              origin: { iata_code: "AKL" },
              destination: { iata_code: "SYD" },
              departing_at: "2026-09-15T10:00:00",
              arriving_at: "2026-09-15T11:30:00",
            },
          ],
        },
      ],
    })),
  },
});

const QUERY = { origin: "AKL", destination: "SYD", departureDate: "2026-09-15", adults: 1, cabin: "economy" } as const;

describe("duffel adapter", () => {
  it("normalizes offers into the canonical shape", async () => {
    const c = cfg(mockFetch(() => duffelResponse([{ name: "Qantas", iata: "QF", amount: "199.00" }])));
    const offers = await duffelProvider(c)(QUERY);
    expect(offers).toHaveLength(1);
    expect(offers[0]).toMatchObject({ carrierCode: "QF", price: 199, currency: "AUD", stops: 0 });
    expect(offers[0].segments[0]).toMatchObject({ from: "AKL", to: "SYD", carrierCode: "QF" });
  });

  it("returns [] when no Duffel key is configured", async () => {
    const c = cfg(mockFetch(() => ({})), undefined);
    expect(await duffelProvider(c)(QUERY)).toEqual([]);
  });

  it("throws a generic error (no upstream status/url leaked) on an upstream failure", async () => {
    const c = cfg(mockFetch(() => ({}), 500));
    await expect(duffelProvider(c)(QUERY)).rejects.toThrow(/unavailable right now/);
    await expect(duffelProvider(c)(QUERY)).rejects.not.toThrow(/500|duffel|http/i);
  });
});

describe("flight façade", () => {
  it("soft-filters to AU/NZ carriers when present, cheapest-first", async () => {
    const c = cfg(
      mockFetch(() =>
        duffelResponse([
          { name: "American", iata: "AA", amount: "99.00" },
          { name: "Qantas", iata: "QF", amount: "199.00" },
          { name: "Air New Zealand", iata: "NZ", amount: "150.00" },
        ]),
      ),
    );
    const r = await searchFlights(QUERY, c);
    expect(r.auNzOnly).toBe(true);
    expect(r.offers.map((o) => o.carrierCode)).toEqual(["NZ", "QF"]); // AA dropped, NZ cheaper
  });

  it("falls back to all offers when no AU/NZ carrier (sandbox/test-mode behavior)", async () => {
    const c = cfg(mockFetch(() => duffelResponse([{ name: "American", iata: "AA", amount: "99.00" }])));
    const r = await searchFlights(QUERY, c);
    expect(r.auNzOnly).toBe(false);
    expect(r.offers.map((o) => o.carrierCode)).toEqual(["AA"]);
  });

  it("dedupes the same itinerary, keeping the cheapest, and re-issues stable ids", async () => {
    // Same QF flight twice at different prices (e.g. two providers / markups).
    const c = cfg(
      mockFetch(() =>
        duffelResponse([
          { name: "Qantas", iata: "QF", amount: "250.00" },
          { name: "Qantas", iata: "QF", amount: "199.00" },
        ]),
      ),
    );
    const r = await searchFlights(QUERY, c);
    expect(r.offers).toHaveLength(1);
    expect(r.offers[0].price).toBe(199); // cheapest survives
    expect(r.offers[0].id).toBe("flight-1"); // id re-issued on the merged list
  });
});
