import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
  type ReactNode,
} from "react";
import { Search } from "lucide-react";
import { Modal } from "./Modal";
import { Button } from "./Button";
import { Input } from "./Field";
import { cn } from "../../lib/cn";

// Promise-based dialog primitives that replace the browser's native
// prompt() / confirm() (and the infamous "type a folder UUID" prompt).
// A single DialogsProvider renders Radix-backed modals — so they are
// themed, focus-trapped, keyboard-accessible and screen-reader friendly —
// and exposes an imperative async API:
//
//   const confirm = useConfirm();
//   if (await confirm({ title: "Delete file?", tone: "danger" })) { ... }
//
//   const prompt = usePrompt();
//   const name = await prompt({ title: "Rename", defaultValue: file.name });
//
//   const pick = useResourcePicker();
//   const folder = await pick({ title: "Move to…", items: folders });
//
// Calls are queued and shown one at a time (like the native dialogs they
// replace), so callers can await them serially.

export interface ConfirmOptions {
  title: ReactNode;
  description?: ReactNode;
  confirmLabel?: string;
  cancelLabel?: string;
  /** "danger" renders a destructive confirm button. */
  tone?: "default" | "danger";
}

export interface PromptOptions {
  title: ReactNode;
  description?: ReactNode;
  label?: ReactNode;
  placeholder?: string;
  defaultValue?: string;
  confirmLabel?: string;
  cancelLabel?: string;
  inputType?: "text" | "email" | "password" | "url" | "number";
  /** Reject an empty value. */
  required?: boolean;
  /**
   * Return an error string to block submission, or null/undefined to allow.
   * Receives the raw, untrimmed value (not the trimmed value used for the
   * `required` check), so trim inside the callback if whitespace matters.
   */
  validate?: (value: string) => string | null | undefined;
}

export interface PickerItem {
  id: string;
  label: ReactNode;
  /** Free-text used for filtering when the picker shows a search box. */
  searchText?: string;
  description?: ReactNode;
  icon?: ReactNode;
  disabled?: boolean;
}

export interface PickerOptions<T extends PickerItem = PickerItem> {
  title: ReactNode;
  description?: ReactNode;
  items: T[];
  confirmLabel?: string;
  cancelLabel?: string;
  /** Show a filter box above the list when true (default: 8+ items). */
  searchable?: boolean;
  searchPlaceholder?: string;
  emptyMessage?: string;
}

type Request =
  | { id: number; kind: "confirm"; opts: ConfirmOptions; resolve: (v: boolean) => void }
  | { id: number; kind: "prompt"; opts: PromptOptions; resolve: (v: string | null) => void }
  | {
      id: number;
      kind: "picker";
      opts: PickerOptions;
      resolve: (v: PickerItem | null) => void;
    };

interface DialogsContextValue {
  confirm: (opts: ConfirmOptions) => Promise<boolean>;
  prompt: (opts: PromptOptions) => Promise<string | null>;
  pickResource: <T extends PickerItem>(opts: PickerOptions<T>) => Promise<T | null>;
}

const DialogsContext = createContext<DialogsContextValue | null>(null);

export function DialogsProvider({ children }: { children: ReactNode }) {
  const [queue, setQueue] = useState<Request[]>([]);
  const nextId = useRef(1);

  const enqueue = useCallback((req: Omit<Request, "id">) => {
    // Compute the id outside the updater so a double-invoked updater (React 18
    // concurrent mode) can't bump the ref twice and skip ids.
    const id = nextId.current++;
    setQueue((prev) => [...prev, { ...req, id } as Request]);
  }, []);

  const dequeue = useCallback(() => {
    setQueue((prev) => prev.slice(1));
  }, []);

  const value = useMemo<DialogsContextValue>(
    () => ({
      confirm: (opts) =>
        new Promise<boolean>((resolve) => enqueue({ kind: "confirm", opts, resolve })),
      prompt: (opts) =>
        new Promise<string | null>((resolve) =>
          enqueue({ kind: "prompt", opts, resolve }),
        ),
      pickResource: <T extends PickerItem>(opts: PickerOptions<T>) =>
        new Promise<T | null>((resolve) =>
          enqueue({
            kind: "picker",
            opts: opts as PickerOptions,
            // The selected item originates from opts.items (T[]), so the
            // narrowing back to T is sound.
            resolve: (item: PickerItem | null) => resolve(item as T | null),
          }),
        ),
    }),
    [enqueue],
  );

  const active = queue[0];

  return (
    <DialogsContext.Provider value={value}>
      {children}
      {active?.kind === "confirm" && (
        <ConfirmDialog key={active.id} req={active} onClose={dequeue} />
      )}
      {active?.kind === "prompt" && (
        <PromptDialog key={active.id} req={active} onClose={dequeue} />
      )}
      {active?.kind === "picker" && (
        <PickerDialog key={active.id} req={active} onClose={dequeue} />
      )}
    </DialogsContext.Provider>
  );
}

function ConfirmDialog({
  req,
  onClose,
}: {
  req: Extract<Request, { kind: "confirm" }>;
  onClose: () => void;
}) {
  const { opts, resolve } = req;
  // Guard against settle() firing twice (e.g. a button click followed by an
  // onOpenChange during unmount); the second call must not dequeue the next
  // dialog. resolve() is already idempotent for promises.
  const settled = useRef(false);
  const settle = (result: boolean) => {
    if (settled.current) return;
    settled.current = true;
    resolve(result);
    onClose();
  };
  return (
    <Modal
      open
      onOpenChange={(o) => !o && settle(false)}
      title={opts.title}
      description={opts.description}
      size="sm"
      footer={
        <>
          <Button variant="secondary" onClick={() => settle(false)}>
            {opts.cancelLabel ?? "Cancel"}
          </Button>
          <Button
            variant={opts.tone === "danger" ? "danger" : "primary"}
            onClick={() => settle(true)}
            autoFocus
          >
            {opts.confirmLabel ?? "Confirm"}
          </Button>
        </>
      }
    >
      {null}
    </Modal>
  );
}

function PromptDialog({
  req,
  onClose,
}: {
  req: Extract<Request, { kind: "prompt" }>;
  onClose: () => void;
}) {
  const { opts, resolve } = req;
  const [value, setValue] = useState(opts.defaultValue ?? "");
  const [error, setError] = useState<string | null>(null);
  const settled = useRef(false);

  const settle = (result: string | null) => {
    if (settled.current) return;
    settled.current = true;
    resolve(result);
    onClose();
  };

  const submit = () => {
    const trimmed = value.trim();
    if (opts.required && !trimmed) {
      setError("This field is required.");
      return;
    }
    const validationError = opts.validate?.(value);
    if (validationError) {
      setError(validationError);
      return;
    }
    settle(value);
  };

  return (
    <Modal
      open
      onOpenChange={(o) => !o && settle(null)}
      title={opts.title}
      description={opts.description}
      size="sm"
      footer={
        <>
          <Button variant="secondary" onClick={() => settle(null)}>
            {opts.cancelLabel ?? "Cancel"}
          </Button>
          <Button variant="primary" onClick={submit}>
            {opts.confirmLabel ?? "OK"}
          </Button>
        </>
      }
    >
      <form
        onSubmit={(e) => {
          e.preventDefault();
          submit();
        }}
        className="flex flex-col gap-1.5"
      >
        {opts.label && (
          <label className="text-sm font-medium text-fg">{opts.label}</label>
        )}
        <Input
          autoFocus
          type={opts.inputType ?? "text"}
          value={value}
          placeholder={opts.placeholder}
          aria-invalid={error ? true : undefined}
          onChange={(e) => {
            setValue(e.target.value);
            if (error) setError(null);
          }}
        />
        {error && (
          <p className="text-sm text-danger" role="alert">
            {error}
          </p>
        )}
      </form>
    </Modal>
  );
}

function PickerDialog({
  req,
  onClose,
}: {
  req: Extract<Request, { kind: "picker" }>;
  onClose: () => void;
}) {
  const { opts, resolve } = req;
  const [selected, setSelected] = useState<string | null>(null);
  const [query, setQuery] = useState("");
  const settled = useRef(false);

  const settle = (item: PickerItem | null) => {
    if (settled.current) return;
    settled.current = true;
    resolve(item);
    onClose();
  };

  const showSearch = opts.searchable ?? opts.items.length >= 8;
  const filtered = useMemo(() => {
    const q = query.trim().toLowerCase();
    if (!q) return opts.items;
    return opts.items.filter((it) => {
      const hay = (
        it.searchText ?? (typeof it.label === "string" ? it.label : "")
      ).toLowerCase();
      return hay.includes(q);
    });
  }, [opts.items, query]);

  // If the active search hides the currently selected item, drop the
  // selection so the confirm button can't submit something off-screen.
  useEffect(() => {
    if (selected && !filtered.some((it) => it.id === selected)) {
      setSelected(null);
    }
  }, [filtered, selected]);

  const choose = () => {
    const item = filtered.find((it) => it.id === selected);
    if (item) settle(item);
  };

  return (
    <Modal
      open
      onOpenChange={(o) => !o && settle(null)}
      title={opts.title}
      description={opts.description}
      size="md"
      footer={
        <>
          <Button variant="secondary" onClick={() => settle(null)}>
            {opts.cancelLabel ?? "Cancel"}
          </Button>
          <Button variant="primary" onClick={choose} disabled={!selected}>
            {opts.confirmLabel ?? "Select"}
          </Button>
        </>
      }
    >
      <div className="flex flex-col gap-3">
        {showSearch && (
          <div className="relative">
            <Search
              className="pointer-events-none absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-muted"
              aria-hidden="true"
            />
            <Input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={opts.searchPlaceholder ?? "Search…"}
              className="pl-9"
              aria-label={opts.searchPlaceholder ?? "Search"}
            />
          </div>
        )}
        <div
          role="listbox"
          aria-label={typeof opts.title === "string" ? opts.title : "Choose an item"}
          className="max-h-72 overflow-y-auto rounded-lg border border-border"
        >
          {filtered.length === 0 ? (
            <p className="px-3 py-6 text-center text-sm text-muted">
              {opts.emptyMessage ?? "No matches."}
            </p>
          ) : (
            <ul className="divide-y divide-border">
              {filtered.map((it) => {
                const isSel = it.id === selected;
                return (
                  <li key={it.id}>
                    <button
                      type="button"
                      role="option"
                      aria-selected={isSel}
                      disabled={it.disabled}
                      onClick={() => setSelected(it.id)}
                      onDoubleClick={() => !it.disabled && settle(it)}
                      className={cn(
                        "flex w-full items-center gap-3 px-3 py-2.5 text-left text-sm transition-colors",
                        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
                        "disabled:opacity-50 disabled:pointer-events-none",
                        isSel ? "bg-brand/10 text-brand" : "text-fg hover:bg-surface-2",
                      )}
                    >
                      {it.icon && <span className="shrink-0">{it.icon}</span>}
                      <span className="min-w-0 flex-1">
                        <span className="block truncate font-medium">{it.label}</span>
                        {it.description && (
                          <span className="block truncate text-xs text-muted">
                            {it.description}
                          </span>
                        )}
                      </span>
                    </button>
                  </li>
                );
              })}
            </ul>
          )}
        </div>
      </div>
    </Modal>
  );
}

function useDialogs(): DialogsContextValue {
  const ctx = useContext(DialogsContext);
  if (!ctx) {
    throw new Error("Dialog hooks must be used within a DialogsProvider");
  }
  return ctx;
}

/** Returns an async confirm() that resolves true/false. */
export function useConfirm() {
  return useDialogs().confirm;
}

/**
 * Returns an async prompt() that resolves the entered string, or null if
 * cancelled.
 *
 * Whitespace contract: the `required` check rejects whitespace-only input,
 * but `validate` and the resolved value receive the RAW, untrimmed string
 * (so inputs where spaces are meaningful — passwords, display names — are
 * preserved). Trim inside your own `validate`/handler when you need to.
 */
export function usePrompt() {
  return useDialogs().prompt;
}

/** Returns an async picker that resolves the chosen item or null. */
export function useResourcePicker() {
  return useDialogs().pickResource;
}
