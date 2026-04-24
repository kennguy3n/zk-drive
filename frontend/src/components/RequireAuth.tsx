import type { ReactNode } from "react";
import { Navigate } from "react-router-dom";
import { useAuth } from "../hooks/useAuth";

// RequireAuth is the single gate for authenticated routes. Keeping it as
// a wrapper component instead of a layout route makes it trivial to mix
// public and private pages under the same router.
export default function RequireAuth({ children }: { children: ReactNode }) {
  const { token } = useAuth();
  if (!token) {
    return <Navigate to="/login" replace />;
  }
  return <>{children}</>;
}
