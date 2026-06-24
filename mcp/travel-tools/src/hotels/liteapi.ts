import type { ToolConfig } from "../config.js";
import { upstreamUnavailable } from "../upstream.js";

const LITEAPI_TIMEOUT_MS = 20000;
const MAX_HOTELS = 10;

export interface HotelQuery {
  cityName: string;
  countryCode: string; // ISO-3166 alpha-2, e.g. NZ
  checkIn: string; // YYYY-MM-DD
  checkOut: string; // YYYY-MM-DD
  adults: number;
  currency: string; // ISO-4217
}

export interface HotelResult {
  id: string; // opaque, provider-free
  name: string;
  stars: number;
  rating: number;
  address: string;
  price?: number; // cheapest total for the stay, when available
  currency?: string;
}

interface LiteHotel {
  id: string;
  name: string;
  stars?: number;
  rating?: number;
  address?: string;
}
interface LiteRates {
  data?: Array<{
    hotelId: string;
    roomTypes?: Array<{
      rates?: Array<{ retailRate?: { total?: Array<{ amount?: number; currency?: string }> } }>;
    }>;
  }>;
}

// Cheapest stay total across all room types / rates for one hotel.
function cheapest(entry: NonNullable<LiteRates["data"]>[number]): { amount: number; currency: string } | undefined {
  let best: { amount: number; currency: string } | undefined;
  for (const rt of entry.roomTypes ?? []) {
    for (const rate of rt.rates ?? []) {
      const total = rate.retailRate?.total?.[0];
      if (total && typeof total.amount === "number" && typeof total.currency === "string") {
        if (!best || total.amount < best.amount) best = { amount: total.amount, currency: total.currency };
      }
    }
  }
  return best;
}

/**
 * LiteAPI hotel adapter: list hotels in the city, then fetch rates for them and
 * join by hotel id. The LiteAPI key never leaves this server and the caller never
 * learns the provider (opaque ids, generic errors). Returns canonical results
 * sorted cheapest-first; hotels without a rate are still listed (price omitted).
 */
export async function searchHotels(query: HotelQuery, cfg: ToolConfig): Promise<HotelResult[]> {
  const key = cfg.liteapi.apiKey;
  if (!key) return [];
  const headers = { "X-API-Key": key, Accept: "application/json", "Content-Type": "application/json" };

  let hotels: LiteHotel[];
  try {
    const url =
      `${cfg.liteapi.url}/data/hotels?countryCode=${encodeURIComponent(query.countryCode)}` +
      `&cityName=${encodeURIComponent(query.cityName)}&limit=${MAX_HOTELS}`;
    const res = await cfg.fetch(url, {
      redirect: "error",
      signal: AbortSignal.timeout(LITEAPI_TIMEOUT_MS),
      headers,
    });
    if (!res.ok) throw new Error(`hotels status ${res.status}`);
    hotels = ((await res.json()) as { data?: LiteHotel[] }).data ?? [];
  } catch (err) {
    throw upstreamUnavailable("hotel search", err);
  }
  if (hotels.length === 0) return [];

  // Fetch rates for the listed hotels (best-effort — list still returns if rates fail).
  const priceById = new Map<string, { amount: number; currency: string }>();
  try {
    const res = await cfg.fetch(`${cfg.liteapi.url}/hotels/rates`, {
      method: "POST",
      redirect: "error",
      signal: AbortSignal.timeout(LITEAPI_TIMEOUT_MS),
      headers,
      body: JSON.stringify({
        hotelIds: hotels.map((h) => h.id),
        checkin: query.checkIn,
        checkout: query.checkOut,
        currency: query.currency,
        guestNationality: query.countryCode,
        occupancies: [{ adults: query.adults }],
      }),
    });
    if (res.ok) {
      for (const entry of ((await res.json()) as LiteRates).data ?? []) {
        const c = cheapest(entry);
        if (c) priceById.set(entry.hotelId, c);
      }
    }
  } catch {
    // Rates are best-effort; fall through to listing hotels without prices.
  }

  const results: HotelResult[] = hotels.map((h, i) => {
    const price = priceById.get(h.id);
    return {
      id: `ho-${i}`,
      name: h.name,
      stars: h.stars ?? 0,
      rating: h.rating ?? 0,
      address: h.address ?? "",
      price: price?.amount,
      currency: price?.currency,
    };
  });

  // Priced hotels first (cheapest), then the rest.
  results.sort((a, b) => {
    if (a.price === undefined) return b.price === undefined ? 0 : 1;
    if (b.price === undefined) return -1;
    return a.price - b.price;
  });
  return results;
}
