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

// App-level routing. Unauthenticated visitors hit /login; everyone else
// lands in the file browser at /drive. The :folderId variant lets us keep
// the current folder in the URL so refreshes / back-navigation work.
export default function App() {
  return (
    <Suspense fallback={<div>Loading...</div>}>
      <InstallPrompt />
      <Routes>
        <Route path="/login" element={<LoginPage />} />
        <Route path="/signup" element={<SignupPage />} />
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
  );
}
