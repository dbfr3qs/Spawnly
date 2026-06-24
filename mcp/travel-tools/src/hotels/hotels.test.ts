import { describe, it, expect } from "vitest";
import type { ToolConfig } from "../config.js";
import { searchHotels } from "./liteapi.js";

interface MockRes {
  status?: number;
  body?: unknown;
}
function mockFetch(map: (url: string) => MockRes): typeof fetch {
  return (async (url: unknown) => {
    const r = map(String(url));
    const status = r.status ?? 200;
    return { ok: status < 400, status, json: async () => r.body ?? {} } as Response;
  }) as typeof fetch;
}

function cfg(fetchImpl: typeof fetch, liteapiKey: string | undefined = "k"): ToolConfig {
  return {
    frankfurterUrl: "http://fr",
    duffel: { url: "http://d", apiKey: undefined, version: "v2" },
    liteapi: { url: "http://lite", apiKey: liteapiKey },
    fetch: fetchImpl,
  };
}

const QUERY = {
  cityName: "Auckland",
  countryCode: "NZ",
  checkIn: "2026-09-15",
  checkOut: "2026-09-17",
  adults: 2,
  currency: "NZD",
} as const;

const rate = (amount: number) => ({ roomTypes: [{ rates: [{ retailRate: { total: [{ amount, currency: "NZD" }] } }] }] });

describe("liteapi hotel adapter", () => {
  it("joins hotels with rates and sorts cheapest-first", async () => {
    const f = mockFetch((url) => {
      if (url.includes("/data/hotels"))
        return {
          body: {
            data: [
              { id: "h1", name: "Cordis", stars: 5, rating: 9, address: "A" },
              { id: "h2", name: "Ibis", stars: 3, rating: 7, address: "B" },
            ],
          },
        };
      if (url.includes("/hotels/rates"))
        return { body: { data: [{ hotelId: "h1", ...rate(400) }, { hotelId: "h2", ...rate(150) }] } };
      return {};
    });
    const res = await searchHotels(QUERY, cfg(f));
    expect(res.map((h) => h.name)).toEqual(["Ibis", "Cordis"]); // cheaper first
    expect(res[0]).toMatchObject({ price: 150, currency: "NZD", stars: 3 });
    expect(res[0].id).toBe("ho-1"); // opaque id, not the LiteAPI hotel id
  });

  it("lists hotels even when the rates call fails (price omitted)", async () => {
    const f = mockFetch((url) => {
      if (url.includes("/data/hotels"))
        return { body: { data: [{ id: "h1", name: "Cordis", stars: 5, rating: 9, address: "A" }] } };
      if (url.includes("/hotels/rates")) return { status: 500 };
      return {};
    });
    const res = await searchHotels(QUERY, cfg(f));
    expect(res).toHaveLength(1);
    expect(res[0].price).toBeUndefined();
  });

  it("returns [] when no LiteAPI key is configured", async () => {
    const res = await searchHotels(QUERY, cfg(mockFetch(() => ({})), undefined));
    expect(res).toEqual([]);
  });
});
