import { describe, it, expect } from "vitest";
import { SignJWT, generateKeyPair, exportJWK, createLocalJWKSet, type JWK } from "jose";
import { InvalidTokenError } from "@modelcontextprotocol/sdk/server/auth/errors.js";
import { SpawnlyTokenVerifier, parseScopes } from "./auth.js";
import { requireScope, ScopeError, SCOPE_FX } from "./scopes.js";

const ISSUER = "http://identity-server:8080";
const AUDIENCE = "travel-tools";

async function setup() {
  const { publicKey, privateKey } = await generateKeyPair("RS256");
  const jwk = (await exportJWK(publicKey)) as JWK;
  jwk.kid = "test";
  jwk.alg = "RS256";
  jwk.use = "sig";
  const jwks = createLocalJWKSet({ keys: [jwk] });
  const verifier = new SpawnlyTokenVerifier(
    { issuer: ISSUER, jwksUrl: "http://unused", audience: AUDIENCE },
    jwks,
  );
  const sign = (
    claims: Record<string, unknown>,
    opts: { exp?: string | number; aud?: string; iss?: string } = {},
  ) =>
    new SignJWT(claims)
      .setProtectedHeader({ alg: "RS256", kid: "test" })
      .setIssuer(opts.iss ?? ISSUER)
      .setAudience(opts.aud ?? AUDIENCE)
      .setIssuedAt()
      .setExpirationTime(opts.exp ?? "1h")
      .sign(privateKey);
  return { verifier, sign };
}

describe("SpawnlyTokenVerifier", () => {
  it("accepts a valid token and surfaces its scopes + client", async () => {
    const { verifier, sign } = await setup();
    const token = await sign({ scope: "fx:read flights:read", client_id: "fx-converter" });
    const info = await verifier.verifyAccessToken(token);
    expect(info.scopes).toContain("fx:read");
    expect(info.clientId).toBe("fx-converter");
  });

  it("rejects a token for a different audience", async () => {
    const { verifier, sign } = await setup();
    const token = await sign({ scope: "fx:read" }, { aud: "sample-api" });
    await expect(verifier.verifyAccessToken(token)).rejects.toThrow(InvalidTokenError);
  });

  it("rejects a token from a different issuer", async () => {
    const { verifier, sign } = await setup();
    const token = await sign({ scope: "fx:read" }, { iss: "http://evil.example" });
    await expect(verifier.verifyAccessToken(token)).rejects.toThrow(InvalidTokenError);
  });

  it("rejects an expired token", async () => {
    const { verifier, sign } = await setup();
    const token = await sign({ scope: "fx:read" }, { exp: Math.floor(Date.now() / 1000) - 60 });
    await expect(verifier.verifyAccessToken(token)).rejects.toThrow(InvalidTokenError);
  });

  it("rejects an HS256 token (algorithm-confusion defense)", async () => {
    const { verifier } = await setup();
    const hs = await new SignJWT({ scope: "fx:read" })
      .setProtectedHeader({ alg: "HS256" })
      .setIssuer(ISSUER)
      .setAudience(AUDIENCE)
      .setIssuedAt()
      .setExpirationTime("1h")
      .sign(new TextEncoder().encode("a-shared-secret-that-must-never-validate"));
    await expect(verifier.verifyAccessToken(hs)).rejects.toThrow(InvalidTokenError);
  });

  it("rejects a malformed token as InvalidTokenError (401, not 500)", async () => {
    const { verifier } = await setup();
    await expect(verifier.verifyAccessToken("not.a.jwt")).rejects.toThrow(InvalidTokenError);
  });

  it("rejects a delegation token", async () => {
    const { verifier, sign } = await setup();
    const token = await sign({ scope: "fx:read", token_use: "delegation" });
    await expect(verifier.verifyAccessToken(token)).rejects.toThrow(InvalidTokenError);
  });
});

describe("parseScopes", () => {
  it("parses a space-delimited string", () => expect(parseScopes("a b c")).toEqual(["a", "b", "c"]));
  it("parses a JSON array", () => expect(parseScopes(["a", "b"])).toEqual(["a", "b"]));
  it("returns [] for missing/odd input", () => {
    expect(parseScopes(undefined)).toEqual([]);
    expect(parseScopes(42)).toEqual([]);
  });
});

describe("requireScope", () => {
  it("passes when the scope is present", () =>
    expect(() => requireScope({ token: "", clientId: "", scopes: ["fx:read"] }, SCOPE_FX)).not.toThrow());
  it("throws ScopeError when the scope is missing", () =>
    expect(() => requireScope({ token: "", clientId: "", scopes: ["flights:read"] }, SCOPE_FX)).toThrow(ScopeError));
  it("throws when there is no auth at all", () =>
    expect(() => requireScope(undefined, SCOPE_FX)).toThrow(ScopeError));
});
