import { clsx, type ClassValue } from "clsx";

// cn merges conditional class names. A thin wrapper over clsx so every
// component imports the same helper (and we can swap in tailwind-merge
// later without touching call sites).
export function cn(...inputs: ClassValue[]): string {
  return clsx(inputs);
}
