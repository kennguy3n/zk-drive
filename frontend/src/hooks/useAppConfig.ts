import { useEffect, useState } from "react";
import { type AppConfig, getAppConfig } from "../api/client";

// The app config (auth mode + OIDC parameters) is deployment-static for
// the lifetime of a page load, so we fetch it once and share the
// in-flight promise across every consumer rather than refetching per
// component mount.
let cached: Promise<AppConfig> | null = null;

export function loadAppConfig(): Promise<AppConfig> {
  if (!cached) {
    cached = getAppConfig().catch((err) => {
      // Don't cache a failed fetch — allow a retry on the next call.
      cached = null;
      throw err;
    });
  }
  return cached;
}

export interface AppConfigState {
  config: AppConfig | null;
  loading: boolean;
  error: unknown;
}

// useAppConfig exposes GET /api/config to components. While loading,
// `config` is null and `loading` is true; consumers should render a
// neutral placeholder rather than assuming a mode. On error it falls
// back to built-in mode so a transient config blip can't lock users
// out of the (always-present) password form.
export function useAppConfig(): AppConfigState {
  const [state, setState] = useState<AppConfigState>({
    config: null,
    loading: true,
    error: null,
  });

  useEffect(() => {
    let active = true;
    loadAppConfig()
      .then((config) => {
        if (active) {
          setState({ config, loading: false, error: null });
        }
      })
      .catch((error) => {
        if (active) {
          setState({ config: { auth_mode: "builtin" }, loading: false, error });
        }
      });
    return () => {
      active = false;
    };
  }, []);

  return state;
}
