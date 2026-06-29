import Constants from 'expo-constants';

// Build-time config from app.json `extra` (override per environment via EAS env
// or an app.config.js). In local dev, point gatewayUrl/issuer at your
// port-forwarded cluster, e.g. via EXPO_PUBLIC_* or by editing app.json.
type Extra = { issuer: string; gatewayUrl: string; clientId: string };

const extra = (Constants.expoConfig?.extra ?? {}) as Partial<Extra>;

export const config = {
  // The IdentityServer (OIDC) issuer. Discovery is fetched from here.
  issuer: process.env.EXPO_PUBLIC_ISSUER ?? extra.issuer ?? 'https://auth.spawnly.run',
  // The mobile-gateway public origin (its /me/* + SSE surface).
  gatewayUrl: process.env.EXPO_PUBLIC_GATEWAY_URL ?? extra.gatewayUrl ?? 'https://mobile.spawnly.run',
  // The public PKCE client registered in identityserver/Config.cs.
  clientId: process.env.EXPO_PUBLIC_CLIENT_ID ?? extra.clientId ?? 'mobile',
  // Explicit OIDC endpoints, for LOCAL dev only. When both are set, auth.ts uses
  // them directly instead of OIDC discovery — locally the discovery doc
  // advertises the in-cluster issuer host (http://identity-server:8080/...) that
  // a phone can't reach, so the native client must be pointed at the
  // dashboard-proxied /connect endpoints explicitly. Unset in production, where
  // discovery against `issuer` (auth.spawnly.run) is correct.
  authEndpoint: process.env.EXPO_PUBLIC_AUTH_ENDPOINT,
  tokenEndpoint: process.env.EXPO_PUBLIC_TOKEN_ENDPOINT,
  // Scopes: openid/profile for the id_token, offline_access for the refresh
  // token (stored in SecureStore), and the delegated authority the gateway
  // forwards to the orchestrator's consent endpoints.
  scopes: ['openid', 'profile', 'offline_access', 'orchestrator:read', 'orchestrator:write'],
};
