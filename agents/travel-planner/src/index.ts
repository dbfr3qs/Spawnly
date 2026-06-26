import { v4 as uuidv4 } from "uuid";
import { ClientFactory } from "@a2a-js/sdk/client";
import { postEvent, spawn, TokenClient } from "@spawnly/sdk";

// Deterministic travel orchestrator (no LLM): fans out to three CONSENT-GATED
// specialists — flight-search, hotel-search, fx-converter — each a DIFFERENT spawn
// edge, so the user gets three independent consent prompts that do NOT collapse.
// Collects the real results and assembles an itinerary.
const agentId = process.env.AGENT_ID ?? "unknown";
const registryUrl = process.env.REGISTRY_URL ?? "http://registry:8080";
const orchestratorUrl = process.env.ORCHESTRATOR_URL ?? "http://orchestrator:8080";
const sidecarUrl = process.env.SIDECAR_URL ?? "http://localhost:8089";

const tokens = new TokenClient(sidecarUrl);

// Trip parameters — overridable via env so a custom trip can be driven, with a
// concrete NZ↔AU default so the demo runs out of the box.
const origin = process.env.ORIGIN ?? "AKL";
const destination = process.env.DESTINATION ?? "SYD";
const destCity = process.env.DEST_CITY ?? "Sydney";
const destCountry = process.env.DEST_COUNTRY ?? "AU";
const departDate = process.env.DEPART_DATE ?? "2026-09-15";
const returnDate = process.env.RETURN_DATE ?? "2026-09-17";
const homeCurrency = process.env.HOME_CURRENCY ?? "NZD";
const budget = Number(process.env.BUDGET ?? 1500);

async function spawnSpecialist(agentType: string): Promise<string> {
  // The orchestrator derives userId/parentId/tenantId from our spawn token + the
  // registry, so we only send agentType.
  const r = await spawn(orchestratorUrl, tokens, agentType);
  if (!r.ok) throw new Error(`spawn ${agentType} failed: ${r.status} ${r.body ?? ""}`);
  if (!r.workloadName) throw new Error(`spawn ${agentType}: no id in response`);
  return r.workloadName;
}

async function waitReady(childId: string): Promise<void> {
  const url = `http://${childId}-svc:8080/.well-known/agent.json`;
  const deadline = Date.now() + 60_000;
  while (Date.now() < deadline) {
    try {
      if ((await fetch(url)).ok) return;
    } catch {
      /* not ready */
    }
    await new Promise((r) => setTimeout(r, 2_000));
  }
  throw new Error(`timed out waiting for ${childId}`);
}

// Drive a specialist's one tool over A2A. This is when the child mints its scoped
// token — and, because the edge is consent-gated, when the user's consent prompt
// for (user, travel-planner, <childType>) appears. Blocks until consent resolves.
async function callSpecialist(childId: string, params: Record<string, unknown>): Promise<unknown> {
  const client = await new ClientFactory().createFromUrl(`http://${childId}-svc:8080`);
  const result = await client.sendMessage({
    message: {
      kind: "message",
      messageId: uuidv4(),
      role: "user",
      parts: [{ kind: "text", text: "run" }],
      metadata: { params }, // the specialist reads its tool args from metadata.params
    },
  });
  let text = "";
  if ("kind" in result && result.kind === "message") {
    for (const p of result.parts) if (p.kind === "text") text += p.text;
  } else if ("kind" in result && result.kind === "task") {
    const t = result as { history?: Array<{ role: string; parts: Array<{ kind: string; text?: string }> }> };
    const last = (t.history ?? []).filter((m) => m.role === "agent").pop();
    text = (last?.parts ?? []).filter((p) => p.kind === "text").map((p) => p.text).join("");
  }
  try {
    return JSON.parse(text);
  } catch {
    return text;
  }
}

async function killSpecialist(childId: string): Promise<void> {
  await fetch(`${orchestratorUrl}/v1/agents/${childId}`, { method: "DELETE" }).catch(() => {});
}

// One consent-gated specialist task: spawn → wait → call → kill.
async function runSpecialist(agentType: string, params: Record<string, unknown>): Promise<unknown> {
  const childId = await spawnSpecialist(agentType);
  await postEvent(registryUrl, agentId, "specialist_spawned", { agentType, childId });
  try {
    await waitReady(childId);
    return await callSpecialist(childId, params);
  } finally {
    await killSpecialist(childId);
  }
}

async function main(): Promise<void> {
  await postEvent(registryUrl, agentId, "parent_started", {
    agentId,
    trip: { origin, destination, departDate, returnDate },
  });

  // Fan out to all three specialists CONCURRENTLY — three distinct consent-gated
  // edges, so three independent prompts appear together (they don't collapse).
  // allSettled (not all): a single specialist failure or a DENIED consent must not
  // abort the others, and every branch's finally→killSpecialist must run before we
  // proceed (no orphaned child pods).
  const [flightsR, hotelsR, fxR] = await Promise.allSettled([
    runSpecialist("flight-search", { origin, destination, departureDate: departDate, adults: 1, cabin: "economy" }),
    runSpecialist("hotel-search", {
      cityName: destCity,
      countryCode: destCountry,
      checkIn: departDate,
      checkOut: returnDate,
      adults: 1,
      currency: homeCurrency,
    }),
    runSpecialist("fx-converter", { amount: budget, from: homeCurrency, to: "AUD" }),
  ]);
  for (const r of [flightsR, hotelsR, fxR]) {
    if (r.status === "rejected") {
      console.error("[travel-planner] a specialist failed:", r.reason instanceof Error ? r.reason.message : r.reason);
    }
  }

  // Assemble a structured itinerary from whatever real results came back (a denied
  // or failed capability simply shows as null — the rest of the trip still plans).
  const f = (flightsR.status === "fulfilled" ? flightsR.value : {}) as {
    offers?: Array<{ carrier: string; price: number; currency: string }>;
  };
  const h = (hotelsR.status === "fulfilled" ? hotelsR.value : {}) as {
    hotels?: Array<{ name: string; price?: number; currency?: string; stars?: number }>;
  };
  const x = (fxR.status === "fulfilled" ? fxR.value : {}) as { converted?: number; rate?: number };
  const itinerary = {
    trip: { from: origin, to: destination, depart: departDate, return: returnDate },
    flight: f.offers?.[0]
      ? { airline: f.offers[0].carrier, price: f.offers[0].price, currency: f.offers[0].currency }
      : null,
    hotel: h.hotels?.[0]
      ? { name: h.hotels[0].name, stars: h.hotels[0].stars, price: h.hotels[0].price, currency: h.hotels[0].currency }
      : null,
    budget: x.converted
      ? { amount: budget, fromCurrency: homeCurrency, toCurrency: "AUD", converted: x.converted, rate: x.rate }
      : null,
  };

  await postEvent(registryUrl, agentId, "parent_completed", { itinerary });
  console.log("[travel-planner] itinerary:", JSON.stringify(itinerary, null, 2));
}

main().catch(async (err) => {
  console.error("[travel-planner] fatal:", err);
  await postEvent(registryUrl, agentId, "agent_error", { error: err instanceof Error ? err.message : String(err) });
  process.exit(1);
});
