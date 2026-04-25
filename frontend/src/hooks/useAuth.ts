import { useEffect, useState } from "react";
import { currentRole, currentToken, currentWorkspaceID, logout as apiLogout } from "../api/client";

// useAuth is the tiny "am I logged in?" hook every page needs. It's
// deliberately read-only — login/signup mutate localStorage directly via
// the api client, and the "storage" event below re-renders subscribers.
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
    return () => window.removeEventListener("storage", sync);
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
