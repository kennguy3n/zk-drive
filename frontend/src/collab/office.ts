// office.ts — frontend mirror of the office-document extension table
// in internal/collab/onlyoffice.go (officeExtensions). Kept in sync so
// the UI shows the "Open in Editor" / "Edit" affordance for exactly
// the file types the backend can hand to the ONLYOFFICE Document
// Server. The server is the authority — it re-validates the type and
// returns 415 for anything it cannot open — so this list only governs
// whether the button appears, never whether the edit is allowed.

const OFFICE_EXTENSIONS = new Set<string>([
  // Word processing.
  "doc",
  "docx",
  "odt",
  "rtf",
  "txt",
  // Spreadsheets.
  "xls",
  "xlsx",
  "ods",
  "csv",
  // Presentations.
  "ppt",
  "pptx",
  "odp",
]);

// isOfficeDocument reports whether a filename has an extension the
// ONLYOFFICE editor can open. Extension-based (not MIME) because file
// listings always carry a name but MIME may be absent / generic
// (application/octet-stream) for direct-to-storage uploads.
export function isOfficeDocument(name: string | null | undefined): boolean {
  if (!name) return false;
  const dot = name.lastIndexOf(".");
  if (dot < 0 || dot === name.length - 1) return false;
  return OFFICE_EXTENSIONS.has(name.slice(dot + 1).toLowerCase());
}
