import { type ReactNode } from "react";
import { Upload, FolderPlus, UserPlus, type LucideIcon } from "lucide-react";
import { cn } from "../lib/cn";

export interface OnboardingAction {
  icon: LucideIcon;
  title: string;
  description: string;
  onClick: () => void;
  /** Hidden when false — used to drop "Invite a teammate" on solo tiers. */
  show?: boolean;
}

interface OnboardingEmptyStateProps {
  workspaceName?: string;
  onUpload: () => void;
  onCreateFolder: () => void;
  /** Omitted when the workspace can't invite (e.g. feature-gated). */
  onInvite?: () => void;
  className?: string;
}

function ActionCard({ icon: Icon, title, description, onClick }: OnboardingAction) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "group flex flex-col items-start gap-2 rounded-card border border-border bg-surface p-4 text-left",
        "transition-colors hover:border-brand hover:bg-surface-2",
        "focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
      )}
    >
      <span className="flex h-10 w-10 items-center justify-center rounded-lg bg-brand/10 text-brand">
        <Icon className="h-5 w-5" aria-hidden="true" />
      </span>
      <span className="font-semibold text-fg">{title}</span>
      <span className="text-sm text-muted">{description}</span>
    </button>
  );
}

// OnboardingEmptyState is the first-run experience shown when a workspace
// has no files or folders yet. It offers the three canonical next steps as
// action cards (4.3). "Invite a teammate" is omitted when onInvite is not
// provided (e.g. the workspace is a solo B2C account or the seat is
// feature-gated).
export function OnboardingEmptyState({
  workspaceName,
  onUpload,
  onCreateFolder,
  onInvite,
  className,
}: OnboardingEmptyStateProps): ReactNode {
  const actions: OnboardingAction[] = [
    {
      icon: Upload,
      title: "Upload your first file",
      description: "Drag a file in or browse — it's encrypted before it leaves your device.",
      onClick: onUpload,
    },
    {
      icon: FolderPlus,
      title: "Create a folder",
      description: "Organise your workspace with folders and sub-folders.",
      onClick: onCreateFolder,
    },
  ];
  if (onInvite) {
    actions.push({
      icon: UserPlus,
      title: "Invite a teammate",
      description: "Share securely and collaborate with your team.",
      onClick: onInvite,
    });
  }

  return (
    <div className={cn("mx-auto w-full max-w-3xl px-4 py-12", className)}>
      <div className="mb-8 text-center">
        <h2 className="text-2xl font-bold text-fg">
          {workspaceName ? `Welcome to ${workspaceName}` : "Welcome to ZK Drive"}
        </h2>
        <p className="mt-2 text-muted">
          Your private, end-to-end encrypted workspace. Get started in one click.
        </p>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {actions.map((a) => (
          <ActionCard key={a.title} {...a} />
        ))}
      </div>
    </div>
  );
}
