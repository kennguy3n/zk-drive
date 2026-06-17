import { type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { Upload, FolderPlus, UserPlus, ShieldCheck, MousePointerClick, type LucideIcon } from "lucide-react";
import { cn } from "../lib/cn";

export interface OnboardingAction {
  icon: LucideIcon;
  title: string;
  description: string;
  onClick: () => void;
  /** Renders the card as the primary, brand-filled call to action. */
  primary?: boolean;
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

function ActionCard({ icon: Icon, title, description, onClick, primary }: OnboardingAction) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={cn(
        "group flex flex-col items-start gap-2 rounded-card border p-4 text-left",
        "transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg",
        primary
          ? "border-transparent bg-brand-gradient text-white shadow-glow hover:opacity-95"
          : "border-border bg-surface hover:border-brand hover:bg-surface-2",
      )}
    >
      <span
        className={cn(
          "flex h-10 w-10 items-center justify-center rounded-lg",
          primary ? "bg-white/20 text-white" : "bg-brand/10 text-brand",
        )}
      >
        <Icon className="h-5 w-5" aria-hidden="true" />
      </span>
      <span className={cn("font-semibold", primary ? "text-white" : "text-fg")}>{title}</span>
      <span className={cn("text-sm", primary ? "text-white/90" : "text-muted")}>{description}</span>
    </button>
  );
}

// OnboardingEmptyState is the first-run experience shown when a workspace
// has no files or folders yet. It offers the three canonical next steps as
// action cards (4.3), led by a brand-filled "Upload your first file" CTA.
// "Invite a teammate" is omitted when onInvite is not provided (e.g. the
// workspace is a solo B2C account or the seat is feature-gated).
export function OnboardingEmptyState({
  workspaceName,
  onUpload,
  onCreateFolder,
  onInvite,
  className,
}: OnboardingEmptyStateProps): ReactNode {
  const { t } = useTranslation();
  const actions: OnboardingAction[] = [
    {
      icon: Upload,
      title: t("drive.onboardingUploadTitle"),
      description: t("drive.onboardingUploadDesc"),
      onClick: onUpload,
      primary: true,
    },
    {
      icon: FolderPlus,
      title: t("drive.onboardingCreateTitle"),
      description: t("drive.onboardingCreateDesc"),
      onClick: onCreateFolder,
    },
  ];
  if (onInvite) {
    actions.push({
      icon: UserPlus,
      title: t("drive.onboardingInviteTitle"),
      description: t("drive.onboardingInviteDesc"),
      onClick: onInvite,
    });
  }

  return (
    <div className={cn("mx-auto w-full max-w-3xl px-4 py-12", className)}>
      <div className="mb-8 flex flex-col items-center text-center">
        <span className="mb-4 flex h-14 w-14 items-center justify-center rounded-2xl bg-brand/10 text-brand">
          <ShieldCheck className="h-7 w-7" aria-hidden="true" />
        </span>
        <h2 className="text-2xl font-bold tracking-tight text-fg">
          {workspaceName
            ? t("drive.onboardingWelcome", { workspaceName })
            : t("drive.onboardingWelcomeDefault")}
        </h2>
        <p className="mt-2 max-w-md text-muted">{t("drive.onboardingSubtitle")}</p>
      </div>
      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3">
        {actions.map((a) => (
          <ActionCard key={a.title} {...a} />
        ))}
      </div>
      <p className="mt-6 flex items-center justify-center gap-2 text-sm text-muted">
        <MousePointerClick className="h-4 w-4" aria-hidden="true" />
        {t("drive.onboardingDragHint")}
      </p>
    </div>
  );
}
