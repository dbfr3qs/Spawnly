// Package spawnly is the Go SDK for the Spawnly agent platform.
//
// It is a per-language adapter over the platform's NEUTRAL HTTP contract and
// nothing more. It depends only on the Go standard library and never imports
// any platform package (e.g. github.com/spawnly/poc) — dependencies
// point one way, from this adapter toward the neutral contract, never the
// reverse. The few wire shapes it needs (the token response and the event
// envelope) are replicated locally rather than imported.
//
// # The contract
//
// Each agent pod runs a sidecar listening on http://localhost:8089 that exposes:
//
//	GET /token?scope=...&audience=...&subject_token=...
//	  -> {"access_token": "...", "expires_in": <seconds>}
//
// The sidecar binds its port only after it has fetched its SVID and
// self-registered, so the first calls at startup can fail with
// connection-refused. Callers retry until ready; see [TokenClient].
//
// The registry accepts lifecycle events at:
//
//	POST <registryURL>/v1/agents/<agentId>/events
//	  body: {"source":"agent","type":"<type>","payload":<any>}
//
// See [PostEvent].
//
// # Intentionally omitted (relative to the TypeScript @spawnly/sdk)
//
// Two helpers from the TypeScript SDK are deliberately NOT ported:
//
//   - instrumentFlue — it is specific to the Flue Node.js runtime; there is no
//     Go Flue runtime to instrument, so the helper has no meaning here.
//   - promptTimeoutSignal — it wraps AbortSignal.timeout; Go has a native
//     idiom, context.WithTimeout, so a dedicated helper is unnecessary. Pass a
//     deadline-bearing context.Context to any method instead.
package spawnly
