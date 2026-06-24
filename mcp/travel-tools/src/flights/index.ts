import type { ToolConfig } from "../config.js";
import { duffelProvider } from "./duffel.js";
import type { FlightOffer, FlightProvider, FlightQuery } from "./types.js";

// IATA codes of New Zealand / Australia–based carriers. Used as a SOFT filter:
// in live mode these surface the AU/NZ options the demo cares about, but in a
// provider's test/sandbox mode (synthetic carriers) nothing would match, so the
// façade falls back to all offers rather than returning an empty list.
const AU_NZ_CARRIERS = new Set(["NZ", "QF", "JQ", "VA", "TT", "ZL", "QQ", "NF"]);

const MAX_RESULTS = 10;

export interface FlightSearchResult {
  offers: FlightOffer[];
  /** Whether results were narrowed to AU/NZ-based carriers (false ⇒ none matched, showing all). */
  auNzOnly: boolean;
  /** Count of upstream sources that contributed (provider-agnostic — no names). */
  sources: number;
}

/** The set of providers that are configured (have a key). Kiwi/Tequila slots in here later. */
function providers(cfg: ToolConfig): FlightProvider[] {
  const list: FlightProvider[] = [];
  if (cfg.duffel.apiKey) list.push(duffelProvider(cfg));
  return list;
}

function dedupeKey(o: FlightOffer): string {
  const first = o.segments[0];
  const last = o.segments[o.segments.length - 1];
  // Identity = the physical itinerary, NOT price: the same flight from two
  // providers often differs by taxes/markup, and we still want to collapse it.
  return [o.carrierCode, first?.flightNumber, first?.departAt, last?.arriveAt].join("|");
}

/**
 * Fan out the query to every configured provider concurrently, normalize +
 * dedupe + rank by price, then apply the AU/NZ soft-filter. A provider that
 * errors is skipped (best-effort aggregation) rather than failing the whole
 * search. Output is fully provider-agnostic.
 */
export async function searchFlights(query: FlightQuery, cfg: ToolConfig): Promise<FlightSearchResult> {
  const provs = providers(cfg);
  const settled = await Promise.allSettled(provs.map((p) => p(query)));

  const all: FlightOffer[] = [];
  let sources = 0;
  for (const r of settled) {
    if (r.status === "fulfilled") {
      if (r.value.length > 0) sources++;
      all.push(...r.value);
    }
  }

  // Sort cheapest-first, THEN dedupe — so when the same itinerary comes from two
  // providers at different prices, the cheapest one survives.
  all.sort((a, b) => a.price - b.price);
  const seen = new Set<string>();
  const unique = all.filter((o) => {
    const k = dedupeKey(o);
    if (seen.has(k)) return false;
    seen.add(k);
    return true;
  });

  const auNz = unique.filter((o) => AU_NZ_CARRIERS.has(o.carrierCode));
  const auNzOnly = auNz.length > 0;
  // Re-issue ids on the FINAL merged list so they're unique and stable within
  // this result (each adapter's positional ids would collide after the merge).
  // Ids are display/reference handles for this response only — not bookable.
  const chosen = (auNzOnly ? auNz : unique)
    .slice(0, MAX_RESULTS)
    .map((o, i) => ({ ...o, id: `flight-${i + 1}` }));

  return { offers: chosen, auNzOnly, sources };
}
