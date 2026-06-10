// iam-core (OIDC) Authorization Code + PKCE flow for the SPA.
//
// zk-drive is a *public* OAuth2 client of iam-core: there is no client
// secret in the browser, so the flow is secured by PKCE (RFC 7636). The
// SPA discovers the authorize/token endpoints from GET /api/config
// (see api/client.ts getAppConfig), redirects the browser to iam-core's
// Universal Login, and on return exchanges the authorization code for
// tokens directly against iam-core's token endpoint.
//
// Token lifecycle:
//   - access_token  -> stored under zkdrive.token; sent as Bearer on
//                       every /api request by the client interceptor.
//   - refresh_token -> used by scheduleSilentRefresh to mint a new
//                       access token shortly before expiry, so an active
//                       session never bounces back to the login screen.
import {
  type AppConfig,
  currentRefreshToken,
  currentToken,
  fetchMe,
  logout,
  storeIamCoreTokens,
  storeIdentity,
  tokenExpiresAt,
} from "./client";

const PKCE_VERIFIER_KEY = "zkdrive.pkce_verifier";
const OAUTH_STATE_KEY = "zkdrive.oauth_state";
// Post-login return path, captured at beginLogin and consumed by the
// callback so a deep link survives the round trip to iam-core.
const RETURN_TO_KEY = "zkdrive.oauth_return_to";

// base64url-encode raw bytes without padding (RFC 7636 §A).
function base64UrlEncode(bytes: Uint8Array): string {
  let str = "";
  for (const b of bytes) {
    str += String.fromCharCode(b);
  }
  return btoa(str).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function randomUrlSafe(byteLength: number): string {
  const bytes = new Uint8Array(byteLength);
  crypto.getRandomValues(bytes);
  return base64UrlEncode(bytes);
}

async function s256Challenge(verifier: string): Promise<string> {
  const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(verifier));
  return base64UrlEncode(new Uint8Array(digest));
}

function requireOidcConfig(cfg: AppConfig): {
  authorizeURL: string;
  tokenURL: string;
  clientID: string;
  redirectURI: string;
  scopes: string[];
  audience?: string;
} {
  if (
    cfg.auth_mode !== "iam-core" ||
    !cfg.authorize_url ||
    !cfg.token_url ||
    !cfg.client_id ||
    !cfg.redirect_uri
  ) {
    throw new Error("iam-core OIDC config is incomplete");
  }
  return {
    authorizeURL: cfg.authorize_url,
    tokenURL: cfg.token_url,
    clientID: cfg.client_id,
    redirectURI: cfg.redirect_uri,
    scopes: cfg.scopes && cfg.scopes.length > 0 ? cfg.scopes : ["openid", "email", "profile"],
    audience: cfg.audience,
  };
}

// beginLogin generates a fresh PKCE verifier + CSRF state, persists them
// in sessionStorage (so they survive the redirect but not a new tab),
// and navigates the browser to iam-core's authorize endpoint. returnTo
// is the in-app path to land on after a successful login.
export async function beginLogin(cfg: AppConfig, returnTo = "/drive"): Promise<void> {
  const oidc = requireOidcConfig(cfg);
  const verifier = randomUrlSafe(32);
  const state = randomUrlSafe(32);
  const challenge = await s256Challenge(verifier);

  sessionStorage.setItem(PKCE_VERIFIER_KEY, verifier);
  sessionStorage.setItem(OAUTH_STATE_KEY, state);
  sessionStorage.setItem(RETURN_TO_KEY, returnTo);

  const params = new URLSearchParams({
    response_type: "code",
    client_id: oidc.clientID,
    redirect_uri: oidc.redirectURI,
    scope: oidc.scopes.join(" "),
    state,
    code_challenge: challenge,
    code_challenge_method: "S256",
  });
  if (oidc.audience) {
    params.set("audience", oidc.audience);
  }
  window.location.assign(`${oidc.authorizeURL}?${params.toString()}`);
}

// CallbackResult is returned by handleCallback so the callback page can
// route the user onward.
export interface CallbackResult {
  returnTo: string;
}

// handleCallback completes the Authorization Code flow: it validates the
// state against the value stashed at beginLogin (CSRF / fixation
// defense), exchanges the code for tokens using the stored PKCE
// verifier, persists them, then resolves the zk-drive identity via
// GET /api/me. searchParams is the callback URL's query string.
export async function handleCallback(
  cfg: AppConfig,
  searchParams: URLSearchParams,
): Promise<CallbackResult> {
  const oidc = requireOidcConfig(cfg);

  const err = searchParams.get("error");
  if (err) {
    const desc = searchParams.get("error_description");
    clearTransient();
    throw new Error(desc ? `${err}: ${desc}` : err);
  }

  const code = searchParams.get("code");
  const state = searchParams.get("state");
  const expectedState = sessionStorage.getItem(OAUTH_STATE_KEY);
  const verifier = sessionStorage.getItem(PKCE_VERIFIER_KEY);
  const returnTo = sessionStorage.getItem(RETURN_TO_KEY) || "/drive";

  if (!code || !state || !expectedState || state !== expectedState || !verifier) {
    clearTransient();
    throw new Error("invalid OAuth callback (state mismatch or missing code)");
  }

  const body = new URLSearchParams({
    grant_type: "authorization_code",
    code,
    redirect_uri: oidc.redirectURI,
    client_id: oidc.clientID,
    code_verifier: verifier,
  });

  const resp = await fetch(oidc.tokenURL, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });
  // The transient PKCE/state values are single-use; clear them as soon
  // as the exchange request has been issued regardless of outcome.
  clearTransient();
  if (!resp.ok) {
    const text = await resp.text().catch(() => "");
    throw new Error(`token exchange failed (${resp.status})${text ? `: ${text}` : ""}`);
  }
  const tokens = (await resp.json()) as {
    access_token: string;
    refresh_token?: string;
    expires_in?: number;
  };
  if (!tokens.access_token) {
    throw new Error("token response missing access_token");
  }
  storeIamCoreTokens(tokens);

  // Resolve the zk-drive-internal identity (user/workspace/role) now
  // that the access token is stored and will be attached by the client
  // interceptor. This also triggers first-login auto-provisioning on
  // the server.
  const me = await fetchMe();
  storeIdentity(me);

  scheduleSilentRefresh(cfg);
  return { returnTo };
}

// refreshTokens exchanges the stored refresh token for a fresh access
// token. Returns true on success. On failure (no refresh token, or the
// IdP rejected it) it returns false WITHOUT logging out — the caller
// decides; scheduleSilentRefresh treats a failure as session-end.
export async function refreshTokens(cfg: AppConfig): Promise<boolean> {
  const oidc = requireOidcConfig(cfg);
  const refreshToken = currentRefreshToken();
  if (!refreshToken) {
    return false;
  }
  const body = new URLSearchParams({
    grant_type: "refresh_token",
    refresh_token: refreshToken,
    client_id: oidc.clientID,
  });
  const resp = await fetch(oidc.tokenURL, {
    method: "POST",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
    body: body.toString(),
  });
  if (!resp.ok) {
    return false;
  }
  const tokens = (await resp.json()) as {
    access_token: string;
    refresh_token?: string;
    expires_in?: number;
  };
  if (!tokens.access_token) {
    return false;
  }
  // Some IdPs rotate refresh tokens, others don't return one on
  // refresh; preserve the existing one when absent.
  storeIamCoreTokens({
    access_token: tokens.access_token,
    refresh_token: tokens.refresh_token ?? refreshToken,
    expires_in: tokens.expires_in,
  });
  return true;
}

// refreshSkewMs is how far before the access token's expiry we proactively
// refresh, so an in-flight request never races the expiry boundary.
const refreshSkewMs = 60_000;
// minRefreshDelayMs floors the scheduler so a near-expired (or already
// expired) token triggers a single near-immediate refresh rather than a
// busy loop.
const minRefreshDelayMs = 1_000;

let refreshTimer: ReturnType<typeof setTimeout> | null = null;

// scheduleSilentRefresh arms a one-shot timer to refresh the access
// token shortly before it expires. It is idempotent: calling it again
// (e.g. after a refresh, or on app mount) replaces any pending timer. A
// failed refresh ends the session via logout(), which clears storage
// and lets RequireAuth bounce the user to /login.
export function scheduleSilentRefresh(cfg: AppConfig): void {
  if (refreshTimer) {
    clearTimeout(refreshTimer);
    refreshTimer = null;
  }
  if (cfg.auth_mode !== "iam-core" || !currentToken() || !currentRefreshToken()) {
    return;
  }
  const expiresAt = tokenExpiresAt();
  if (expiresAt === null) {
    return;
  }
  const delay = Math.max(expiresAt - Date.now() - refreshSkewMs, minRefreshDelayMs);
  refreshTimer = setTimeout(() => {
    void (async () => {
      const ok = await refreshTokens(cfg).catch(() => false);
      if (ok) {
        scheduleSilentRefresh(cfg);
      } else {
        logout();
      }
    })();
  }, delay);
}

function clearTransient(): void {
  sessionStorage.removeItem(PKCE_VERIFIER_KEY);
  sessionStorage.removeItem(OAUTH_STATE_KEY);
  sessionStorage.removeItem(RETURN_TO_KEY);
}
