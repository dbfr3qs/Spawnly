/**
 * Tool handlers must NEVER surface upstream provider details to the agent: the
 * MCP SDK returns a thrown `error.message` verbatim as tool output, so a leaked
 * upstream URL, response body, request id, auth failure — and, from phase 2 on,
 * a provider's identity or API key — would flow straight to the caller. The house
 * rule for every tool is: log the real cause server-side, throw only this generic,
 * provider-agnostic error. Keep the `capability` label free of provider names.
 */
export function upstreamUnavailable(capability: string, internal?: unknown): Error {
  if (internal !== undefined) {
    console.error(`[travel-tools] ${capability} upstream failure:`, internal);
  }
  return new Error(`${capability} is unavailable right now — please try again`);
}
