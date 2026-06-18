import { describe, it, expect, vi, afterEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { OnboardingEmptyState } from "./OnboardingEmptyState";

describe("OnboardingEmptyState", () => {
  afterEach(() => cleanup());

  it("renders upload + create-folder cards and wires their handlers", () => {
    const onUpload = vi.fn();
    const onCreateFolder = vi.fn();
    render(
      <OnboardingEmptyState
        workspaceName="Acme"
        onUpload={onUpload}
        onCreateFolder={onCreateFolder}
      />,
    );
    expect(screen.getByText("Welcome to Acme")).toBeTruthy();
    // The drag-and-drop affordance hint is always present.
    expect(screen.getByText(/drag and drop/i)).toBeTruthy();
    fireEvent.click(screen.getByText("Upload your first file"));
    fireEvent.click(screen.getByText("Create a folder"));
    expect(onUpload).toHaveBeenCalledOnce();
    expect(onCreateFolder).toHaveBeenCalledOnce();
  });

  it("omits the invite card when onInvite is not provided", () => {
    render(
      <OnboardingEmptyState onUpload={() => {}} onCreateFolder={() => {}} />,
    );
    expect(screen.queryByText("Invite a teammate")).toBeNull();
  });

  it("shows the invite card when onInvite is provided", () => {
    const onInvite = vi.fn();
    render(
      <OnboardingEmptyState
        onUpload={() => {}}
        onCreateFolder={() => {}}
        onInvite={onInvite}
      />,
    );
    fireEvent.click(screen.getByText("Invite a teammate"));
    expect(onInvite).toHaveBeenCalledOnce();
  });
});
