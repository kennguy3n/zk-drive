import {
  forwardRef,
  useId,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
} from "react";
import { cn } from "../../lib/cn";

// Shared, fully tokenised form controls. Every workstream builds its forms
// from these so inputs look identical, respond to the dark-mode toggle and
// the KChat re-theme, and carry the same focus ring + invalid styling
// without re-deriving Tailwind classes (or falling back to inline styles).

const controlBase =
  "w-full rounded-lg border border-border bg-surface text-sm text-fg " +
  "placeholder:text-muted shadow-sm transition-colors " +
  "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring " +
  "focus-visible:border-brand disabled:opacity-60 disabled:cursor-not-allowed " +
  "aria-[invalid=true]:border-danger aria-[invalid=true]:focus-visible:ring-danger";

export const Input = forwardRef<HTMLInputElement, InputHTMLAttributes<HTMLInputElement>>(
  ({ className, ...rest }, ref) => (
    <input ref={ref} className={cn(controlBase, "h-10 px-3", className)} {...rest} />
  ),
);
Input.displayName = "Input";

export const Textarea = forwardRef<
  HTMLTextAreaElement,
  TextareaHTMLAttributes<HTMLTextAreaElement>
>(({ className, rows = 4, ...rest }, ref) => (
  <textarea
    ref={ref}
    rows={rows}
    className={cn(controlBase, "min-h-[80px] px-3 py-2 leading-relaxed", className)}
    {...rest}
  />
));
Textarea.displayName = "Textarea";

export const Select = forwardRef<
  HTMLSelectElement,
  SelectHTMLAttributes<HTMLSelectElement>
>(({ className, children, ...rest }, ref) => (
  <select ref={ref} className={cn(controlBase, "h-10 px-3 pr-8", className)} {...rest}>
    {children}
  </select>
));
Select.displayName = "Select";

export interface FieldProps {
  label: ReactNode;
  /** Helper text shown under the control when there is no error. */
  hint?: ReactNode;
  /** Error message; when present it replaces the hint and marks invalid. */
  error?: ReactNode;
  /** Marks the field required (adds an asterisk to the label). */
  required?: boolean;
  /** Visually hide the label but keep it for screen readers. */
  hideLabel?: boolean;
  className?: string;
  /**
   * Render prop receiving the wiring (id + aria-* attributes) to spread
   * onto the control so the label, hint and error are correctly
   * associated for assistive tech.
   */
  children: (props: {
    id: string;
    "aria-describedby"?: string;
    "aria-invalid"?: boolean;
    required?: boolean;
  }) => ReactNode;
}

// Field wires a label, optional hint and error message to a control with
// the correct id / aria-describedby / aria-invalid wiring (WCAG 3.3.1/1.3.1).
export function Field({
  label,
  hint,
  error,
  required,
  hideLabel,
  className,
  children,
}: FieldProps) {
  const id = useId();
  const hintId = `${id}-hint`;
  const describedBy = error || hint ? hintId : undefined;

  return (
    <div className={cn("flex flex-col gap-1.5", className)}>
      <label
        htmlFor={id}
        className={cn(
          "text-sm font-medium text-fg",
          hideLabel && "sr-only",
        )}
      >
        {label}
        {required && (
          <span className="ml-0.5 text-danger" aria-hidden="true">
            *
          </span>
        )}
      </label>
      {children({
        id,
        "aria-describedby": describedBy,
        "aria-invalid": error ? true : undefined,
        required,
      })}
      {error ? (
        <p id={hintId} className="text-sm text-danger" role="alert">
          {error}
        </p>
      ) : hint ? (
        <p id={hintId} className="text-sm text-muted">
          {hint}
        </p>
      ) : null}
    </div>
  );
}

export interface RadioCardProps {
  selected: boolean;
  onSelect: () => void;
  title: ReactNode;
  description?: ReactNode;
  icon?: ReactNode;
  /** Optional pill rendered top-right (e.g. "Recommended"). */
  badge?: ReactNode;
  disabled?: boolean;
  className?: string;
}

// RadioCard is a large, clickable selection card (privacy mode, billing
// tier, storage backend…). It behaves as a radio: arrow/space/enter
// activate it and the selected state is announced via aria-checked.
//
// ACCESSIBILITY CONTRACT: this renders a single role="radio" cell and does
// NOT provide its own group. Consumers MUST wrap a set of RadioCards in a
// container with role="radiogroup" and an accessible name, e.g.
//
//   <div role="radiogroup" aria-label="Privacy mode">
//     <RadioCard selected={…} onSelect={…} title="Managed" />
//     <RadioCard selected={…} onSelect={…} title="Strict ZK" />
//   </div>
//
// Without a radiogroup parent, assistive tech may misrepresent the widget.
export function RadioCard({
  selected,
  onSelect,
  title,
  description,
  icon,
  badge,
  disabled,
  className,
}: RadioCardProps) {
  return (
    <button
      type="button"
      role="radio"
      aria-checked={selected}
      disabled={disabled}
      onClick={onSelect}
      className={cn(
        "relative flex w-full items-start gap-3 rounded-card border p-4 text-left transition-colors",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg",
        "disabled:opacity-60 disabled:pointer-events-none",
        selected
          ? "border-brand bg-brand/5 shadow-sm"
          : "border-border bg-surface hover:border-brand/40 hover:bg-surface-2",
        className,
      )}
    >
      {icon && (
        <span
          className={cn(
            "mt-0.5 flex h-9 w-9 shrink-0 items-center justify-center rounded-lg",
            selected ? "bg-brand text-brand-fg" : "bg-surface-2 text-muted",
          )}
          aria-hidden="true"
        >
          {icon}
        </span>
      )}
      <span className="min-w-0 flex-1">
        <span className="flex items-center gap-2">
          <span className="font-semibold text-fg">{title}</span>
          {badge && (
            <span className="rounded-full bg-brand/10 px-2 py-0.5 text-xs font-medium text-brand">
              {badge}
            </span>
          )}
        </span>
        {description && (
          <span className="mt-1 block text-sm text-muted">{description}</span>
        )}
      </span>
      <span
        className={cn(
          "mt-1 flex h-4 w-4 shrink-0 items-center justify-center rounded-full border",
          selected ? "border-brand" : "border-border",
        )}
        aria-hidden="true"
      >
        {selected && <span className="h-2 w-2 rounded-full bg-brand" />}
      </span>
    </button>
  );
}
