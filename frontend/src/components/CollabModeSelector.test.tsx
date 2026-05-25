// CollabModeSelector tests — pin the capability-gating UX. The
// component must:
//   1. Render every mode as a radio option.
//   2. Disable modes outside the allowedModes list.
//   3. Show the strict_zk-specific tooltip on disabled modes when
//      that's the folder's encryption_mode.
//   4. Call onChange only when an allowed option is clicked.

import { describe, it, expect, vi } from "vitest";
import { render, fireEvent } from "@testing-library/react";
import CollabModeSelector from "./CollabModeSelector";

// radio() walks the rendered fieldset to find the <input> with the
// given `value` attribute. The component's labels embed their
// descriptions inside the label, so getByLabelText would have to
// match the entire wrapped block — `value` is simpler and stable.
function radio(container: HTMLElement, value: string): HTMLInputElement {
  const input = container.querySelector<HTMLInputElement>(
    `input[type="radio"][value="${value}"]`,
  );
  if (!input) throw new Error(`no radio for value=${value}`);
  return input;
}

describe("CollabModeSelector", () => {
  it("renders the three modes and disables rich + rich_presence in strict_zk", () => {
    const { container } = render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown"]}
        encryptionMode="strict_zk"
      />,
    );
    expect(radio(container, "markdown").disabled).toBe(false);
    expect(radio(container, "rich").disabled).toBe(true);
    expect(radio(container, "rich_presence").disabled).toBe(true);
  });

  it("calls onChange when an allowed option is clicked", () => {
    const onChange = vi.fn();
    const { container } = render(
      <CollabModeSelector
        value="markdown"
        onChange={onChange}
        allowedModes={["markdown", "rich", "rich_presence"]}
        encryptionMode="managed_encrypted"
      />,
    );
    fireEvent.click(radio(container, "rich"));
    expect(onChange).toHaveBeenCalledWith("rich");
    fireEvent.click(radio(container, "rich_presence"));
    expect(onChange).toHaveBeenCalledWith("rich_presence");
  });

  it("surfaces a strict_zk-specific explanation in the tooltip of disabled modes", () => {
    const { container } = render(
      <CollabModeSelector
        value="markdown"
        onChange={() => undefined}
        allowedModes={["markdown"]}
        encryptionMode="strict_zk"
      />,
    );
    const richLabel = radio(container, "rich").closest("label");
    expect(richLabel).not.toBeNull();
    expect(richLabel?.getAttribute("title")).toMatch(/zero-knowledge/i);
  });
});
