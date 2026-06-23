import { createRemoteJWKSet, jwtVerify, type JWTPayload, type JWTVerifyGetKey } from "jose";
import type { OAuthTokenVerifier } from "@modelcontextprotocol/sdk/server/auth/provider.js";
import type { AuthInfo } from "@modelcontextprotocol/sdk/server/auth/types.js";
import { InvalidTokenError } from "@modelcontextprotocol/sdk/server/auth/errors.js";

// parseScopes reads an OAuth `scope` claim. Duende emits it as a space-delimited
// string, but it can also arrive as a JSON array — mirrors the Go tokenvalidator
// (internal/tokenvalidator) so both halves of the platform agree on granted scopes.
export function parseScopes(scope: unknown): string[] {
  if (typeof scope === "string") return scope.split(" ").filter(Boolean);
  if (Array.isArray(scope)) return scope.filter((s): s is string => typeof s === "string");
  return [];
}

export interface SpawnlyVerifierOptions {
  /** IdP issuer the token's `iss` must equal (IS_ISSUER, e.g. http://identity-server:8080). */
  issuer: string;
  /** IdP JWKS endpoint (IS_JWKS_URL). */
  jwksUrl: string;
  /** This resource server's identifier; the token's `aud` must contain it. */
  audience: string;
}

/**
 * Validates a Spawnly access token, treating travel-tools as an OAuth 2.0
 * protected resource: signature against the IdP JWKS, plus `iss`, `aud`, and
 * expiry. Algorithms are pinned to RS256 so a token can't downgrade to `alg=none`
 * or an HMAC-with-public-key confusion attack. Returns the MCP AuthInfo carrying
 * the granted scopes; per-tool scope enforcement happens in each tool handler.
 */
export class SpawnlyTokenVerifier implements OAuthTokenVerifier {
  private readonly getKey: JWTVerifyGetKey;

  // keySet is injectable so tests can supply a local JWKS; production uses the
  // remote JWKS (cached + auto-refreshed by jose on unknown `kid`).
  constructor(private readonly opts: SpawnlyVerifierOptions, keySet?: JWTVerifyGetKey) {
    this.getKey = keySet ?? createRemoteJWKSet(new URL(opts.jwksUrl));
  }

  async verifyAccessToken(token: string): Promise<AuthInfo> {
    let payload: JWTPayload;
    try {
      ({ payload } = await jwtVerify(token, this.getKey, {
        issuer: this.opts.issuer,
        audience: this.opts.audience,
        algorithms: ["RS256"],
      }));
    } catch {
      // Any validation failure (bad signature, wrong iss/aud, expired, malformed,
      // non-RS256) becomes a 401 invalid_token via requireBearerAuth. Throwing a
      // generic Error here would surface as a 500; we also avoid leaking the jose
      // failure reason to the caller.
      throw new InvalidTokenError("token validation failed");
    }
    // jose has now enforced signature, iss, aud, exp and nbf.
    const claims = payload as JWTPayload & {
      scope?: unknown;
      client_id?: unknown;
      token_use?: unknown;
    };
    // Reject delegation tokens, matching every other resource server in the
    // platform (cmd/sample-api): a delegated token is only valid as the
    // subject_token of a token exchange, never to call a resource directly.
    // (The audience check above already rejects these — their aud is
    // "delegation" — but we enforce it explicitly for consistency.)
    if (claims.token_use === "delegation") {
      throw new InvalidTokenError("delegation tokens are not accepted by resource servers");
    }
    const clientId =
      typeof claims.client_id === "string" ? claims.client_id : (claims.sub ?? "");
    return {
      token,
      clientId,
      scopes: parseScopes(claims.scope),
      expiresAt: claims.exp,
    };
  }
}
