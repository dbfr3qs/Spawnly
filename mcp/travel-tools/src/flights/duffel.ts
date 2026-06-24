import type { ToolConfig } from "../config.js";
import { upstreamUnavailable } from "../upstream.js";
import type { FlightOffer, FlightQuery } from "./types.js";

const DUFFEL_TIMEOUT_MS = 30000; // a live offer search fans out across airlines

// Minimal shape of the Duffel Air offer-request response we consume.
interface DuffelResponse {
  data?: {
    offers?: Array<{
      total_amount: string;
      total_currency: string;
      owner?: { name?: string; iata_code?: string };
      slices?: Array<{
        segments?: Array<{
          operating_carrier?: { name?: string; iata_code?: string };
          operating_carrier_flight_number?: string;
          origin?: { iata_code?: string };
          destination?: { iata_code?: string };
          departing_at?: string;
          arriving_at?: string;
        }>;
      }>;
    }>;
  };
}

/**
 * Duffel Air adapter. Calls /air/offer_requests and normalizes the result into
 * canonical FlightOffers. The Duffel API key never leaves this server, and the
 * caller never learns the provider was Duffel (opaque ids, generic errors).
 * Returns [] when no key is configured so the façade simply skips this provider.
 */
export function duffelProvider(cfg: ToolConfig) {
  return async (query: FlightQuery): Promise<FlightOffer[]> => {
    if (!cfg.duffel.apiKey) return [];

    const body = {
      data: {
        slices: [
          { origin: query.origin, destination: query.destination, departure_date: query.departureDate },
        ],
        passengers: Array.from({ length: query.adults }, () => ({ type: "adult" })),
        cabin_class: query.cabin,
      },
    };

    let json: DuffelResponse;
    try {
      const res = await cfg.fetch(`${cfg.duffel.url}/air/offer_requests?return_offers=true`, {
        method: "POST",
        redirect: "error",
        signal: AbortSignal.timeout(DUFFEL_TIMEOUT_MS),
        headers: {
          Authorization: `Bearer ${cfg.duffel.apiKey}`,
          "Duffel-Version": cfg.duffel.version,
          Accept: "application/json",
          "Content-Type": "application/json",
        },
        body: JSON.stringify(body),
      });
      if (!res.ok) throw new Error(`status ${res.status}`);
      json = (await res.json()) as DuffelResponse;
    } catch (err) {
      // Never leak the provider/url/body/key to the agent.
      throw upstreamUnavailable("flight search", err);
    }

    const offers = json.data?.offers ?? [];
    return offers.map((o, i): FlightOffer => {
      const segments = (o.slices ?? []).flatMap((s) => s.segments ?? []);
      return {
        id: `fo-${i}`,
        carrier: o.owner?.name ?? "Unknown",
        carrierCode: o.owner?.iata_code ?? "",
        price: Number.parseFloat(o.total_amount),
        currency: o.total_currency,
        stops: Math.max(0, segments.length - (o.slices?.length ?? 1)),
        segments: segments.map((seg) => ({
          carrier: seg.operating_carrier?.name ?? "Unknown",
          carrierCode: seg.operating_carrier?.iata_code ?? "",
          flightNumber: seg.operating_carrier_flight_number ?? "",
          from: seg.origin?.iata_code ?? "",
          to: seg.destination?.iata_code ?? "",
          departAt: seg.departing_at ?? "",
          arriveAt: seg.arriving_at ?? "",
        })),
      };
    });
  };
}
