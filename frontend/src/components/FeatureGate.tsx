import { type ReactNode } from "react";
import { useFeatures } from "../hooks/useFeatures";
import type { FeatureKey } from "../features/featureKeys";

export interface FeatureGateProps {
  feature: FeatureKey | string;
  children: ReactNode;
  /** Rendered when the feature is disabled. Defaults to nothing. */
  fallback?: ReactNode;
}

// FeatureGate conditionally renders its children based on a single
// feature flag. Use it to hide admin nav, KChat, the ONLYOFFICE editor,
// AI-summary buttons, retention-policy UI, webhook config, etc. for
// workspaces whose tier doesn't include the feature.
//
//   <FeatureGate feature={Feature.Webhooks}><WebhookConfig /></FeatureGate>
export function FeatureGate({ feature, children, fallback = null }: FeatureGateProps) {
  const { isEnabled } = useFeatures();
  return <>{isEnabled(feature) ? children : fallback}</>;
}
