import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { render, screen, waitFor, act, cleanup } from "@testing-library/react";
import { FeaturesProvider, useFeatures } from "./useFeatures";
import { Feature } from "../features/featureKeys";

// Mock the auth hook so we control the token that drives the fetch.
let mockToken: string | null = "tok";
vi.mock("./useAuth", () => ({
  useAuth: () => ({
    token: mockToken,
    workspaceID: "ws",
    role: "member",
    isAdmin: false,
    logout: () => {},
  }),
}));

// Mock the API client's getFeatures.
const getFeatures = vi.fn();
vi.mock("../api/client", () => ({
  getFeatures: () => getFeatures(),
}));

function Probe() {
  const { tier, loaded, isEnabled } = useFeatures();
  return (
    <div>
      <span data-testid="tier">{tier}</span>
      <span data-testid="loaded">{String(loaded)}</span>
      <span data-testid="sso">{String(isEnabled(Feature.SSO))}</span>
      <span data-testid="folders">{String(isEnabled(Feature.Folders))}</span>
    </div>
  );
}

describe("useFeatures", () => {
  beforeEach(() => {
    mockToken = "tok";
    getFeatures.mockReset();
  });
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    vi.useRealTimers();
  });

  it("fetches features on mount and gates by the returned map", async () => {
    getFeatures.mockResolvedValue({
      tier: "business",
      features: { folders: true, sso: true, strict_zk: false },
    });
    render(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );
    await waitFor(() => expect(screen.getByTestId("loaded").textContent).toBe("true"));
    expect(screen.getByTestId("tier").textContent).toBe("business");
    expect(screen.getByTestId("sso").textContent).toBe("true");
    expect(screen.getByTestId("folders").textContent).toBe("true");
  });

  it("retries then fails closed when the fetch keeps erroring", async () => {
    vi.useFakeTimers();
    getFeatures.mockRejectedValue(new Error("boom"));
    render(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );
    // Drive the initial attempt plus all backoff retries to completion.
    await act(async () => {
      await vi.runAllTimersAsync();
    });
    // Initial attempt + RETRY_DELAYS_MS.length (3) retries = 4 calls.
    expect(getFeatures).toHaveBeenCalledTimes(4);
    expect(screen.getByTestId("loaded").textContent).toBe("true");
    expect(screen.getByTestId("sso").textContent).toBe("false");
    expect(screen.getByTestId("folders").textContent).toBe("false");
  });

  it("recovers via retry after a transient fetch failure", async () => {
    vi.useFakeTimers();
    getFeatures
      .mockRejectedValueOnce(new Error("transient"))
      .mockResolvedValueOnce({
        tier: "business",
        features: { folders: true, sso: true },
      });
    render(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );
    await act(async () => {
      await vi.runAllTimersAsync();
    });
    // First attempt rejected, second (after one backoff) succeeded.
    expect(getFeatures).toHaveBeenCalledTimes(2);
    expect(screen.getByTestId("loaded").textContent).toBe("true");
    expect(screen.getByTestId("tier").textContent).toBe("business");
    expect(screen.getByTestId("sso").textContent).toBe("true");
    expect(screen.getByTestId("folders").textContent).toBe("true");
  });

  it("ignores a slow features response that resolves after logout", async () => {
    // Login fetch is in flight and resolves only after we trigger logout.
    let resolve!: (v: unknown) => void;
    getFeatures.mockReturnValue(
      new Promise((r) => {
        resolve = r;
      }),
    );
    const { rerender } = render(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );
    expect(getFeatures).toHaveBeenCalledTimes(1);

    // User logs out before the response arrives.
    mockToken = null;
    rerender(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );

    // The stale login response now resolves; it must NOT repaint the cleared
    // state with the previous session's features.
    await act(async () => {
      resolve({ tier: "business", features: { sso: true, folders: true } });
      await Promise.resolve();
    });
    expect(screen.getByTestId("tier").textContent).toBe("free");
    expect(screen.getByTestId("sso").textContent).toBe("false");
    expect(screen.getByTestId("folders").textContent).toBe("false");
  });

  it("does not fetch and reports nothing enabled when logged out", async () => {
    mockToken = null;
    render(
      <FeaturesProvider>
        <Probe />
      </FeaturesProvider>,
    );
    // Give effects a tick.
    await act(async () => {
      await Promise.resolve();
    });
    expect(getFeatures).not.toHaveBeenCalled();
    expect(screen.getByTestId("sso").textContent).toBe("false");
  });
});
