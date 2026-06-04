import { useEffect, useState } from "react";
import {
  AUTH_CHANGE_EVENT,
  currentRole,
  currentToken,
  currentWorkspaceID,
  logout as apiLogout,
} from "../api/client";

// useAuth is the tiny "am I logged in?" hook every page needs. It's
// deliberately read-only — login/signup mutate localStorage directly via
// the api client. The native "storage" event re-renders subscribers when
// ANOTHER tab changes the session; AUTH_CHANGE_EVENT covers the
// same-tab case (storage events never fire in the tab that wrote them),
// so a subscriber mounted before login — e.g. useAuth at the App root —
// still picks up the new token without a page reload.
export function useAuth(): {
  token: string | null;
  workspaceID: string | null;
  role: string | null;
  isAdmin: boolean;
  logout: () => void;
} {
  const [token, setToken] = useState<string | null>(currentToken());
  const [workspaceID, setWorkspaceID] = useState<string | null>(currentWorkspaceID());
  const [role, setRole] = useState<string | null>(currentRole());

  useEffect(() => {
    const sync = () => {
      setToken(currentToken());
      setWorkspaceID(currentWorkspaceID());
      setRole(currentRole());
    };
    window.addEventListener("storage", sync);
    window.addEventListener(AUTH_CHANGE_EVENT, sync);
    return () => {
      window.removeEventListener("storage", sync);
      window.removeEventListener(AUTH_CHANGE_EVENT, sync);
    };
  }, []);

  return {
    token,
    workspaceID,
    role,
    isAdmin: role === "admin",
    logout: () => {
      apiLogout();
      setToken(null);
      setWorkspaceID(null);
      setRole(null);
    },
  };
}
