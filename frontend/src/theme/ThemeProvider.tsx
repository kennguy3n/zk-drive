import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

// Theme is the user's explicit preference. "system" defers to the OS
// (prefers-color-scheme) and keeps tracking it live; "light"/"dark" pin
// the choice. We persist the preference (not the resolved value) so a
// user who picked "system" keeps following the OS across reloads.
export type Theme = "light" | "dark" | "system";

// ResolvedTheme is the concrete theme actually applied to the DOM after
// resolving "system" against the media query.
export type ResolvedTheme = "light" | "dark";

const STORAGE_KEY = "zkdrive.theme";

interface ThemeContextValue {
  theme: Theme;
  resolvedTheme: ResolvedTheme;
  setTheme: (theme: Theme) => void;
  /** Cycle light -> dark -> system, used by the simple toggle button. */
  toggle: () => void;
}

const ThemeContext = createContext<ThemeContextValue | null>(null);

function prefersDark(): boolean {
  return (
    typeof window !== "undefined" &&
    typeof window.matchMedia === "function" &&
    window.matchMedia("(prefers-color-scheme: dark)").matches
  );
}

function readStoredTheme(): Theme {
  if (typeof localStorage === "undefined") return "system";
  const raw = localStorage.getItem(STORAGE_KEY);
  return raw === "light" || raw === "dark" || raw === "system" ? raw : "system";
}

function resolve(theme: Theme): ResolvedTheme {
  if (theme === "system") return prefersDark() ? "dark" : "light";
  return theme;
}

// applyTheme toggles the `.dark` class on <html> so every CSS-variable
// token defined under `.dark` in index.css takes effect app-wide, and
// sets the native color-scheme so form controls / scrollbars match.
function applyTheme(resolved: ResolvedTheme): void {
  if (typeof document === "undefined") return;
  const root = document.documentElement;
  root.classList.toggle("dark", resolved === "dark");
  root.style.colorScheme = resolved;
}

export function ThemeProvider({ children }: { children: ReactNode }) {
  const [theme, setThemeState] = useState<Theme>(() => readStoredTheme());
  const [resolvedTheme, setResolvedTheme] = useState<ResolvedTheme>(() =>
    resolve(readStoredTheme()),
  );

  // Apply on mount + whenever the explicit preference changes.
  useEffect(() => {
    const next = resolve(theme);
    setResolvedTheme(next);
    applyTheme(next);
  }, [theme]);

  // When following the system, keep in sync with live OS changes.
  useEffect(() => {
    if (theme !== "system") return;
    if (typeof window === "undefined" || !window.matchMedia) return;
    const mq = window.matchMedia("(prefers-color-scheme: dark)");
    const onChange = () => {
      const next: ResolvedTheme = mq.matches ? "dark" : "light";
      setResolvedTheme(next);
      applyTheme(next);
    };
    mq.addEventListener("change", onChange);
    return () => mq.removeEventListener("change", onChange);
  }, [theme]);

  const setTheme = useCallback((next: Theme) => {
    setThemeState(next);
    try {
      localStorage.setItem(STORAGE_KEY, next);
    } catch {
      // Private-mode / disabled storage: theme still applies for the
      // session, it just won't persist. Non-fatal.
    }
  }, []);

  const toggle = useCallback(() => {
    // Delegate to setTheme so the localStorage write stays outside the
    // setState updater (which React may invoke twice in Strict Mode). theme
    // is in the dep list so the closure always sees the current value.
    setTheme(theme === "light" ? "dark" : theme === "dark" ? "system" : "light");
  }, [theme, setTheme]);

  const value = useMemo<ThemeContextValue>(
    () => ({ theme, resolvedTheme, setTheme, toggle }),
    [theme, resolvedTheme, setTheme, toggle],
  );

  return <ThemeContext.Provider value={value}>{children}</ThemeContext.Provider>;
}

// useTheme returns the theme controls. Throws if used outside the
// provider so a missing <ThemeProvider> fails loudly in development.
export function useTheme(): ThemeContextValue {
  const ctx = useContext(ThemeContext);
  if (!ctx) {
    throw new Error("useTheme must be used within a ThemeProvider");
  }
  return ctx;
}
