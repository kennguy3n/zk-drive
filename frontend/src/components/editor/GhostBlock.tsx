import { useState, useEffect, useRef } from "react";
import { useTranslation } from "react-i18next";
import { Check, X, Loader2, ThumbsUp, ThumbsDown } from "lucide-react";
import { cn } from "../../lib/cn";

export interface GhostBlockProps {
  content: string;
  status: "streaming" | "done" | "error";
  errorMessage?: string;
  onAccept: () => void;
  onReject: () => void;
  onFeedback?: (rating: "up" | "down") => void;
}

export default function GhostBlock({
  content,
  status,
  errorMessage,
  onAccept,
  onReject,
  onFeedback,
}: GhostBlockProps) {
  const { t } = useTranslation();
  const [accepted, setAccepted] = useState(false);
  const [rejected, setRejected] = useState(false);
  const [feedback, setFeedback] = useState<"up" | "down" | null>(null);
  const scrollRef = useRef<HTMLDivElement>(null);
  const scrollTimerRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Auto-scroll to keep the latest streamed content visible. Throttled
  // to avoid competing smooth-scroll animations at high token rates
  // (15+ tokens/sec would otherwise trigger 15+ scrollIntoView calls/sec).
  useEffect(() => {
    if (status !== "streaming" || !scrollRef.current) return;
    if (scrollTimerRef.current) return;
    scrollTimerRef.current = setTimeout(() => {
      scrollTimerRef.current = null;
      scrollRef.current?.scrollIntoView({ behavior: "auto", block: "nearest" });
    }, 100);
  }, [content, status]);

  useEffect(() => {
    return () => {
      if (scrollTimerRef.current) clearTimeout(scrollTimerRef.current);
    };
  }, []);

  if (rejected) return null;

  const handleAccept = () => {
    setAccepted(true);
    onAccept();
  };

  const handleReject = () => {
    setRejected(true);
    onReject();
  };

  const handleFeedback = (rating: "up" | "down") => {
    setFeedback(rating);
    onFeedback?.(rating);
  };

  return (
    <div
      className={cn(
        "my-2 rounded-lg border p-4 transition-colors",
        status === "error"
          ? "border-danger/30 bg-danger/5"
          : accepted
            ? "border-success/30 bg-success/5"
            : "border-brand/30 bg-brand/5",
      )}
    >
      <div className="mb-2 flex items-center gap-2">
        {status === "streaming" && (
          <>
            <Loader2 className="h-3.5 w-3.5 animate-spin text-brand" aria-hidden="true" />
            <span className="text-xs font-medium text-muted">
              {t("editor.aiGenerating")}
            </span>
          </>
        )}
        {status === "done" && !accepted && (
          <>
            <span className="text-xs font-medium text-brand">
              {t("editor.aiSuggestion")}
            </span>
            <div className="ml-auto flex items-center gap-1">
              <button
                type="button"
                onClick={handleAccept}
                className="inline-flex h-7 items-center gap-1 rounded-md bg-success/10 px-2 text-xs font-medium text-success transition-colors hover:bg-success/20"
              >
                <Check className="h-3.5 w-3.5" aria-hidden="true" />
                {t("editor.aiAccept")}
              </button>
              <button
                type="button"
                onClick={handleReject}
                className="inline-flex h-7 items-center gap-1 rounded-md bg-surface-2 px-2 text-xs font-medium text-muted transition-colors hover:bg-danger/10 hover:text-danger"
              >
                <X className="h-3.5 w-3.5" aria-hidden="true" />
                {t("editor.aiReject")}
              </button>
            </div>
          </>
        )}
        {status === "error" && (
          <span className="text-xs font-medium text-danger">
            {t("editor.aiError")}: {errorMessage}
          </span>
        )}
        {accepted && (
          <>
            <span className="text-xs font-medium text-success">
              {t("editor.aiAccepted")}
            </span>
            {onFeedback && feedback === null && (
              <div className="ml-auto flex items-center gap-1">
                <button
                  type="button"
                  onClick={() => handleFeedback("up")}
                  className="inline-flex h-6 w-6 items-center justify-center rounded-md text-muted transition-colors hover:bg-success/10 hover:text-success"
                  title={t("editor.aiFeedbackUp")}
                >
                  <ThumbsUp className="h-3.5 w-3.5" aria-hidden="true" />
                </button>
                <button
                  type="button"
                  onClick={() => handleFeedback("down")}
                  className="inline-flex h-6 w-6 items-center justify-center rounded-md text-muted transition-colors hover:bg-danger/10 hover:text-danger"
                  title={t("editor.aiFeedbackDown")}
                >
                  <ThumbsDown className="h-3.5 w-3.5" aria-hidden="true" />
                </button>
              </div>
            )}
            {feedback && (
              <span className="ml-auto text-xs text-muted">
                {t("editor.aiFeedbackThanks")}
              </span>
            )}
          </>
        )}
      </div>
      <div
        ref={scrollRef}
        className={cn(
          "text-sm leading-relaxed whitespace-pre-wrap",
          status === "streaming" ? "text-muted" : "text-fg",
        )}
      >
        {content}
        {status === "streaming" && (
          <span className="ml-0.5 inline-block h-4 w-1.5 animate-pulse bg-brand align-middle" />
        )}
      </div>
    </div>
  );
}
