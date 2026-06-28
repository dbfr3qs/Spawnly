import * as AuthSession from 'expo-auth-session';
import * as SecureStore from 'expo-secure-store';
import { config } from './config';

// The native redirect URI. In a standalone build this resolves to the app's
// custom scheme (spawnly://auth); in Expo Go / dev it uses the Expo proxy.
export const redirectUri = AuthSession.makeRedirectUri({ scheme: 'spawnly', path: 'auth' });

const ACCESS_KEY = 'spawnly.accessToken';
const REFRESH_KEY = 'spawnly.refreshToken';
const EXPIRY_KEY = 'spawnly.accessExpiry';

// A 30s skew so we refresh slightly before the access token actually expires.
const EXPIRY_SKEW_MS = 30_000;

async function discovery(): Promise<AuthSession.DiscoveryDocument> {
  return AuthSession.fetchDiscoveryAsync(config.issuer);
}

// login drives the authorization-code + PKCE flow against IdentityServer. On
// success the tokens are persisted to OS secure storage (Keychain/Keystore).
// expo-auth-session generates and verifies the PKCE code_verifier/challenge.
export async function login(): Promise<boolean> {
  const disco = await discovery();
  const request = new AuthSession.AuthRequest({
    clientId: config.clientId,
    redirectUri,
    responseType: AuthSession.ResponseType.Code,
    scopes: config.scopes,
    usePKCE: true,
  });
  const result = await request.promptAsync(disco);
  if (result.type !== 'success' || !result.params.code) return false;

  const token = await AuthSession.exchangeCodeAsync(
    {
      clientId: config.clientId,
      code: result.params.code,
      redirectUri,
      extraParams: { code_verifier: request.codeVerifier ?? '' },
    },
    disco,
  );
  await persist(token);
  return true;
}

async function persist(token: AuthSession.TokenResponse): Promise<void> {
  if (!token.accessToken) throw new Error('token response missing access_token');
  await SecureStore.setItemAsync(ACCESS_KEY, token.accessToken);
  if (token.refreshToken) await SecureStore.setItemAsync(REFRESH_KEY, token.refreshToken);
  const expiresInMs = (token.expiresIn ?? 3600) * 1000;
  await SecureStore.setItemAsync(EXPIRY_KEY, String(Date.now() + expiresInMs));
}

// accessToken returns a valid access token, silently refreshing via the stored
// refresh token when the current one is expired/near-expiry. Returns null when
// there is no usable session (the caller routes back to login).
export async function accessToken(): Promise<string | null> {
  const current = await SecureStore.getItemAsync(ACCESS_KEY);
  const expiryStr = await SecureStore.getItemAsync(EXPIRY_KEY);
  const expiry = expiryStr ? Number(expiryStr) : 0;
  if (current && Date.now() < expiry - EXPIRY_SKEW_MS) return current;

  const refresh = await SecureStore.getItemAsync(REFRESH_KEY);
  if (!refresh) return null;
  try {
    const disco = await discovery();
    const token = await AuthSession.refreshAsync(
      { clientId: config.clientId, refreshToken: refresh },
      disco,
    );
    await persist(token);
    return token.accessToken;
  } catch {
    // Refresh failed (revoked/expired) — clear the session so the app re-logs in.
    await logout();
    return null;
  }
}

export async function isLoggedIn(): Promise<boolean> {
  return (await accessToken()) !== null;
}

export async function logout(): Promise<void> {
  await SecureStore.deleteItemAsync(ACCESS_KEY);
  await SecureStore.deleteItemAsync(REFRESH_KEY);
  await SecureStore.deleteItemAsync(EXPIRY_KEY);
}
