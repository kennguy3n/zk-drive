import { useEffect, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { searchFiles, type SearchHit } from "../api/client";

// SearchBar is a header-mounted FTS input that queries the backend's
// /api/search endpoint. Results are rendered in a dropdown anchored to
// the input and collapse when the user clicks outside or picks a
// result. The component debounces its own calls so a user typing
// "report-final" doesn't hammer the backend with seven partial
// queries.
export default function SearchBar() {
  const nav = useNavigate();
  const [query, setQuery] = useState("");
  const [hits, setHits] = useState<SearchHit[]>([]);
  const [open, setOpen] = useState(false);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const wrapRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    // Close the dropdown when the user clicks outside the wrapper.
    // Important for keyboard-only users too — Escape also closes.
    const onDocClick = (e: MouseEvent) => {
      if (!wrapRef.current?.contains(e.target as Node)) {
        setOpen(false);
      }
    };
    document.addEventListener("mousedown", onDocClick);
    return () => document.removeEventListener("mousedown", onDocClick);
  }, []);

  useEffect(() => {
    if (!query.trim()) {
      setHits([]);
      setError(null);
      return;
    }
    // 250 ms is a comfortable compromise: fast enough to feel live,
    // slow enough to skip intermediate keystrokes on fast typists.
    const handle = window.setTimeout(async () => {
      setLoading(true);
      try {
        const resp = await searchFiles(query.trim(), { limit: 20 });
        setHits(resp.hits);
        setError(null);
        setOpen(true);
      } catch (err) {
        setError(String((err as Error)?.message ?? err));
      } finally {
        setLoading(false);
      }
    }, 250);
    return () => window.clearTimeout(handle);
  }, [query]);

  const pick = (hit: SearchHit) => {
    setOpen(false);
    setQuery("");
    if (hit.type === "folder") {
      nav(`/drive/folder/${hit.id}`);
    } else if (hit.folder_id) {
      nav(`/drive/folder/${hit.folder_id}`);
    }
  };

  return (
    <div ref={wrapRef} style={wrap}>
      <input
        type="search"
        placeholder="Search files and folders…"
        value={query}
        onFocus={() => query && setOpen(true)}
        onChange={(e) => setQuery(e.target.value)}
        onKeyDown={(e) => {
          if (e.key === "Escape") setOpen(false);
        }}
        style={input}
      />
      {open && (hits.length > 0 || error || loading) ? (
        <div style={dropdown} role="listbox">
          {loading ? <div style={statusRow}>Searching…</div> : null}
          {error ? <div style={{ ...statusRow, color: "#b91c1c" }}>{error}</div> : null}
          {hits.map((hit) => (
            <button
              key={`${hit.type}:${hit.id}`}
              onClick={() => pick(hit)}
              style={hitRow}
              role="option"
            >
              <span style={typeBadge(hit.type)}>{hit.type}</span>
              <span style={{ fontWeight: 500 }}>{hit.name}</span>
              <span style={{ color: "#6b7280", fontSize: 11 }}>{hit.path}</span>
            </button>
          ))}
          {!loading && !error && hits.length === 0 ? (
            <div style={statusRow}>No results</div>
          ) : null}
        </div>
      ) : null}
    </div>
  );
}

const wrap: React.CSSProperties = {
  position: "relative",
  width: 320,
};

const input: React.CSSProperties = {
  width: "100%",
  padding: "8px 10px",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  boxSizing: "border-box",
};

const dropdown: React.CSSProperties = {
  position: "absolute",
  top: "calc(100% + 4px)",
  left: 0,
  right: 0,
  background: "white",
  border: "1px solid #e5e7eb",
  borderRadius: 4,
  boxShadow: "0 10px 20px rgba(0,0,0,0.08)",
  maxHeight: 320,
  overflowY: "auto",
  zIndex: 20,
};

const hitRow: React.CSSProperties = {
  display: "grid",
  gridTemplateColumns: "auto 1fr",
  gridTemplateRows: "auto auto",
  columnGap: 8,
  padding: "8px 10px",
  border: "none",
  background: "transparent",
  width: "100%",
  textAlign: "left",
  cursor: "pointer",
  fontSize: 13,
  borderBottom: "1px solid #f3f4f6",
};

const statusRow: React.CSSProperties = {
  padding: "8px 10px",
  fontSize: 12,
  color: "#6b7280",
};

function typeBadge(type: "file" | "folder"): React.CSSProperties {
  return {
    gridRow: "1 / span 2",
    display: "inline-flex",
    alignItems: "center",
    justifyContent: "center",
    width: 48,
    height: 20,
    borderRadius: 10,
    fontSize: 10,
    fontWeight: 600,
    textTransform: "uppercase",
    background: type === "folder" ? "#fef3c7" : "#dbeafe",
    color: type === "folder" ? "#92400e" : "#1e40af",
  };
}
