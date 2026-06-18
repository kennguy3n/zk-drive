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
import { AlertTriangle } from "lucide-react";
import { getEditorConfig, type OnlyOfficeEditorConfig } from "../api/client";
import { translateApiError } from "../api/errors";
import { Button, Skeleton } from "./ui";

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
      <div
        role="alert"
        className="flex h-full w-full flex-col items-center justify-center gap-4 p-8 text-center"
      >
        <span className="flex h-12 w-12 items-center justify-center rounded-full bg-danger/10 text-danger">
          <AlertTriangle className="h-6 w-6" aria-hidden="true" />
        </span>
        <p className="m-0 max-w-sm text-sm text-muted">{error}</p>
        {onClose ? (
          <Button variant="secondary" size="sm" onClick={onClose}>
            {t("common.close")}
          </Button>
        ) : null}
      </div>
    );
  }

  return (
    <div className="flex h-full w-full flex-col bg-surface">
      {onClose ? (
        <div className="flex justify-end border-b border-border px-3 py-2">
          <Button variant="ghost" size="sm" onClick={onClose}>
            {t("common.close")}
          </Button>
        </div>
      ) : null}
      <div className="relative min-h-[480px] flex-1">
        {/* The Document Server replaces this element's contents with its
            editor iframe. Kept mounted under the loading overlay so the
            id exists when DocEditor initialises. */}
        <div id={containerIdRef.current} className="absolute inset-0 h-full w-full" />
        {loading ? (
          <div
            className="absolute inset-0 flex flex-col gap-4 bg-surface p-6"
            role="status"
            aria-live="polite"
            aria-label={t("onlyoffice.loading")}
          >
            <div className="flex items-center gap-2 border-b border-border pb-4">
              <Skeleton className="h-8 w-8 rounded-lg" />
              <Skeleton className="h-8 w-24 rounded-lg" />
              <Skeleton className="h-8 w-24 rounded-lg" />
              <div className="flex-1" />
              <Skeleton className="h-8 w-20 rounded-lg" />
            </div>
            <div className="mx-auto flex w-full max-w-2xl flex-col gap-3 pt-6">
              <Skeleton className="h-6 w-1/2" />
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-full" />
              <Skeleton className="h-4 w-5/6" />
              <Skeleton className="h-4 w-2/3" />
            </div>
          </div>
        ) : null}
      </div>
    </div>
  );
}
