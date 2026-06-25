// Canonical, provider-agnostic flight schema. Every provider adapter normalizes
// INTO this; the tool only ever returns this shape, so no provider identity
// (Duffel/Kiwi/…), upstream ids, or upstream error detail leak to the agent.

export interface FlightQuery {
  origin: string; // IATA, e.g. AKL
  destination: string; // IATA, e.g. SYD
  departureDate: string; // YYYY-MM-DD
  adults: number;
  cabin: "economy" | "premium_economy" | "business" | "first";
}

export interface FlightSegment {
  carrier: string; // operating airline name
  carrierCode: string; // IATA, e.g. NZ
  flightNumber: string;
  from: string;
  to: string;
  departAt: string;
  arriveAt: string;
}

export interface FlightOffer {
  id: string; // opaque, provider-free
  carrier: string; // marketing/owner airline name
  carrierCode: string; // IATA
  price: number;
  currency: string;
  stops: number;
  segments: FlightSegment[];
}

/** A provider adapter: turns a normalized query into canonical offers. */
export type FlightProvider = (query: FlightQuery) => Promise<FlightOffer[]>;
