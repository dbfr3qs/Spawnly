import Constants from 'expo-constants';

// Build-time config from app.json `extra` (override per environment via EAS env
// or an app.config.js). In local dev, point gatewayUrl/issuer at your
// port-forwarded cluster, e.g. via EXPO_PUBLIC_* or by editing app.json.
type Extra = { issuer: string; gatewayUrl: string; clientId: string };

const extra = (Constants.expoConfig?.extra ?? {}) as Partial<Extra>;

export const config = {
  // The Keycloak realm (OIDC) issuer. Discovery is fetched from here. In
  // production this is the public realm URL, e.g.
  // https://auth.spawnly.run/realms/spawnly.
  issuer: process.env.EXPO_PUBLIC_ISSUER ?? extra.issuer ?? 'https://auth.spawnly.run/realms/spawnly',
  // The mobile-gateway public origin (its /me/* + SSE surface).
  gatewayUrl: process.env.EXPO_PUBLIC_GATEWAY_URL ?? extra.gatewayUrl ?? 'https://mobile.spawnly.run',
  // The public PKCE client registered in the Keycloak realm (deploy/keycloak/).
  clientId: process.env.EXPO_PUBLIC_CLIENT_ID ?? extra.clientId ?? 'mobile',
  // Explicit OIDC endpoints, for LOCAL dev only. When both are set, auth.ts uses
  // them directly instead of OIDC discovery — locally the discovery doc
  // advertises the in-cluster issuer host (http://keycloak:8080/realms/...) that
  // a phone can't reach, so the native client must be pointed at the
  // dashboard-proxied Keycloak endpoints
  // (/realms/spawnly/protocol/openid-connect/auth and /token) explicitly. Unset
  // in production, where discovery against `issuer` is correct.
  authEndpoint: process.env.EXPO_PUBLIC_AUTH_ENDPOINT,
  tokenEndpoint: process.env.EXPO_PUBLIC_TOKEN_ENDPOINT,
  // Scopes: openid/profile for the id_token, spawnly-roles for the flat `role`
  // claim (admin gating; distinct from Keycloak's built-in `roles` scope),
  // offline_access for the refresh token (stored in SecureStore), and the
  // delegated authority the gateway forwards to the orchestrator's consent
  // endpoints.
  scopes: ['openid', 'profile', 'spawnly-roles', 'offline_access', 'orchestrator:read', 'orchestrator:write'],
};
