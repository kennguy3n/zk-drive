import { useState, type ReactNode } from "react";
import { useTranslation } from "react-i18next";
import {
  ChevronDown,
  Copy,
  Eye,
  Link2,
  Mail,
  MessageSquare,
  Pencil,
} from "lucide-react";
import {
  createGuestInvite,
  createShareLink,
  type FileItem,
  type Folder,
  type ShareLink,
  type GuestInvite,
} from "../api/client";
import { translateApiError } from "../api/errors";
import {
  Badge,
  Button,
  Field,
  Input,
  Modal,
  RadioCard,
  Tabs,
  useToast,
  type TabItem,
} from "./ui";
import { cn } from "../lib/cn";

// ShareDialog is the single entry point for sharing a file or folder.
// It intentionally keeps both share-link and guest-invite flows in one
// modal because from the end-user's perspective these are two
// renderings of the same intent ("give this resource to someone") and
// switching modals mid-flow is jarring. The default path stays simple —
// pick a permission, create a link, copy it — while password / expiry /
// download-cap controls are progressively disclosed so they never clutter
// the common case.
interface Props {
  resource:
    | { type: "folder"; value: Folder }
    | { type: "file"; value: FileItem };
  onClose: () => void;
}

type Role = "viewer" | "commenter" | "editor";
type Tab = "link" | "invite";

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;

export default function ShareDialog({ resource, onClose }: Props) {
  const { t } = useTranslation();
  const toast = useToast();
  const [tab, setTab] = useState<Tab>("link");
  const [error, setError] = useState<string | null>(null);
  const [link, setLink] = useState<ShareLink | null>(null);
  const [invite, setInvite] = useState<GuestInvite | null>(null);

  // Share link form state
  const [linkRole, setLinkRole] = useState<Role>("viewer");
  const [linkPassword, setLinkPassword] = useState("");
  const [linkExpiresAt, setLinkExpiresAt] = useState("");
  const [linkMaxDownloads, setLinkMaxDownloads] = useState("");
  const [linkAdvanced, setLinkAdvanced] = useState(false);
  const [maxDownloadsError, setMaxDownloadsError] = useState<string | null>(null);
  const [linkSubmitting, setLinkSubmitting] = useState(false);

  // Guest invite form state
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState<Role>("viewer");
  const [inviteExpiresAt, setInviteExpiresAt] = useState("");
  const [inviteAdvanced, setInviteAdvanced] = useState(false);
  const [emailError, setEmailError] = useState<string | null>(null);
  const [inviteSubmitting, setInviteSubmitting] = useState(false);

  const kind =
    resource.type === "folder" ? t("search.typeFolder") : t("search.typeFile");

  const switchTab = (next: Tab) => {
    setTab(next);
    setError(null);
  };

  const submitLink = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    let maxDownloads: number | undefined;
    if (linkMaxDownloads.trim()) {
      const parsed = Number.parseInt(linkMaxDownloads, 10);
      if (!Number.isFinite(parsed) || parsed < 1) {
        setMaxDownloadsError(t("share.maxDownloadsError"));
        setLinkAdvanced(true);
        return;
      }
      maxDownloads = parsed;
    }
    setMaxDownloadsError(null);

    setLinkSubmitting(true);
    try {
      const created = await createShareLink({
        resource_type: resource.type,
        resource_id: resource.value.id,
        role: linkRole,
        password: linkPassword || undefined,
        // datetime-local inputs give "YYYY-MM-DDTHH:mm" without a
        // timezone; rely on the backend's permissive RFC3339 parser to
        // treat these as local-time ISO strings.
        expires_at: linkExpiresAt || undefined,
        max_downloads: maxDownloads,
      });
      setLink(created);
    } catch (err) {
      setError(translateApiError(err, t));
    } finally {
      setLinkSubmitting(false);
    }
  };

  // Guest invites are always folder-scoped on the backend
  // (internal/sharing/models.go GuestInvite.FolderID). When the current
  // resource is a file, fall back to its parent folder so the invite
  // grants access to the directory that contains the file. If the file
  // is somehow unparented (shouldn't happen for normal user-owned
  // files) we surface a clear error instead of silently sending an
  // invalid request.
  const inviteFolderID: string | null =
    resource.type === "folder"
      ? resource.value.id
      : resource.value.folder_id ?? null;

  const submitInvite = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);

    if (!EMAIL_RE.test(inviteEmail.trim())) {
      setEmailError(t("share.emailInvalid"));
      return;
    }
    setEmailError(null);

    if (!inviteFolderID) {
      setError(t("share.fileNoParent"));
      return;
    }

    setInviteSubmitting(true);
    try {
      const created = await createGuestInvite({
        folder_id: inviteFolderID,
        email: inviteEmail.trim(),
        role: inviteRole,
        expires_at: inviteExpiresAt || undefined,
      });
      setInvite(created);
    } catch (err) {
      setError(translateApiError(err, t));
    } finally {
      setInviteSubmitting(false);
    }
  };

  const copyLink = async (url: string) => {
    try {
      await navigator.clipboard.writeText(url);
      toast.success(t("share.copied"));
    } catch {
      toast.error(t("share.copyFailed"));
    }
  };

  const tabs: TabItem<Tab>[] = [
    { value: "link", label: t("share.tabLink"), icon: <Link2 className="h-4 w-4" /> },
    { value: "invite", label: t("share.tabInvite"), icon: <Mail className="h-4 w-4" /> },
  ];

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t("share.headingFor", { kind })}
      description={resource.value.name}
      size="md"
      className="max-h-[88vh] overflow-y-auto"
    >
      <div className="flex flex-col gap-5">
        <Tabs
          tabs={tabs}
          value={tab}
          onChange={switchTab}
          variant="pill"
          aria-label={t("share.tabsAria")}
        />

        <PrivacyNote resource={resource} />

        {error && (
          <Callout tone="danger" role="alert">
            {error}
          </Callout>
        )}

        {tab === "link" ? (
          <form onSubmit={submitLink} className="flex flex-col gap-5">
            <RoleSelector value={linkRole} onChange={setLinkRole} label={t("share.role")} />

            <div className="flex flex-col gap-3">
              <button
                type="button"
                onClick={() => setLinkAdvanced((v) => !v)}
                aria-expanded={linkAdvanced}
                className="inline-flex items-center gap-1.5 self-start text-sm font-medium text-brand hover:text-brand-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg rounded"
              >
                <ChevronDown
                  className={cn(
                    "h-4 w-4 transition-transform",
                    linkAdvanced && "rotate-180",
                  )}
                  aria-hidden="true"
                />
                {linkAdvanced ? t("share.fewerOptions") : t("share.moreOptions")}
              </button>

              {linkAdvanced && (
                <div className="flex flex-col gap-4 rounded-card border border-border bg-surface-2/50 p-4">
                  <Field label={t("share.passwordOptional")} hint={t("share.passwordHint")}>
                    {(p) => (
                      <Input
                        {...p}
                        type="password"
                        autoComplete="new-password"
                        value={linkPassword}
                        onChange={(e) => setLinkPassword(e.target.value)}
                        placeholder={t("share.passwordPlaceholder")}
                      />
                    )}
                  </Field>
                  <Field label={t("share.expiresOptional")}>
                    {(p) => (
                      <Input
                        {...p}
                        type="datetime-local"
                        value={linkExpiresAt}
                        onChange={(e) => setLinkExpiresAt(e.target.value)}
                      />
                    )}
                  </Field>
                  <Field
                    label={t("share.maxDownloadsOptional")}
                    error={maxDownloadsError ?? undefined}
                  >
                    {(p) => (
                      <Input
                        {...p}
                        type="number"
                        min={1}
                        value={linkMaxDownloads}
                        onChange={(e) => {
                          setLinkMaxDownloads(e.target.value);
                          if (maxDownloadsError) setMaxDownloadsError(null);
                        }}
                        placeholder={t("share.unlimitedPlaceholder")}
                      />
                    )}
                  </Field>
                </div>
              )}
            </div>

            <Button
              type="submit"
              variant="gradient"
              size="lg"
              loading={linkSubmitting}
              className="w-full"
            >
              <Link2 className="h-4 w-4" aria-hidden="true" />
              {t("share.createLink")}
            </Button>

            {link && <ShareLinkCard link={link} onCopy={copyLink} />}
          </form>
        ) : (
          <form onSubmit={submitInvite} className="flex flex-col gap-5">
            <Field
              label={t("share.emailLabel")}
              error={emailError ?? undefined}
              required
            >
              {(p) => (
                <Input
                  {...p}
                  type="email"
                  value={inviteEmail}
                  onChange={(e) => {
                    setInviteEmail(e.target.value);
                    if (emailError) setEmailError(null);
                  }}
                  placeholder={t("share.emailPlaceholder")}
                />
              )}
            </Field>

            <RoleSelector value={inviteRole} onChange={setInviteRole} label={t("share.role")} />

            <div className="flex flex-col gap-3">
              <button
                type="button"
                onClick={() => setInviteAdvanced((v) => !v)}
                aria-expanded={inviteAdvanced}
                className="inline-flex items-center gap-1.5 self-start text-sm font-medium text-brand hover:text-brand-hover focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring focus-visible:ring-offset-2 focus-visible:ring-offset-bg rounded"
              >
                <ChevronDown
                  className={cn(
                    "h-4 w-4 transition-transform",
                    inviteAdvanced && "rotate-180",
                  )}
                  aria-hidden="true"
                />
                {inviteAdvanced ? t("share.fewerOptions") : t("share.moreOptions")}
              </button>

              {inviteAdvanced && (
                <div className="rounded-card border border-border bg-surface-2/50 p-4">
                  <Field label={t("share.expiresOptional")}>
                    {(p) => (
                      <Input
                        {...p}
                        type="datetime-local"
                        value={inviteExpiresAt}
                        onChange={(e) => setInviteExpiresAt(e.target.value)}
                      />
                    )}
                  </Field>
                </div>
              )}
            </div>

            <Button
              type="submit"
              variant="gradient"
              size="lg"
              loading={inviteSubmitting}
              className="w-full"
            >
              <Mail className="h-4 w-4" aria-hidden="true" />
              {t("share.sendInvite")}
            </Button>

            {invite && <GuestInviteCard invite={invite} />}
          </form>
        )}
      </div>
    </Modal>
  );
}

const ROLE_ICONS: Record<Role, ReactNode> = {
  viewer: <Eye className="h-5 w-5" />,
  commenter: <MessageSquare className="h-5 w-5" />,
  editor: <Pencil className="h-5 w-5" />,
};

function RoleSelector({
  value,
  onChange,
  label,
}: {
  value: Role;
  onChange: (role: Role) => void;
  label: string;
}) {
  const { t } = useTranslation();
  const roles: { role: Role; title: string; description: string }[] = [
    { role: "viewer", title: t("share.roleViewer"), description: t("share.roleViewerDesc") },
    {
      role: "commenter",
      title: t("share.roleCommenter"),
      description: t("share.roleCommenterDesc"),
    },
    { role: "editor", title: t("share.roleEditor"), description: t("share.roleEditorDesc") },
  ];
  return (
    <div className="flex flex-col gap-1.5">
      <span className="text-sm font-medium text-fg" id="share-role-label">
        {label}
      </span>
      <div
        role="radiogroup"
        aria-labelledby="share-role-label"
        className="flex flex-col gap-2"
      >
        {roles.map((r) => (
          <RadioCard
            key={r.role}
            selected={value === r.role}
            onSelect={() => onChange(r.role)}
            title={r.title}
            description={r.description}
            icon={ROLE_ICONS[r.role]}
          />
        ))}
      </div>
    </div>
  );
}

function ShareLinkCard({
  link,
  onCopy,
}: {
  link: ShareLink;
  onCopy: (url: string) => void;
}) {
  const { t } = useTranslation();
  const url = `${window.location.origin}/share/${link.token}`;
  return (
    <div className="flex flex-col gap-3 rounded-card border border-success/30 bg-success/10 p-4">
      <div className="flex items-center gap-2 text-sm font-semibold text-success">
        <Link2 className="h-4 w-4" aria-hidden="true" />
        {t("share.linkCreated")}
      </div>

      <div className="flex gap-2">
        <Input
          readOnly
          value={url}
          aria-label={t("share.linkUrlLabel")}
          onFocus={(e) => e.currentTarget.select()}
          className="font-mono text-xs"
        />
        <Button
          type="button"
          variant="primary"
          onClick={() => onCopy(url)}
          className="shrink-0"
        >
          <Copy className="h-4 w-4" aria-hidden="true" />
          {t("share.linkCopy")}
        </Button>
      </div>

      <div className="flex flex-wrap gap-1.5">
        <Badge tone="brand">{t(`share.role${capitalize(link.role)}`)}</Badge>
        <Badge tone={link.expires_at ? "warning" : "neutral"}>
          {link.expires_at
            ? t("share.expiresBadge", { date: formatDate(link.expires_at) })
            : t("share.noExpiryBadge")}
        </Badge>
        {link.password_protected && (
          <Badge tone="neutral">{t("share.passwordProtectedBadge")}</Badge>
        )}
        {link.max_downloads != null && (
          <Badge tone="neutral">
            {t("share.maxDownloadsBadge", { count: link.max_downloads })}
          </Badge>
        )}
      </div>
    </div>
  );
}

function GuestInviteCard({ invite }: { invite: GuestInvite }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col gap-2 rounded-card border border-success/30 bg-success/10 p-4">
      <div className="flex items-center gap-2 text-sm font-semibold text-success">
        <Mail className="h-4 w-4" aria-hidden="true" />
        {t("share.inviteSent")}
      </div>
      <p className="text-sm text-fg">
        <strong className="font-semibold">{invite.email}</strong>{" "}
        <span className="text-muted">{t("share.as")}</span>{" "}
        <Badge tone="brand">{t(`share.role${capitalize(invite.role)}`)}</Badge>
      </p>
    </div>
  );
}

// PrivacyNote explains, in plain language, what sharing means under the
// resource's privacy mode — most importantly that a zero-knowledge
// (strict_zk) folder can't be server-previewed for recipients.
function PrivacyNote({ resource }: { resource: Props["resource"] }) {
  const { t } = useTranslation();
  if (resource.type === "file") {
    return <Callout tone="info">{t("share.privacyFileNote")}</Callout>;
  }
  const isStrict = resource.value.encryption_mode === "strict_zk";
  return (
    <Callout tone={isStrict ? "warning" : "info"}>
      {isStrict ? t("share.privacyStrictNote") : t("share.privacyConfidentialNote")}
    </Callout>
  );
}

function Callout({
  tone,
  role,
  children,
}: {
  tone: "info" | "warning" | "danger";
  role?: string;
  children: ReactNode;
}) {
  const tones: Record<typeof tone, string> = {
    info: "border-border bg-surface-2 text-muted",
    warning: "border-warning/30 bg-warning/10 text-fg",
    danger: "border-danger/30 bg-danger/10 text-danger",
  };
  return (
    <div role={role} className={cn("rounded-card border px-3 py-2.5 text-sm", tones[tone])}>
      {children}
    </div>
  );
}

function capitalize(s: string): string {
  return s.charAt(0).toUpperCase() + s.slice(1);
}

function formatDate(iso: string): string {
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? iso : d.toLocaleDateString();
}
