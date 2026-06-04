import type { ToolDef, FlueSession } from "@flue/runtime";
import { configureProvider } from "@flue/runtime/app";
import {
  createFlueContext,
  InMemorySessionStore,
  resolveModel,
} from "@flue/runtime/internal";
import { local } from "@flue/runtime/node";
import { postEvent, instrumentFlue, promptTimeoutSignal } from "@spawnly/sdk";

export const triggers = { webhook: true };

const agentId        = process.env.AGENT_ID        ?? "unknown";
const registryUrl    = process.env.REGISTRY_URL    ?? "http://registry:8080";
const aiProvider     = process.env.AI_PROVIDER     ?? "openai";
const aiApiKey       = process.env.AI_API_KEY      ?? "";
const aiModel        = process.env.AI_MODEL        ?? "openai/gpt-4o";
const promptTimeoutMs = Number(process.env.PROMPT_TIMEOUT_MS ?? 120000);

// Open-Meteo — free, no API key. Geocoding resolves a place name to lat/lon;
// the forecast endpoint returns current conditions for those coordinates.
// wttr.in is a free, no-key fallback geocoder for networks where Open-Meteo's
// geocoding host is unreachable (its forecast host stays the weather source).
const geocodeUrl  = process.env.OPEN_METEO_GEOCODE_URL  ?? "https://geocoding-api.open-meteo.com/v1/search";
const forecastUrl = process.env.OPEN_METEO_FORECAST_URL ?? "https://api.open-meteo.com/v1/forecast";
const wttrUrl     = process.env.WTTR_URL                ?? "https://wttr.in";

configureProvider(aiProvider, { apiKey: aiApiKey });

console.log(`[weather-monitor] chat agent ${agentId} using ${aiProvider} (${aiModel})`);

// --- Heartbeat ---------------------------------------------------------------
// Long-lived liveness signal: the HTTP server stays up until the pod is killed,
// and this loop posts a periodic event so the dashboard shows the agent running.
async function beat(): Promise<void> {
  await postEvent(registryUrl, agentId, "heartbeat", {
    status: "running",
    timestamp: new Date().toISOString(),
  });
}
void beat();
setInterval(() => void beat(), 30_000);

// WMO weather-code → human description for the codes Open-Meteo returns in
// `weather_code`; anything unmapped falls back to the raw code.
const WEATHER_CODES: Record<number, string> = {
  0: "clear sky",
  1: "mainly clear", 2: "partly cloudy", 3: "overcast",
  45: "fog", 48: "depositing rime fog",
  51: "light drizzle", 53: "moderate drizzle", 55: "dense drizzle",
  56: "light freezing drizzle", 57: "dense freezing drizzle",
  61: "slight rain", 63: "moderate rain", 65: "heavy rain",
  66: "light freezing rain", 67: "heavy freezing rain",
  71: "slight snow", 73: "moderate snow", 75: "heavy snow", 77: "snow grains",
  80: "slight rain showers", 81: "moderate rain showers", 82: "violent rain showers",
  85: "slight snow showers", 86: "heavy snow showers",
  95: "thunderstorm", 96: "thunderstorm with slight hail", 99: "thunderstorm with heavy hail",
};

interface GeoResult {
  latitude: number;
  longitude: number;
  label: string;
}

// Resolve a place name to coordinates. Primary: Open-Meteo geocoding. Fallback:
// wttr.in's `nearest_area`, for networks where the Open-Meteo geocoding host is
// unreachable. Returns null if neither resolves the name.
async function geocode(location: string): Promise<GeoResult | null> {
  try {
    const res = await fetch(
      `${geocodeUrl}?name=${encodeURIComponent(location)}&count=1&language=en&format=json`,
      { signal: AbortSignal.timeout(8000) },
    );
    if (res.ok) {
      const geo = (await res.json()) as {
        results?: Array<{ latitude: number; longitude: number; name: string; country?: string; admin1?: string }>;
      };
      const p = geo.results?.[0];
      if (p) {
        return {
          latitude: p.latitude,
          longitude: p.longitude,
          label: [p.name, p.admin1, p.country].filter(Boolean).join(", "),
        };
      }
    }
  } catch {
    // Fall through to the wttr.in fallback below.
  }

  try {
    const res = await fetch(`${wttrUrl}/${encodeURIComponent(location)}?format=j1`, {
      signal: AbortSignal.timeout(8000),
    });
    if (res.ok) {
      const data = (await res.json()) as {
        nearest_area?: Array<{
          latitude?: string; longitude?: string;
          areaName?: Array<{ value: string }>;
          region?: Array<{ value: string }>;
          country?: Array<{ value: string }>;
        }>;
      };
      const a = data.nearest_area?.[0];
      if (a?.latitude && a?.longitude) {
        return {
          latitude: Number(a.latitude),
          longitude: Number(a.longitude),
          label: [a.areaName?.[0]?.value, a.region?.[0]?.value, a.country?.[0]?.value]
            .filter(Boolean)
            .join(", "),
        };
      }
    }
  } catch {
    // Both geocoders failed.
  }

  return null;
}

// Tool: resolve a place name and report its current weather (no auth required).
const getWeather: ToolDef = {
  name: "get_weather",
  description:
    "Look up the current weather for a location by name (city, optionally with region/country). " +
    "Returns temperature, apparent temperature, humidity, wind speed and a short conditions description.",
  parameters: {
    type: "object",
    properties: {
      location: {
        type: "string",
        description: "Place name to look up, e.g. 'Paris', 'Tokyo', 'Austin, Texas'.",
      },
    },
    required: ["location"],
  },
  execute: async (args: Record<string, unknown>) => {
    const location = String(args.location ?? "").trim();
    if (!location) {
      return JSON.stringify({ error: "no location provided" });
    }

    // Step 1: geocode the place name to coordinates (Open-Meteo, wttr.in fallback).
    const place = await geocode(location);
    if (!place) {
      return JSON.stringify({ error: `could not find a location named "${location}"` });
    }

    // Step 2: fetch current conditions for those coordinates from Open-Meteo.
    const params = new URLSearchParams({
      latitude: String(place.latitude),
      longitude: String(place.longitude),
      current: "temperature_2m,apparent_temperature,relative_humidity_2m,wind_speed_10m,weather_code",
    });
    const wxRes = await fetch(`${forecastUrl}?${params.toString()}`, { signal: AbortSignal.timeout(8000) });
    if (!wxRes.ok) {
      return JSON.stringify({ error: `forecast failed: ${wxRes.status}` });
    }
    const wx = (await wxRes.json()) as {
      current?: Record<string, number>;
      current_units?: Record<string, string>;
    };
    const cur = wx.current ?? {};
    const units = wx.current_units ?? {};
    const code = Number(cur.weather_code);

    return JSON.stringify({
      location: place.label,
      conditions: WEATHER_CODES[code] ?? `weather code ${code}`,
      temperature: cur.temperature_2m,
      apparentTemperature: cur.apparent_temperature,
      humidityPercent: cur.relative_humidity_2m,
      windSpeed: cur.wind_speed_10m,
      units: {
        temperature: units.temperature_2m ?? "°C",
        windSpeed: units.wind_speed_10m ?? "km/h",
      },
    });
  },
};

const SYSTEM_PROMPT =
  "You are Weather Monitor, a friendly weather assistant. When the user asks about the weather " +
  "for any place, call the get_weather tool with that location and answer using the values it returns. " +
  "Keep replies concise and conversational. If the tool reports it could not find a location, say so and " +
  "ask the user to clarify. For non-weather chit-chat, reply briefly without calling the tool.";

// One Flue session per chat sessionId, so follow-up questions keep context.
// In-memory only — lost if the pod restarts.
const sessions = new Map<string, Promise<FlueSession>>();

function getSession(sessionId: string): Promise<FlueSession> {
  const existing = sessions.get(sessionId);
  if (existing) return existing;

  const created = (async () => {
    const ctx = createFlueContext({
      id: agentId,
      runId: crypto.randomUUID(),
      payload: {},
      env: process.env as Record<string, string>,
      agentConfig: {
        systemPrompt: SYSTEM_PROMPT,
        skills: {},
        roles: {},
        model: undefined,
        resolveModel,
      },
      createDefaultEnv: async () => local().createSessionEnv({ id: agentId, cwd: process.cwd() }),
      defaultStore: new InMemorySessionStore(),
    });
    instrumentFlue(ctx, registryUrl, agentId);
    const harness = await ctx.init({ model: aiModel, tools: [getWeather], sandbox: local() });
    return harness.session();
  })();

  sessions.set(sessionId, created);
  return created;
}

interface ChatPayload {
  message: string;
  sessionId?: string;
}

export default async function ({ payload }: { payload: ChatPayload }) {
  const sessionId = payload.sessionId ?? "default";
  const message = (payload.message ?? "").trim();

  if (!message) {
    return {
      response: "Ask me about the weather anywhere — e.g. \"What's the weather in Tokyo?\"",
      agentId,
      timestamp: new Date().toISOString(),
    };
  }

  try {
    const session = await getSession(sessionId);
    const result = await session.prompt(message, { signal: promptTimeoutSignal(promptTimeoutMs) });
    return { response: result.text, agentId, timestamp: new Date().toISOString() };
  } catch (err) {
    // Drop the session so the next message starts clean rather than reusing a
    // potentially wedged one.
    sessions.delete(sessionId);
    const detail = err instanceof Error ? err.message : String(err);
    await postEvent(registryUrl, agentId, "agent_error", { error: detail });
    return {
      response: `Sorry — something went wrong handling that: ${detail}`,
      agentId,
      timestamp: new Date().toISOString(),
    };
  }
}
