// CollabModeSelector tests — pin the capability-gating UX. The
// component must:
//   1. Render every mode as a radio option in a labelled radiogroup.
//   2. Disable modes outside the allowedModes list.
//   3. Show the strict_zk-specific explanation on disabled modes when
//      that's the folder's encryption_mode.
//   4. Call onChange only when an allowed option is activated.

import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import CollabModeSelector from "./CollabModeSelector";

// vitest runs with globals:false (see vite.config.ts), so React
// Testing Library's automatic afterEach cleanup is not registered.
// Unmount between tests manually, otherwise the screen.getAllByRole
// queries below would see radios accumulated from prior renders.
afterEach(cleanup);

// Each mode renders as a KChat RadioCard, i.e. a <button role="radio">.
// They're emitted in the fixed MODES order (markdown, rich,
// rich_presence), so we can address them positionally.
const MARKDOWN = 0;
const RICH = 1;
const RICH_PRESENCE = 2;

function radios(): HTMLButtonElement[] {
  return screen.getAllByRole("radio") as HTMLButtonElement[];
}

describe("CollabModeSelector", () => {
  it("renders a labelled radiogroup with the three modes", () => {
    render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown", "rich", "rich_presence"]}
        encryptionMode="managed_encrypted"
      />,
    );
    const group = screen.getByRole("radiogroup");
    // Wired to an accessible name via aria-labelledby (the
    // "Editor experience" label), so AT announces it as a group.
    expect(group.getAttribute("aria-labelledby")).toBeTruthy();
    expect(radios()).toHaveLength(3);
  });

  it("disables rich + rich_presence in strict_zk", () => {
    render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown"]}
        encryptionMode="strict_zk"
      />,
    );
    expect(radios()[MARKDOWN].disabled).toBe(false);
    expect(radios()[RICH].disabled).toBe(true);
    expect(radios()[RICH_PRESENCE].disabled).toBe(true);
    expect(radios()[MARKDOWN].getAttribute("aria-checked")).toBe("true");
  });

  it("calls onChange when an allowed option is clicked", () => {
    const onChange = vi.fn();
    render(
      <CollabModeSelector
        value="markdown"
        onChange={onChange}
        allowedModes={["markdown", "rich", "rich_presence"]}
        encryptionMode="managed_encrypted"
      />,
    );
    fireEvent.click(radios()[RICH]);
    expect(onChange).toHaveBeenCalledWith("rich");
    fireEvent.click(radios()[RICH_PRESENCE]);
    expect(onChange).toHaveBeenCalledWith("rich_presence");
  });

  it("does not call onChange for a disabled (policy-gated) option", () => {
    const onChange = vi.fn();
    render(
      <CollabModeSelector
        value="markdown"
        onChange={onChange}
        allowedModes={["markdown"]}
        encryptionMode="strict_zk"
      />,
    );
    fireEvent.click(radios()[RICH]);
    expect(onChange).not.toHaveBeenCalled();
  });

  it("surfaces a strict_zk-specific explanation on disabled modes", () => {
    render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown"]}
        encryptionMode="strict_zk"
      />,
    );
    // The disabled rich / rich_presence cards render the strict_zk
    // rationale as their description (collab.disabledStrictZk mentions
    // "zero-knowledge").
    expect(screen.getAllByText(/zero-knowledge/i).length).toBeGreaterThan(0);
  });

  it("restores a Saving… affordance and disables every mode when globally disabled", () => {
    render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown", "rich", "rich_presence"]}
        encryptionMode="managed_encrypted"
        disabled
      />,
    );
    // The parent sets `disabled` while a setCollabMode PATCH is in flight,
    // so every radio is forced off regardless of allowedModes.
    radios().forEach((r) => expect(r.disabled).toBe(true));
    // The textual "Saving…" hint is restored (role=status) and the group is
    // flagged busy for assistive tech.
    expect(screen.getByRole("status").textContent ?? "").toMatch(/saving/i);
    expect(screen.getByRole("radiogroup").getAttribute("aria-busy")).toBe(
      "true",
    );
  });
});
