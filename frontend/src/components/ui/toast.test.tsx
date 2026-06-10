import { describe, it, expect, afterEach } from "vitest";
import { render, screen, act, cleanup, fireEvent } from "@testing-library/react";
import { ToastProvider, useToast } from "./toast";

function Trigger({ durationMs }: { durationMs?: number }) {
  const { toast } = useToast();
  return (
    <button onClick={() => toast({ title: "Sticky", durationMs })}>go</button>
  );
}

describe("ToastProvider", () => {
  afterEach(() => cleanup());

  it("keeps a durationMs:0 toast on screen (sticky, not instant-dismiss)", async () => {
    render(
      <ToastProvider>
        <Trigger durationMs={0} />
      </ToastProvider>,
    );
    fireEvent.click(screen.getByText("go"));
    expect(screen.getByText("Sticky")).toBeTruthy();

    // The documented API says durationMs:0 means "sticky". With the old
    // `duration={0}` Radix scheduled dismissal after 0ms, so the toast would
    // vanish on the next macrotask. A real tick must leave it in place.
    await act(async () => {
      await new Promise((r) => setTimeout(r, 50));
    });
    expect(screen.queryByText("Sticky")).toBeTruthy();
  });
});
