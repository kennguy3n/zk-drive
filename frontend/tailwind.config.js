/** @type {import('tailwindcss').Config} */
//
// Tailwind is configured to consume CSS custom properties (defined in
// src/index.css for both the light and dark themes) rather than hard-
// coded hex values. This is the single source of truth for colour: a
// `bg-surface` class and an inline `style={{ background: "var(--surface)" }}`
// resolve to the same token, so the legacy inline-styled components and
// the new Tailwind components stay visually consistent and both respond
// to the dark-mode toggle (which flips the `.dark` class on <html>).
//
// Colours are stored as space-separated RGB channels (e.g. "37 99 235")
// so Tailwind's `<alpha-value>` opacity modifiers (bg-brand/50) work.
export default {
  darkMode: "class",
  content: ["./index.html", "./src/**/*.{ts,tsx}"],
  theme: {
    extend: {
      colors: {
        brand: {
          DEFAULT: "rgb(var(--color-brand) / <alpha-value>)",
          fg: "rgb(var(--color-brand-fg) / <alpha-value>)",
          hover: "rgb(var(--color-brand-hover) / <alpha-value>)",
        },
        // Semantic surface / text tokens.
        bg: "rgb(var(--color-bg) / <alpha-value>)",
        surface: "rgb(var(--color-surface) / <alpha-value>)",
        "surface-2": "rgb(var(--color-surface-2) / <alpha-value>)",
        overlay: "rgb(var(--color-overlay) / <alpha-value>)",
        border: "rgb(var(--color-border) / <alpha-value>)",
        fg: "rgb(var(--color-fg) / <alpha-value>)",
        muted: "rgb(var(--color-muted) / <alpha-value>)",
        success: "rgb(var(--color-success) / <alpha-value>)",
        danger: "rgb(var(--color-danger) / <alpha-value>)",
        warning: "rgb(var(--color-warning) / <alpha-value>)",
        ring: "rgb(var(--color-ring) / <alpha-value>)",
      },
      borderRadius: {
        card: "12px",
      },
      boxShadow: {
        card: "0 1px 2px rgb(0 0 0 / 0.04), 0 4px 16px rgb(0 0 0 / 0.06)",
        overlay: "0 10px 38px rgb(0 0 0 / 0.18), 0 2px 8px rgb(0 0 0 / 0.10)",
      },
      keyframes: {
        "fade-in": {
          from: { opacity: "0" },
          to: { opacity: "1" },
        },
        "fade-out": {
          from: { opacity: "1" },
          to: { opacity: "0" },
        },
        "slide-out-right": {
          from: { opacity: "1", transform: "translateX(0)" },
          to: { opacity: "0", transform: "translateX(12px)" },
        },
        "scale-in": {
          from: { opacity: "0", transform: "translateY(4px) scale(0.98)" },
          to: { opacity: "1", transform: "translateY(0) scale(1)" },
        },
        "slide-in-right": {
          from: { opacity: "0", transform: "translateX(12px)" },
          to: { opacity: "1", transform: "translateX(0)" },
        },
        shimmer: {
          "100%": { transform: "translateX(100%)" },
        },
      },
      animation: {
        "fade-in": "fade-in 120ms ease-out",
        "fade-out": "fade-out 120ms ease-in",
        "scale-in": "scale-in 140ms cubic-bezier(0.16, 1, 0.3, 1)",
        "slide-in-right": "slide-in-right 160ms cubic-bezier(0.16, 1, 0.3, 1)",
        "slide-out-right": "slide-out-right 120ms ease-in",
      },
    },
  },
  plugins: [],
};
