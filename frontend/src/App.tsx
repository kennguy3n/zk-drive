import { Navigate, Route, Routes } from "react-router-dom";
import LoginPage from "./pages/LoginPage";
import SignupPage from "./pages/SignupPage";
import FileBrowserPage from "./pages/FileBrowserPage";
import RequireAuth from "./components/RequireAuth";

// App-level routing. Unauthenticated visitors hit /login; everyone else
// lands in the file browser at /drive. The :folderId variant lets us keep
// the current folder in the URL so refreshes / back-navigation work.
export default function App() {
  return (
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
      <Route path="/" element={<Navigate to="/drive" replace />} />
      <Route path="*" element={<Navigate to="/drive" replace />} />
    </Routes>
  );
}
