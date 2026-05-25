// ConnectionStatusChip tests — pin the read-only override path and
// the four live status labels. These render-only tests are quick
// but catch regressions in the editor header at zero runtime cost.

import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";
import ConnectionStatusChip from "./ConnectionStatusChip";

describe("ConnectionStatusChip", () => {
  it("renders read-only label regardless of status when readOnly is set", () => {
    render(<ConnectionStatusChip status="connected" readOnly />);
    expect(screen.getByText("Read-only")).toBeTruthy();
    expect(screen.queryByText("Live")).toBeNull();
  });

  it.each([
    ["connecting", "Connecting"],
    ["connected", "Live"],
    ["reconnecting", "Reconnecting"],
    ["disconnected", "Disconnected"],
  ] as const)("renders %s as %s", (status, label) => {
    render(<ConnectionStatusChip status={status} />);
    expect(screen.getByText(label)).toBeTruthy();
  });
});
