import {
  createContext,
  useCallback,
  useContext,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import * as RToast from "@radix-ui/react-toast";
import { CheckCircle2, AlertTriangle, Info, X } from "lucide-react";
import { cn } from "../../lib/cn";

export type ToastVariant = "success" | "error" | "info";

export interface ToastOptions {
  title: string;
  description?: string;
  variant?: ToastVariant;
  /** Auto-dismiss after this many ms. Defaults to 5000; 0 = sticky. */
  durationMs?: number;
}

interface ToastRecord extends ToastOptions {
  id: number;
}

interface ToastContextValue {
  toast: (opts: ToastOptions) => void;
  success: (title: string, description?: string) => void;
  error: (title: string, description?: string) => void;
  info: (title: string, description?: string) => void;
  dismiss: (id: number) => void;
}

const ToastContext = createContext<ToastContextValue | null>(null);

const variantIcon: Record<ToastVariant, ReactNode> = {
  success: <CheckCircle2 className="h-5 w-5 text-success" aria-hidden="true" />,
  error: <AlertTriangle className="h-5 w-5 text-danger" aria-hidden="true" />,
  info: <Info className="h-5 w-5 text-brand" aria-hidden="true" />,
};

// ToastProvider renders the Radix Toast primitives and exposes an
// imperative API via context. Radix handles the accessibility heavy
// lifting (aria-live region, swipe/keyboard dismissal, focus rotation),
// so success/error/info announcements reach screen readers automatically.
export function ToastProvider({ children }: { children: ReactNode }) {
  const [toasts, setToasts] = useState<ToastRecord[]>([]);
  const nextId = useRef(1);

  const dismiss = useCallback((id: number) => {
    setToasts((prev) => prev.filter((t) => t.id !== id));
  }, []);

  const toast = useCallback((opts: ToastOptions) => {
    const id = nextId.current++;
    setToasts((prev) => [...prev, { id, variant: "info", ...opts }]);
  }, []);

  const value = useMemo<ToastContextValue>(
    () => ({
      toast,
      dismiss,
      success: (title, description) => toast({ title, description, variant: "success" }),
      error: (title, description) =>
        toast({ title, description, variant: "error", durationMs: 8000 }),
      info: (title, description) => toast({ title, description, variant: "info" }),
    }),
    [toast, dismiss],
  );

  return (
    <ToastContext.Provider value={value}>
      <RToast.Provider swipeDirection="right">
        {children}
        {toasts.map((t) => (
          <RToast.Root
            key={t.id}
            duration={t.durationMs === 0 ? Infinity : (t.durationMs ?? 5000)}
            onOpenChange={(open) => {
              if (!open) dismiss(t.id);
            }}
            className={cn(
              "pointer-events-auto flex w-[360px] max-w-[calc(100vw-2rem)] items-start gap-3",
              "rounded-card border border-border bg-overlay p-4 shadow-overlay",
              "animate-slide-in-right data-[state=closed]:animate-slide-out-right",
            )}
          >
            <span className="mt-0.5 shrink-0">{variantIcon[t.variant ?? "info"]}</span>
            <div className="min-w-0 flex-1">
              <RToast.Title className="text-sm font-semibold text-fg">
                {t.title}
              </RToast.Title>
              {t.description && (
                <RToast.Description className="mt-0.5 break-words text-sm text-muted">
                  {t.description}
                </RToast.Description>
              )}
            </div>
            <RToast.Close
              aria-label="Dismiss notification"
              className="shrink-0 rounded p-1 text-muted hover:bg-surface-2 hover:text-fg"
            >
              <X className="h-4 w-4" aria-hidden="true" />
            </RToast.Close>
          </RToast.Root>
        ))}
        <RToast.Viewport className="fixed bottom-4 right-4 z-[100] flex w-auto flex-col gap-2 outline-none" />
      </RToast.Provider>
    </ToastContext.Provider>
  );
}

// useToast returns the imperative toast API. Throws outside the provider.
export function useToast(): ToastContextValue {
  const ctx = useContext(ToastContext);
  if (!ctx) {
    throw new Error("useToast must be used within a ToastProvider");
  }
  return ctx;
}
