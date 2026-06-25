import { z } from "zod";
import type { ToolConfig } from "../config.js";
import { SCOPE_HOTELS } from "../scopes.js";
import { searchHotels } from "../hotels/liteapi.js";
import { jsonResult, type TravelToolDef } from "./types.js";

const DATE = /^\d{4}-\d{2}-\d{2}$/;
const COUNTRY = /^[A-Za-z]{2}$/;
const ISO_4217 = /^[A-Za-z]{3}$/;

/**
 * search_hotels — real hotel availability + nightly rates for a city. Gated on
 * hotels:read. If the hotel provider isn't configured (no LITEAPI_KEY), returns a
 * clear non-erroring "not configured" result so flights + currency still demo.
 */
export const hotelsTool: TravelToolDef = {
  name: "search_hotels",
  scope: SCOPE_HOTELS,
  config: {
    title: "Search hotels",
    description:
      "Search real hotels in a city with stay dates and get nightly rates. Requires the hotels:read scope.",
    inputSchema: {
      cityName: z.string().min(1).max(80).describe("City name, e.g. Auckland"),
      countryCode: z.string().regex(COUNTRY).describe("ISO-3166 alpha-2 country code, e.g. NZ"),
      checkIn: z.string().regex(DATE).describe("Check-in date, YYYY-MM-DD"),
      checkOut: z.string().regex(DATE).describe("Check-out date, YYYY-MM-DD"),
      adults: z.number().int().min(1).max(9).default(2).describe("Number of adult guests"),
      currency: z.string().regex(ISO_4217).default("NZD").describe("ISO-4217 currency for rates"),
    },
  },
  async run(args, cfg: ToolConfig) {
    if (!cfg.liteapi.apiKey) {
      return jsonResult({
        configured: false,
        message: "Hotel search is not configured on this server.",
      });
    }

    const hotels = await searchHotels(
      {
        cityName: args.cityName as string,
        countryCode: (args.countryCode as string).toUpperCase(),
        checkIn: args.checkIn as string,
        checkOut: args.checkOut as string,
        adults: (args.adults as number) ?? 2,
        currency: (args.currency as string).toUpperCase(),
      },
      cfg,
    );

    return jsonResult({ configured: true, count: hotels.length, hotels });
  },
};
