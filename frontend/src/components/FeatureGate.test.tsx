import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, waitFor, cleanup } from "@testing-library/react";
import { FeatureGate } from "./FeatureGate";
import { FeaturesProvider } from "../hooks/useFeatures";
import { Feature } from "../features/featureKeys";

vi.mock("../hooks/useAuth", () => ({
  useAuth: () => ({ token: "tok", workspaceID: "ws", isAdmin: false, logout: () => {} }),
}));

const getFeatures = vi.fn();
vi.mock("../api/client", () => ({ getFeatures: () => getFeatures() }));

describe("FeatureGate", () => {
  afterEach(() => cleanup());

  it("renders children when the feature is enabled, fallback otherwise", async () => {
    getFeatures.mockResolvedValue({
      tier: "business",
      features: { webhooks: true, ai_summaries: false },
    });
    render(
      <FeaturesProvider>
        <FeatureGate feature={Feature.Webhooks}>
          <div>webhooks-ui</div>
        </FeatureGate>
        <FeatureGate feature={Feature.AISummaries} fallback={<div>locked</div>}>
          <div>ai-ui</div>
        </FeatureGate>
      </FeaturesProvider>,
    );
    await waitFor(() => expect(screen.getByText("webhooks-ui")).toBeTruthy());
    expect(screen.queryByText("ai-ui")).toBeNull();
    expect(screen.getByText("locked")).toBeTruthy();
  });
});
