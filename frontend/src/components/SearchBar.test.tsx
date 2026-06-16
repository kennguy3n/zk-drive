import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { render, screen, fireEvent, cleanup } from "@testing-library/react";
import { MemoryRouter } from "react-router-dom";
import SearchBar from "./SearchBar";
import type { SearchResponse, SearchHit } from "../api/client";

// searchFiles is replaced per-test with deferreds so we can control
// exactly when each (debounced) request resolves relative to the
// others — the only way to exercise the out-of-order response race.
vi.mock("../api/client", async (orig) => {
  const actual = await orig<typeof import("../api/client")>();
  return { ...actual, searchFiles: vi.fn() };
});

// useNavigate is mocked so picking a result can assert the navigation
// target without standing up real routes.
const navigateMock = vi.fn();
vi.mock("react-router-dom", async (orig) => {
  const actual = await orig<typeof import("react-router-dom")>();
  return { ...actual, useNavigate: () => navigateMock };
});

import { searchFiles } from "../api/client";

function deferred<T>() {
  let resolve!: (v: T) => void;
  let reject!: (e: unknown) => void;
  const promise = new Promise<T>((res, rej) => {
    resolve = res;
    reject = rej;
  });
  return { promise, resolve, reject };
}

function resp(name: string): SearchResponse {
  return respWith({ type: "file", id: name, name, folder_id: null });
}

function respWith(overrides: Partial<SearchHit> & { name: string }): SearchResponse {
  const hit: SearchHit = {
    type: "file",
    id: overrides.name,
    path: `/${overrides.name}`,
    workspace_id: "ws",
    folder_id: null,
    updated_at: "2024-01-01T00:00:00Z",
    ...overrides,
  };
  return { query: hit.name, limit: 20, offset: 0, hits: [hit] };
}

// Drive the dropdown to a populated, open state for the given response so
// a result row can be clicked.
async function showResult(value: string, response: SearchResponse) {
  const input = screen.getByPlaceholderText("Search files and folders\u2026");
  const d = deferred<SearchResponse>();
  vi.mocked(searchFiles).mockReturnValue(d.promise);
  fireEvent.change(input, { target: { value } });
  fireEvent.focus(input);
  await vi.advanceTimersByTimeAsync(250);
  d.resolve(response);
  await vi.runAllTimersAsync();
}

function renderBar() {
  return render(
    <MemoryRouter>
      <SearchBar />
    </MemoryRouter>,
  );
}

describe("SearchBar", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.runOnlyPendingTimers();
    vi.useRealTimers();
    vi.clearAllMocks();
    cleanup();
  });

  it("ignores a stale response that resolves after a newer query fired", async () => {
    const stale = deferred<SearchResponse>();
    const fresh = deferred<SearchResponse>();
    vi.mocked(searchFiles)
      .mockReturnValueOnce(stale.promise)
      .mockReturnValueOnce(fresh.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    // First query fires and goes in-flight.
    fireEvent.change(input, { target: { value: "rep" } });
    await vi.advanceTimersByTimeAsync(250);
    // Second query supersedes it and also goes in-flight.
    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(250);
    expect(searchFiles).toHaveBeenCalledTimes(2);

    // The newer request resolves first and populates the dropdown.
    fresh.resolve(resp("fresh-result.txt"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("fresh-result.txt")).not.toBeNull();

    // The older request resolves last — it must NOT clobber the
    // current results with stale data for a superseded query.
    stale.resolve(resp("stale-result.txt"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("stale-result.txt")).toBeNull();
    expect(screen.queryByText("fresh-result.txt")).not.toBeNull();
  });

  it("clears stale hits the moment the query changes", async () => {
    const first = deferred<SearchResponse>();
    const second = deferred<SearchResponse>();
    vi.mocked(searchFiles)
      .mockReturnValueOnce(first.promise)
      .mockReturnValueOnce(second.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    // First query resolves and its results populate the dropdown.
    fireEvent.change(input, { target: { value: "rep" } });
    await vi.advanceTimersByTimeAsync(250);
    first.resolve(resp("rep-result.txt"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("rep-result.txt")).not.toBeNull();

    // Typing a new query must drop the previous results immediately —
    // before the new (still-pending) request resolves.
    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText("rep-result.txt")).toBeNull();

    // Resolve the pending request so the test leaves no timer in flight.
    second.resolve(resp("report.txt"));
    await vi.runAllTimersAsync();
  });

  it("clears a stale error the moment the query changes", async () => {
    const failed = deferred<SearchResponse>();
    const next = deferred<SearchResponse>();
    vi.mocked(searchFiles)
      .mockReturnValueOnce(failed.promise)
      .mockReturnValueOnce(next.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    // First query fails; focus opens the dropdown so the error shows.
    fireEvent.change(input, { target: { value: "rep" } });
    fireEvent.focus(input);
    await vi.advanceTimersByTimeAsync(250);
    failed.reject(new Error("search failed"));
    await vi.runAllTimersAsync();
    expect(screen.queryByText("search failed")).not.toBeNull();

    // Typing a new query must drop the stale error immediately —
    // before the new (still-pending) request resolves.
    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(0);
    expect(screen.queryByText("search failed")).toBeNull();

    // Resolve the pending request so the test leaves no timer in flight.
    next.resolve(resp("report.txt"));
    await vi.runAllTimersAsync();
  });

  it("navigates to the drive root when a root-level file result is picked", async () => {
    renderBar();
    // A file at the workspace root: folder_id === null. Before the fix
    // this click was a no-op (neither pick() branch matched).
    await showResult("root-file.txt", respWith({ name: "root-file.txt", folder_id: null }));

    fireEvent.click(screen.getByText("root-file.txt"));
    expect(navigateMock).toHaveBeenCalledWith("/drive");
  });

  it("navigates to the containing folder when a nested file result is picked", async () => {
    renderBar();
    await showResult("nested.txt", respWith({ name: "nested.txt", folder_id: "folder-123" }));

    fireEvent.click(screen.getByText("nested.txt"));
    expect(navigateMock).toHaveBeenCalledWith("/drive/folder/folder-123");
  });

  it("navigates to the folder itself when a folder result is picked", async () => {
    renderBar();
    await showResult("my-folder", respWith({ type: "folder", id: "folder-9", name: "my-folder", folder_id: null }));

    fireEvent.click(screen.getByText("my-folder"));
    expect(navigateMock).toHaveBeenCalledWith("/drive/folder/folder-9");
  });

  it("discards a response that resolves after the query is cleared", async () => {
    const d = deferred<SearchResponse>();
    vi.mocked(searchFiles).mockReturnValue(d.promise);

    renderBar();
    const input = screen.getByPlaceholderText("Search files and folders…");

    fireEvent.change(input, { target: { value: "report" } });
    await vi.advanceTimersByTimeAsync(250);
    expect(searchFiles).toHaveBeenCalledTimes(1);

    // Clear the box before the in-flight request resolves.
    fireEvent.change(input, { target: { value: "" } });
    d.resolve(resp("stale-result.txt"));
    await vi.runAllTimersAsync();

    expect(screen.queryByText("stale-result.txt")).toBeNull();
  });
});
