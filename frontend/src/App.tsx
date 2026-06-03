import { lazy, Suspense } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import LoginPage from "./pages/LoginPage";
import SignupPage from "./pages/SignupPage";
import FileBrowserPage from "./pages/FileBrowserPage";
import RequireAuth from "./components/RequireAuth";
import InstallPrompt from "./components/InstallPrompt";
import { useAuth } from "./hooks/useAuth";
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

  return (
    <>
      <InstallPrompt />
      <Suspense fallback={<div>Loading...</div>}>
        <Routes>
          <Route path="/login" element={<LoginPage />} />
          <Route path="/signup" element={<SignupPage />} />
          {/* MFA pages are unauthenticated routes: they accept the
              short-lived mfa_challenge / mfa_enroll token passed via
              react-router navigation state, NOT a stored session
              token. The user explicitly does not have a session
              token at this point — that's the whole point of the
              two-factor flow. */}
          <Route path="/mfa-challenge" element={<MfaChallengePage />} />
          <Route path="/mfa-enroll" element={<TwoFactorEnrollPage />} />
          {/* Authenticated re-enrollment / disable flow from account
              settings. RequireAuth gives us the session token. */}
          <Route
            path="/account/2fa"
            element={
              <RequireAuth>
                <TwoFactorEnrollPage />
              </RequireAuth>
            }
          />
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
