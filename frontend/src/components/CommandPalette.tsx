import {
  Suspense,
  createContext,
  lazy,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";
import { useAuth } from "../hooks/useAuth";

// The palette body (Radix Dialog + search UI) is code-split: it only
// downloads the first time the user opens the palette, keeping it out of
// the initial JS payload. The provider itself stays tiny — just the
// open/close state and the global Cmd+K / Ctrl+K listener.
const CommandPaletteDialog = lazy(() => import("./CommandPaletteDialog"));

interface CommandPaletteContextValue {
  open: () => void;
  close: () => void;
  toggle: () => void;
  isOpen: boolean;
}

const CommandPaletteContext = createContext<CommandPaletteContextValue | null>(null);

// CommandPaletteProvider mounts the global Cmd+K / Ctrl+K palette and
// exposes open/close controls via context. The shortcut is only armed for
// authenticated sessions so it never fires on the login screen. The heavy
// dialog UI is lazily loaded the first time the palette opens.
export function CommandPaletteProvider({ children }: { children: ReactNode }) {
  const { token } = useAuth();
  const [isOpen, setIsOpen] = useState(false);
  // Tracks whether the palette has ever been opened so we don't mount the
  // lazy chunk until it's actually needed.
  const [everOpened, setEverOpened] = useState(false);

  const open = useCallback(() => {
    setEverOpened(true);
    setIsOpen(true);
  }, []);
  const close = useCallback(() => setIsOpen(false), []);
  const toggle = useCallback(() => {
    setEverOpened(true);
    setIsOpen((o) => !o);
  }, []);

  useEffect(() => {
    if (!token) return;
    const onKey = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        e.preventDefault();
        toggle();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [token, toggle]);

  const value = useMemo<CommandPaletteContextValue>(
    () => ({ open, close, toggle, isOpen }),
    [open, close, toggle, isOpen],
  );

  return (
    <CommandPaletteContext.Provider value={value}>
      {children}
      {everOpened && (
        <Suspense fallback={null}>
          <CommandPaletteDialog open={isOpen} onOpenChange={setIsOpen} />
        </Suspense>
      )}
    </CommandPaletteContext.Provider>
  );
}

// useCommandPalette returns controls to open/close the global palette.
// Safe to call outside the provider (returns no-ops) so a stray trigger on
// the login screen doesn't crash.
export function useCommandPalette(): CommandPaletteContextValue {
  const ctx = useContext(CommandPaletteContext);
  if (!ctx) {
    return { open: () => {}, close: () => {}, toggle: () => {}, isOpen: false };
  }
  return ctx;
}
