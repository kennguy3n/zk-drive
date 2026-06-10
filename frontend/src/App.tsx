import { lazy, Suspense, useEffect } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import LoginPage from "./pages/LoginPage";
import SignupPage from "./pages/SignupPage";
import CallbackPage from "./pages/CallbackPage";
import FileBrowserPage from "./pages/FileBrowserPage";
import RequireAuth from "./components/RequireAuth";
import InstallPrompt from "./components/InstallPrompt";
import { PagePreviewSkeleton } from "./components/ui/Skeleton";
import { useAuth } from "./hooks/useAuth";
import { useAppConfig } from "./hooks/useAppConfig";
import { scheduleSilentRefresh } from "./api/oidc";
import { usePushNotifications } from "./hooks/usePushNotifications";

// Admin-only pages are off the critical path — split them into their own
// chunks so the initial JS payload stays small for the login / drive flows.
const AdminPage = lazy(() => import("./pages/AdminPage"));
const BillingPage = lazy(() => import("./pages/BillingPage"));
const PlacementPage = lazy(() => import("./pages/PlacementPage"));
const EncryptionPage = lazy(() => import("./pages/EncryptionPage"));
const KChatRoomsPage = lazy(() => import("./pages/KChatRoomsPage"));
// PrivacyPage is the customer-facing explainer for the two per-folder
// privacy modes (docs/PRODUCT.md). Linked from the FileBrowserPage
// header and CreateFolderDialog, so it sits behind RequireAuth like
// the rest of the /drive surface.
const PrivacyPage = lazy(() => import("./pages/PrivacyPage"));
// Collab editor pages are off the critical path — TipTap + Yjs
// pull in ~300KB of editor JS that the file-browser flow doesn't
// need. Lazy-loading keeps the initial bundle small for users who
// never open a document.
const DocumentListPage = lazy(() => import("./pages/DocumentListPage"));
const DocumentEditorPage = lazy(() => import("./pages/DocumentEditorPage"));
// MFA pages are also off the critical path so the initial bundle
// keeps shipping only the login + drive flow.
const MfaChallengePage = lazy(() => import("./pages/MfaChallengePage"));
const TwoFactorEnrollPage = lazy(() => import("./pages/TwoFactorEnrollPage"));

// App-level routing. Unauthenticated visitors hit /login; everyone else
// lands in the file browser at /drive. The :folderId variant lets us keep
// the current folder in the URL so refreshes / back-navigation work.
//
// InstallPrompt sits OUTSIDE Suspense so its captured beforeinstallprompt
// event survives lazy-route transitions (the browser only fires that event
// once per page load).
export default function App() {
  // Register the browser for Web Push once the user is authenticated so
  // PWA notifications arrive with the tab closed. No-op when the browser
  // lacks push support or the server has web push disabled.
  const { token } = useAuth();
  usePushNotifications(token);
  const { config, loading } = useAppConfig();
  const iamCoreMode = config?.auth_mode === "iam-core";

  // In iam-core mode, arm the silent access-token refresh once config is
  // known and a session exists (e.g. after a page reload mid-session).
  // scheduleSilentRefresh is idempotent and a no-op in built-in mode.
  useEffect(() => {
    if (config) {
      scheduleSilentRefresh(config);
    }
  }, [config, token]);

  // Hold the initial render until the auth mode is known so the MFA
  // routes below aren't briefly mounted in iam-core mode (where the IdP
  // owns MFA) before config resolves.
  if (loading) {
    return (
      <>
        <InstallPrompt />
        <div>Loading...</div>
      </>
    );
  }

  return (
    <>
      <InstallPrompt />
      <Suspense fallback={<PagePreviewSkeleton />}>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          {/* iam-core OAuth2 redirect target. Present only in iam-core
              mode; the SPA exchanges the authorization code for tokens
              here via PKCE. */}
          {iamCoreMode && <Route path="/auth/callback" element={<CallbackPage />} />}
          {/* Built-in signup and the local MFA flow exist ONLY in
              built-in mode. In iam-core mode account creation and MFA
              (TOTP, passkeys) are owned by iam-core's Universal Login,
              so these routes are not mounted and fall through to the
              catch-all redirect. */}
          {!iamCoreMode && <Route path="/signup" element={<SignupPage />} />}
          {/* MFA pages are unauthenticated routes: they accept the
              short-lived mfa_challenge / mfa_enroll token passed via
              react-router navigation state, NOT a stored session
              token. The user explicitly does not have a session
              token at this point — that's the whole point of the
              two-factor flow. */}
          {!iamCoreMode && <Route path="/mfa-challenge" element={<MfaChallengePage />} />}
          {!iamCoreMode && <Route path="/mfa-enroll" element={<TwoFactorEnrollPage />} />}
          {/* Authenticated re-enrollment / disable flow from account
              settings. RequireAuth gives us the session token. */}
          {!iamCoreMode && (
            <Route
              path="/account/2fa"
              element={
                <RequireAuth>
                  <TwoFactorEnrollPage />
                </RequireAuth>
              }
            />
          )}
          <Route
            path="/drive"
            element={
              <RequireAuth>
                <FileBrowserPage />
              </RequireAuth>
            }
          />
          <Route
            path="/drive/folder/:folderId"
            element={
              <RequireAuth>
                <FileBrowserPage />
              </RequireAuth>
            }
          />
          <Route
            path="/drive/privacy"
            element={
              <RequireAuth>
                <PrivacyPage />
              </RequireAuth>
            }
          />
          <Route
            path="/drive/folder/:folderId/documents"
            element={
              <RequireAuth>
                <DocumentListPage />
              </RequireAuth>
            }
          />
          <Route
            path="/drive/document/:id"
            element={
              <RequireAuth>
                <DocumentEditorPage />
              </RequireAuth>
            }
          />
          {/* Canonical collaborative-editor deep link. Aliases the
              /drive/document/:id route above so links shaped as
              /documents/:id/edit (e.g. from the file list "Edit"
              button) resolve to the same TipTap + Yjs editor. */}
          <Route
            path="/documents/:id/edit"
            element={
              <RequireAuth>
                <DocumentEditorPage />
              </RequireAuth>
            }
          />
          <Route
            path="/admin"
            element={
              <RequireAuth>
                <AdminPage />
              </RequireAuth>
            }
          />
          <Route
            path="/billing"
            element={
              <RequireAuth>
                <BillingPage />
              </RequireAuth>
            }
          />
          <Route
            path="/admin/placement"
            element={
              <RequireAuth>
                <PlacementPage />
              </RequireAuth>
            }
          />
          <Route
            path="/admin/encryption"
            element={
              <RequireAuth>
                <EncryptionPage />
              </RequireAuth>
            }
          />
          <Route
            path="/admin/kchat"
            element={
              <RequireAuth>
                <KChatRoomsPage />
              </RequireAuth>
            }
          />
          <Route path="/" element={<Navigate to="/drive" replace />} />
          <Route path="*" element={<Navigate to="/drive" replace />} />
        </Routes>
      </Suspense>
    </>
  );
}
