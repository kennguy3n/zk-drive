import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup, within } from "@testing-library/react";
import FileList from "./FileList";
import type { FileItem } from "../api/client";

// FilePreview fetches a presigned URL on mount; stub the client so rows
// render synchronously without network calls. getDownloadURL is stubbed
// for the download action.
vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return {
    ...actual,
    getFilePreviewURL: vi.fn().mockRejectedValue(new Error("no preview")),
    getDownloadURL: vi.fn().mockResolvedValue("https://example/dl"),
  };
});

function mkFile(over: Partial<FileItem> & { id: string; name: string }): FileItem {
  return {
    workspace_id: "ws",
    folder_id: "f",
    size_bytes: 100,
    mime_type: "text/plain",
    current_version_id: "v1",
    created_at: "2024-01-01T00:00:00Z",
    updated_at: "2024-01-01T00:00:00Z",
    ...over,
  };
}

const files: FileItem[] = [
  mkFile({ id: "1", name: "banana.txt", size_bytes: 300 }),
  mkFile({ id: "2", name: "apple.txt", size_bytes: 100 }),
  mkFile({ id: "3", name: "cherry.txt", size_bytes: 200 }),
];

describe("FileList", () => {
  beforeEach(() => {
    vi.spyOn(window, "confirm").mockReturnValue(true);
  });
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });

  it("renders an empty state when there are no files", () => {
    render(<FileList files={[]} onRename={() => {}} onDelete={() => {}} />);
    expect(screen.getByText(/No files in this folder/)).toBeTruthy();
  });

  it("renders one grid row per file with action buttons", () => {
    render(<FileList files={files} onRename={() => {}} onDelete={() => {}} />);
    const rows = screen.getAllByRole("row");
    // header row + 3 data rows
    expect(rows).toHaveLength(4);
    expect(screen.getByText("banana.txt")).toBeTruthy();
    expect(screen.getAllByLabelText("Delete").length).toBe(3);
  });

  it("sorts by name ascending by default and reverses on header click", () => {
    render(<FileList files={files} onRename={() => {}} onDelete={() => {}} />);
    const names = () =>
      screen
        .getAllByRole("row")
        .slice(1)
        .map((r) => within(r).getByText(/\.txt$/).textContent);
    expect(names()).toEqual(["apple.txt", "banana.txt", "cherry.txt"]);
    fireEvent.click(screen.getByRole("columnheader", { name: /Name/ }));
    expect(names()).toEqual(["cherry.txt", "banana.txt", "apple.txt"]);
  });

  it("toggles selection through the checkbox when onToggleSelect is provided", () => {
    const onToggleSelect = vi.fn();
    render(
      <FileList
        files={files}
        onRename={() => {}}
        onDelete={() => {}}
        selectedIDs={new Set()}
        onToggleSelect={onToggleSelect}
      />,
    );
    const cb = screen.getAllByRole("checkbox")[0];
    fireEvent.click(cb);
    expect(onToggleSelect).toHaveBeenCalledTimes(1);
  });

  it("keeps exactly one focusable row after navigating to a smaller folder", () => {
    const many = Array.from({ length: 6 }, (_, i) =>
      mkFile({ id: `m${i}`, name: `m${i}.txt` }),
    );
    const { rerender } = render(
      <FileList files={many} onRename={() => {}} onDelete={() => {}} />,
    );
    const grid = screen.getByRole("grid");
    // Move the active row to the last item (out of range for the next folder).
    fireEvent.keyDown(grid, { key: "End" });
    // Navigate to a 2-file folder: activeIndex must clamp so a row stays
    // keyboard-focusable (tabIndex=0).
    rerender(
      <FileList
        files={[mkFile({ id: "a", name: "a.txt" }), mkFile({ id: "b", name: "b.txt" })]}
        onRename={() => {}}
        onDelete={() => {}}
      />,
    );
    const focusable = screen
      .getAllByRole("row")
      .slice(1)
      .filter((r) => r.getAttribute("tabindex") === "0");
    expect(focusable).toHaveLength(1);
  });

  it("deletes the active row on Delete keypress", () => {
    const onDelete = vi.fn();
    render(<FileList files={files} onRename={() => {}} onDelete={onDelete} />);
    // First row is active by default (sorted: apple.txt id=2).
    const grid = screen.getByRole("grid");
    fireEvent.keyDown(grid, { key: "Delete" });
    expect(onDelete).toHaveBeenCalledWith("2");
  });

  it("ignores grid shortcuts when focus is on a row action button", () => {
    const onDelete = vi.fn();
    render(<FileList files={files} onRename={() => {}} onDelete={onDelete} />);
    // With focus on an action button (not the row), a Delete keypress must
    // NOT trash the active row — otherwise the grid handler fires alongside
    // the button's own action (the double-action bug). The Download button is
    // a non-destructive proxy for "focus is inside a row control".
    const downloadBtn = screen.getAllByLabelText("Download")[0];
    fireEvent.keyDown(downloadBtn, { key: "Delete" });
    expect(onDelete).not.toHaveBeenCalled();
  });
});
