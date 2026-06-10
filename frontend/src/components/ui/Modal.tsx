import * as Dialog from "@radix-ui/react-dialog";
import { X } from "lucide-react";
import { type ReactNode } from "react";
import { cn } from "../../lib/cn";

export interface ModalProps {
  open: boolean;
  onOpenChange: (open: boolean) => void;
  title: ReactNode;
  /** Optional sub-heading rendered under the title. */
  description?: ReactNode;
  children: ReactNode;
  /** Footer action row, right-aligned. */
  footer?: ReactNode;
  /** Width preset. Defaults to "md". */
  size?: "sm" | "md" | "lg" | "xl";
  className?: string;
}

const sizes = {
  sm: "max-w-sm",
  md: "max-w-md",
  lg: "max-w-lg",
  xl: "max-w-2xl",
};

// Modal wraps Radix Dialog, which gives us focus trapping, focus
// restoration on close, Escape-to-close, scroll locking and an
// aria-labelledby/-describedby wired to the title/description for free —
// satisfying the 4.6 focus-management + screen-reader requirements.
export function Modal({
  open,
  onOpenChange,
  title,
  description,
  children,
  footer,
  size = "md",
  className,
}: ModalProps) {
  return (
    <Dialog.Root open={open} onOpenChange={onOpenChange}>
      <Dialog.Portal>
        <Dialog.Overlay className="fixed inset-0 z-50 bg-black/50 backdrop-blur-sm animate-fade-in" />
        <Dialog.Content
          className={cn(
            "fixed left-1/2 top-1/2 z-50 w-[calc(100vw-2rem)] -translate-x-1/2 -translate-y-1/2",
            "rounded-card border border-border bg-overlay p-5 text-fg shadow-overlay",
            "animate-scale-in focus:outline-none",
            sizes[size],
            className,
          )}
        >
          <div className="mb-4 flex items-start justify-between gap-4">
            <div className="min-w-0">
              <Dialog.Title className="text-lg font-semibold text-fg">
                {title}
              </Dialog.Title>
              {description && (
                <Dialog.Description className="mt-1 text-sm text-muted">
                  {description}
                </Dialog.Description>
              )}
            </div>
            <Dialog.Close
              aria-label="Close dialog"
              className="-mr-1 -mt-1 shrink-0 rounded-lg p-1.5 text-muted hover:bg-surface-2 hover:text-fg"
            >
              <X className="h-5 w-5" aria-hidden="true" />
            </Dialog.Close>
          </div>
          <div className="text-sm">{children}</div>
          {footer && (
            <div className="mt-5 flex items-center justify-end gap-2">{footer}</div>
          )}
        </Dialog.Content>
      </Dialog.Portal>
    </Dialog.Root>
  );
}
