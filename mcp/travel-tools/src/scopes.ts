import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";

// One scope per tool. The user consents to a sub-agent receiving exactly one of
// these, so the scope is what authorizes a single tool — deny consent → no scope
// → the tool is uncallable.
export const SCOPE_FX = "fx:read";
export const SCOPE_FLIGHTS = "flights:read";
export const SCOPE_HOTELS = "hotels:read";

/** Raised when the validated token lacks the scope a tool requires. */
export class ScopeError extends Error {
  constructor(public readonly scope: string) {
    super(`access denied: token is missing the required scope '${scope}'`);
    this.name = "ScopeError";
  }
}

/**
 * Enforce that the validated token carries `scope` before a tool does any work.
 * Throwing here aborts the call with no upstream request — the scope (and the
 * consent behind it) is what makes a tool callable. A valid token for a different
 * tool's scope cannot reach this tool (confused-deputy protection).
 */
export function requireScope(auth: AuthInfo | undefined, scope: string): void {
  if (!auth || !auth.scopes.includes(scope)) {
    throw new ScopeError(scope);
  }
}
