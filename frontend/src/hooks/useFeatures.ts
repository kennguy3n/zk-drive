import {
  createContext,
  createElement,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { getFeatures } from "../api/client";
import { useAuth } from "./useAuth";
import type { FeatureKey } from "../features/featureKeys";

export interface FeaturesState {
  /** Resolved billing tier for the workspace ("free" until loaded). */
  tier: string;
  /** Full enabled/disabled map keyed by feature. */
  features: Record<string, boolean>;
  loading: boolean;
  /** Non-null when the last fetch failed. */
  error: boolean;
  /** True once an initial fetch has resolved (success or failure). */
  loaded: boolean;
  /** isEnabled(key) — fail-closed: unknown / not-yet-loaded => false. */
  isEnabled: (feature: FeatureKey | string) => boolean;
  /** Re-fetch the feature set (e.g. after a tier upgrade). */
  refresh: () => void;
}

const FeaturesContext = createContext<FeaturesState | null>(null);

// FeaturesProvider fetches GET /api/features whenever an authenticated
// session appears (login) and clears the cache on logout. It gates
// progressive feature disclosure across the app: consumers call
// isEnabled(Feature.X) to decide whether to render a surface.
//
// Fail-closed semantics: while loading, on error, or for an unknown key,
// isEnabled returns false so a paid surface never flashes for a user who
// isn't entitled to it. Baseline features (folders/files/etc.) are always
// returned by the backend so they light up as soon as the fetch resolves.
export function FeaturesProvider({ children }: { children: ReactNode }) {
  const { token } = useAuth();
  const [tier, setTier] = useState<string>("free");
  const [features, setFeatures] = useState<Record<string, boolean>>({});
  const [loading, setLoading] = useState<boolean>(false);
  const [error, setError] = useState<boolean>(false);
  const [loaded, setLoaded] = useState<boolean>(false);
  // Guards against a stale response from a previous token overwriting a
  // newer session's features (login → logout → login race).
  const reqSeq = useRef(0);

  const load = useCallback(async () => {
    if (!token) {
      // Invalidate any in-flight request from the previous session so its
      // late response can't pass the staleness guard below and repaint the
      // cleared state with the old workspace's features (logout, or
      // logout→login into a different workspace).
      ++reqSeq.current;
      setFeatures({});
      setTier("free");
      setLoaded(false);
      setError(false);
      setLoading(false);
      return;
    }
    const seq = ++reqSeq.current;
    setLoading(true);
    setError(false);
    try {
      const resp = await getFeatures();
      if (seq !== reqSeq.current) return; // superseded
      setFeatures(resp.features);
      setTier(resp.tier);
      setLoaded(true);
    } catch {
      if (seq !== reqSeq.current) return;
      // Fail-closed: drop to an empty map so only always-on baseline
      // surfaces (which the caller may treat as defaults) render.
      setFeatures({});
      setError(true);
      setLoaded(true);
    } finally {
      if (seq === reqSeq.current) setLoading(false);
    }
  }, [token]);

  useEffect(() => {
    void load();
  }, [load]);

  const isEnabled = useCallback(
    (feature: FeatureKey | string) => features[feature] === true,
    [features],
  );

  const value = useMemo<FeaturesState>(
    () => ({
      tier,
      features,
      loading,
      error,
      loaded,
      isEnabled,
      refresh: () => void load(),
    }),
    [tier, features, loading, error, loaded, isEnabled, load],
  );

  return createElement(FeaturesContext.Provider, { value }, children);
}

// useFeatures returns the feature-gating API. Throws outside the provider
// so a missing <FeaturesProvider> fails loudly in development.
export function useFeatures(): FeaturesState {
  const ctx = useContext(FeaturesContext);
  if (!ctx) {
    throw new Error("useFeatures must be used within a FeaturesProvider");
  }
  return ctx;
}

// useFeatureEnabled is a convenience selector for a single flag.
export function useFeatureEnabled(feature: FeatureKey | string): boolean {
  return useFeatures().isEnabled(feature);
}
