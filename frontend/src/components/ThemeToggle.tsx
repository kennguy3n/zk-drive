import * as DropdownMenu from "@radix-ui/react-dropdown-menu";
import { Monitor, Moon, Sun, Check } from "lucide-react";
import { useTheme, type Theme } from "../theme/ThemeProvider";
import { cn } from "../lib/cn";

const options: { value: Theme; label: string; icon: typeof Sun }[] = [
  { value: "light", label: "Light", icon: Sun },
  { value: "dark", label: "Dark", icon: Moon },
  { value: "system", label: "System", icon: Monitor },
];

// ThemeToggle is a dropdown that lets the user pick light / dark / system.
// The trigger icon reflects the *resolved* theme so it always shows what's
// currently on screen. Built on Radix DropdownMenu for keyboard nav +
// focus management.
export function ThemeToggle() {
  const { theme, resolvedTheme, setTheme } = useTheme();
  const TriggerIcon = resolvedTheme === "dark" ? Moon : Sun;

  return (
    <DropdownMenu.Root>
      <DropdownMenu.Trigger asChild>
        <button
          type="button"
          aria-label="Change theme"
          className="inline-flex h-9 w-9 items-center justify-center rounded-lg text-fg hover:bg-surface-2 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <TriggerIcon className="h-5 w-5" aria-hidden="true" />
        </button>
      </DropdownMenu.Trigger>
      <DropdownMenu.Portal>
        <DropdownMenu.Content
          align="end"
          sideOffset={6}
          className="z-50 min-w-[160px] rounded-lg border border-border bg-overlay p-1 shadow-overlay animate-scale-in"
        >
          {options.map(({ value, label, icon: Icon }) => (
            <DropdownMenu.Item
              key={value}
              onSelect={() => setTheme(value)}
              className={cn(
                "flex cursor-pointer items-center gap-2 rounded-md px-2.5 py-2 text-sm text-fg outline-none",
                "data-[highlighted]:bg-surface-2",
              )}
            >
              <Icon className="h-4 w-4 text-muted" aria-hidden="true" />
              <span className="flex-1">{label}</span>
              {theme === value && (
                <Check className="h-4 w-4 text-brand" aria-hidden="true" />
              )}
            </DropdownMenu.Item>
          ))}
        </DropdownMenu.Content>
      </DropdownMenu.Portal>
    </DropdownMenu.Root>
  );
}
