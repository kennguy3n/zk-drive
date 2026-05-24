import { lazy, Suspense } from "react";
import { Navigate, Route, Routes } from "react-router-dom";
import LoginPage from "./pages/LoginPage";
import SignupPage from "./pages/SignupPage";
import FileBrowserPage from "./pages/FileBrowserPage";
import RequireAuth from "./components/RequireAuth";
import InstallPrompt from "./components/InstallPrompt";

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
