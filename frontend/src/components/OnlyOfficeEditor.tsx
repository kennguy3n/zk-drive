// OnlyOfficeEditor — embeds an ONLYOFFICE Document Server editor for
// an office file. It fetches the signed editor config from the backend
// (GET /api/files/{id}/editor-config), lazily loads the Document
// Server's api.js, and instantiates `new DocsAPI.DocEditor(...)` into
// a container div.
//
// Graceful degradation: when office editing is not configured the
// backend responds 503 and we render a fallback message instead of a
// blank editor. Script-load and config-fetch failures surface the same
// fallback so a misconfigured Document Server never wedges the UI.

import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { getEditorConfig, type OnlyOfficeEditorConfig } from "../api/client";
import { translateApiError } from "../api/errors";

// DocsAPI is injected on window by the Document Server's api.js. We
// model only the surface we call (`new DocsAPI.DocEditor(id, config)`
// returning an object with `destroyEditor()`).
interface DocEditorInstance {
  destroyEditor: () => void;
}
interface DocsAPIGlobal {
  DocEditor: new (containerId: string, config: unknown) => DocEditorInstance;
}
declare global {
  interface Window {
    DocsAPI?: DocsAPIGlobal;
  }
}

// loadDocsAPI injects the Document Server's api.js exactly once and
// resolves when window.DocsAPI is available. Concurrent callers share
// the same in-flight promise so opening two editors doesn't inject the
// script twice.
let docsAPILoad: Promise<void> | null = null;
function loadDocsAPI(serverURL: string): Promise<void> {
  if (window.DocsAPI) return Promise.resolve();
  if (docsAPILoad) return docsAPILoad;
  docsAPILoad = new Promise<void>((resolve, reject) => {
    const src = `${serverURL.replace(/\/$/, "")}/web-apps/apps/api/documents/api.js`;
    const script = document.createElement("script");
    script.src = src;
    script.async = true;
    script.onload = () => resolve();
    script.onerror = () => {
      // Reset so a later retry can re-attempt the injection.
      docsAPILoad = null;
      reject(new Error("failed to load ONLYOFFICE api.js"));
    };
    document.head.appendChild(script);
  });
  return docsAPILoad;
}

// A stable, unique container id per editor instance — the Document
// Server's DocEditor takes an element id (not a node) and replaces its
// contents with an <iframe>.
let editorSeq = 0;

export interface OnlyOfficeEditorProps {
  fileID: string;
  mode?: "edit" | "view";
  onClose?: () => void;
}

export default function OnlyOfficeEditor({ fileID, mode = "edit", onClose }: OnlyOfficeEditorProps) {
  const { t } = useTranslation();
  // Keep the latest t in a ref so the editor effect can produce
  // translated messages without listing t in its dependency array — a
  // new t identity on language change must not tear down and re-init a
  // live co-editing session.
  const tRef = useRef(t);
  tRef.current = t;
  const [error, setError] = useState<string | null>(null);
  const [loading, setLoading] = useState(true);
  // Assign the unique container id once per mount; guarding with the ref
  // keeps ++editorSeq from running (and burning ids) on every render.
  const containerIdRef = useRef<string>("");
  if (!containerIdRef.current) {
    containerIdRef.current = `onlyoffice-editor-${++editorSeq}`;
  }
  const editorRef = useRef<DocEditorInstance | null>(null);

  useEffect(() => {
    let cancelled = false;
    setLoading(true);
    setError(null);

    let config: OnlyOfficeEditorConfig | null = null;
    getEditorConfig(fileID, mode)
      .then((cfg) => {
        config = cfg;
        return loadDocsAPI(cfg.documentServerUrl);
      })
      .then(() => {
        if (cancelled || !config) return;
        if (!window.DocsAPI) {
          setError(tRef.current("onlyoffice.unavailable"));
          setLoading(false);
          return;
        }
        // DocEditor expects the documentType / document / editorConfig
        // / token at the top level of its config object.
        editorRef.current = new window.DocsAPI.DocEditor(containerIdRef.current, {
          documentType: config.documentType,
          document: config.document,
          editorConfig: config.editorConfig,
          token: config.token,
          width: "100%",
          height: "100%",
          events: {
            onError: () => setError(tRef.current("onlyoffice.editorError")),
          },
        });
        setLoading(false);
      })
      .catch((e) => {
        if (cancelled) return;
        setError(translateApiError(e, tRef.current));
        setLoading(false);
      });

    return () => {
      cancelled = true;
      // Tear down the editor iframe so reopening (or navigating away)
      // doesn't leak a second co-editing session.
      if (editorRef.current) {
        try {
          editorRef.current.destroyEditor();
        } catch {
          // destroyEditor throws if the iframe is already gone; the
          // goal is just to release it, so swallow.
        }
        editorRef.current = null;
      }
    };
  }, [fileID, mode]);

  if (error) {
    return (
      <div style={fallbackStyle} role="alert">
        <p style={{ margin: 0 }}>{error}</p>
        {onClose ? (
          <button onClick={onClose} style={closeBtnStyle}>
            {t("common.close")}
          </button>
        ) : null}
      </div>
    );
  }

  return (
    <div style={wrapStyle}>
      {onClose ? (
        <div style={toolbarStyle}>
          <button onClick={onClose} style={closeBtnStyle}>
            {t("common.close")}
          </button>
        </div>
      ) : null}
      {loading ? <div style={fallbackStyle}>{t("common.loading")}</div> : null}
      {/* The Document Server replaces this element's contents with its
          editor iframe. Kept mounted under the loading overlay so the
          id exists when DocEditor initialises. */}
      <div id={containerIdRef.current} style={containerStyle} />
    </div>
  );
}

const wrapStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  width: "100%",
  height: "100%",
};

const toolbarStyle: React.CSSProperties = {
  display: "flex",
  justifyContent: "flex-end",
  padding: 8,
  borderBottom: "1px solid #e5e7eb",
};

const containerStyle: React.CSSProperties = {
  flex: 1,
  minHeight: 480,
};

const fallbackStyle: React.CSSProperties = {
  display: "flex",
  flexDirection: "column",
  gap: 12,
  alignItems: "center",
  justifyContent: "center",
  padding: 32,
  color: "#6b7280",
};

const closeBtnStyle: React.CSSProperties = {
  padding: "4px 10px",
  background: "transparent",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 12,
  cursor: "pointer",
};
