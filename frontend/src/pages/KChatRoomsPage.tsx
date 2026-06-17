import { useCallback, useEffect, useState, type ReactNode } from "react";
import { Link, NavLink, useNavigate } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  CreditCard,
  KeyRound,
  LogOut,
  MapPin,
  MessagesSquare,
  Plus,
  RefreshCw,
  Trash2,
  Users as UsersIcon,
  UserPlus,
  type LucideIcon,
} from "lucide-react";
import {
  createKChatRoom,
  deleteKChatRoom,
  fetchKChatRooms,
  fetchUsers,
  syncKChatMembers,
  type AdminUser,
  type KChatMemberSync,
  type KChatRoom,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";
import { useFeatures } from "../hooks/useFeatures";
import { Feature } from "../features/featureKeys";
import {
  AppShell,
  Badge,
  Button,
  EmptyState,
  Field,
  Input,
  Modal,
  PageHeader,
  Select,
  Skeleton,
  Table,
  TBody,
  Td,
  Th,
  THead,
  Tr,
  useConfirm,
  useToast,
} from "../components/ui";
import { ThemeToggle } from "../components/ThemeToggle";
import { cn } from "../lib/cn";

// KChatRoomsPage maps KChat rooms to zk-drive folders and lets admins push a
// complete membership snapshot to a room. Every action is wired to the real
// /kchat API; the native confirm() and hand-rolled overlay dialog from the
// legacy screen are replaced with useConfirm and the shared Modal.
export default function KChatRoomsPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
  const toast = useToast();
  const confirm = useConfirm();

  const [rooms, setRooms] = useState<KChatRoom[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const [newRoomID, setNewRoomID] = useState("");
  const [creating, setCreating] = useState(false);
  const [deletingId, setDeletingId] = useState<string | null>(null);
  const [syncTarget, setSyncTarget] = useState<KChatRoom | null>(null);

  const refresh = useCallback(async () => {
    setLoading(true);
    try {
      setRooms(await fetchKChatRooms());
      setError(null);
    } catch (e) {
      setError(translateApiError(e, t));
    } finally {
      setLoading(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (isAdmin) refresh();
  }, [isAdmin, refresh]);

  if (!isAdmin) {
    return <AccessDenied />;
  }

  const create = async (e: React.FormEvent) => {
    e.preventDefault();
    const id = newRoomID.trim();
    if (!id || creating) return;
    setCreating(true);
    try {
      await createKChatRoom(id);
      toast.success(t("kchat.roomCreated"));
      setNewRoomID("");
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t));
    } finally {
      setCreating(false);
    }
  };

  const remove = async (room: KChatRoom) => {
    const ok = await confirm({
      title: t("kchat.deleteRoomTitle"),
      description: t("kchat.deleteMappingPrompt", { id: room.kchat_room_id }),
      confirmLabel: t("common.delete"),
      tone: "danger",
    });
    if (!ok) return;
    setDeletingId(room.id);
    try {
      await deleteKChatRoom(room.id);
      toast.success(t("kchat.roomDeleted"));
      await refresh();
    } catch (err) {
      toast.error(translateApiError(err, t));
    } finally {
      setDeletingId(null);
    }
  };

  return (
    <AdminShell active="kchat">
      <PageHeader
        title={t("kchat.title")}
        description={t("kchat.pageDescription")}
        actions={
          <Button variant="ghost" size="sm" onClick={refresh} disabled={loading} aria-label={t("admin.refresh")}>
            <RefreshCw className={cn("h-4 w-4", loading && "animate-spin")} aria-hidden />
            <span className="hidden sm:inline">{t("admin.refresh")}</span>
          </Button>
        }
      />

      {error && <ErrorBanner message={error} />}

      <div className="flex flex-col gap-6">
        <Panel>
          <SectionHeading
            title={t("kchat.createHeading")}
            description={t("kchat.createDescription")}
          />
          <form onSubmit={create} className="flex flex-col gap-3 sm:flex-row sm:items-end">
            <Field label={t("kchat.roomIdLabel")} className="flex-1">
              {(p) => (
                <Input
                  {...p}
                  value={newRoomID}
                  onChange={(e) => setNewRoomID(e.target.value)}
                  placeholder={t("kchat.roomIdPlaceholder")}
                  autoComplete="off"
                />
              )}
            </Field>
            <Button type="submit" loading={creating} disabled={!newRoomID.trim()}>
              <Plus className="h-4 w-4" aria-hidden />
              {t("kchat.createRoom")}
            </Button>
          </form>
        </Panel>

        <Panel>
          <SectionHeading title={t("kchat.roomsHeading")} />
          {loading ? (
            <RoomsTableSkeleton />
          ) : rooms.length === 0 ? (
            <EmptyState
              icon={<MessagesSquare className="h-6 w-6" aria-hidden />}
              title={t("kchat.noRooms")}
              description={t("kchat.noRoomsDescription")}
            />
          ) : (
            <Table>
              <THead>
                <Tr>
                  <Th>{t("kchat.roomIdColumn")}</Th>
                  <Th>{t("kchat.folderIdColumn")}</Th>
                  <Th>{t("kchat.createdAtColumn")}</Th>
                  <Th>
                    <span className="block text-right">{t("common.actions")}</span>
                  </Th>
                </Tr>
              </THead>
              <TBody>
                {rooms.map((room) => (
                  <Tr key={room.id}>
                    <Td className="font-medium">
                      <span className="inline-flex items-center gap-2">
                        <Badge tone="brand">{room.kchat_room_id}</Badge>
                      </span>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <span className="font-mono text-xs text-muted">{room.folder_id}</span>
                    </Td>
                    <Td className="whitespace-nowrap">
                      <span className="text-muted">{formatDate(room.created_at)}</span>
                    </Td>
                    <Td>
                      <div className="flex items-center justify-end gap-2">
                        <Button variant="secondary" size="sm" onClick={() => setSyncTarget(room)}>
                          <UserPlus className="h-4 w-4" aria-hidden />
                          {t("kchat.sync")}
                        </Button>
                        <Button
                          variant="ghost"
                          size="sm"
                          onClick={() => remove(room)}
                          loading={deletingId === room.id}
                          disabled={deletingId === room.id}
                        >
                          <span className="inline-flex items-center gap-1.5 text-danger">
                            <Trash2 className="h-4 w-4" aria-hidden />
                            {t("common.delete")}
                          </span>
                        </Button>
                      </div>
                    </Td>
                  </Tr>
                ))}
              </TBody>
            </Table>
          )}
        </Panel>
      </div>

      {syncTarget && (
        <SyncMembersModal
          room={syncTarget}
          onClose={() => setSyncTarget(null)}
        />
      )}
    </AdminShell>
  );
}

// --- Sync members modal -------------------------------------------------

interface MemberDraft {
  user_id: string;
  role: string;
}

const MEMBER_ROLES = ["viewer", "editor", "admin"] as const;

// SyncMembersModal collects a complete membership snapshot for a room and
// posts it to /kchat/rooms/:id/sync-members. Users are chosen from the
// workspace directory (no more pasting raw UUIDs), satisfying the audit's
// "replace type-a-UUID flows with a picker" finding.
function SyncMembersModal({ room, onClose }: { room: KChatRoom; onClose: () => void }) {
  const { t } = useTranslation();
  const toast = useToast();
  const [users, setUsers] = useState<AdminUser[]>([]);
  const [loadingUsers, setLoadingUsers] = useState(true);
  const [usersError, setUsersError] = useState<string | null>(null);
  const [members, setMembers] = useState<MemberDraft[]>([{ user_id: "", role: "viewer" }]);
  const [syncing, setSyncing] = useState(false);

  const loadUsers = useCallback(async () => {
    setLoadingUsers(true);
    setUsersError(null);
    try {
      const list = await fetchUsers();
      setUsers(list.filter((u) => !u.deactivated_at));
    } catch (e) {
      setUsersError(translateApiError(e, t));
    } finally {
      setLoadingUsers(false);
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    loadUsers();
  }, [loadUsers]);

  const addRow = () => setMembers((prev) => [...prev, { user_id: "", role: "viewer" }]);
  const removeRow = (index: number) =>
    setMembers((prev) => (prev.length === 1 ? prev : prev.filter((_, i) => i !== index)));
  const setRow = (index: number, patch: Partial<MemberDraft>) =>
    setMembers((prev) => prev.map((m, i) => (i === index ? { ...m, ...patch } : m)));

  const usedIds = new Set(members.map((m) => m.user_id).filter(Boolean));

  const sync = async () => {
    const payload: KChatMemberSync[] = members
      .filter((m) => m.user_id)
      .map((m) => ({ user_id: m.user_id, role: m.role }));
    setSyncing(true);
    try {
      const r = await syncKChatMembers(room.id, payload);
      toast.success(t("kchat.syncedCount", { count: r.synced }));
      onClose();
    } catch (e) {
      toast.error(translateApiError(e, t));
    } finally {
      setSyncing(false);
    }
  };

  return (
    <Modal
      open
      onOpenChange={(o) => !o && onClose()}
      title={t("kchat.syncTitle", { id: room.kchat_room_id })}
      description={t("kchat.syncDescription")}
      size="lg"
      footer={
        <>
          <Button variant="ghost" onClick={onClose} disabled={syncing}>
            {t("common.cancel")}
          </Button>
          <Button onClick={sync} loading={syncing} disabled={loadingUsers || !!usersError}>
            {t("kchat.sync")}
          </Button>
        </>
      }
    >
      {loadingUsers ? (
        <div className="flex flex-col gap-2">
          <Skeleton className="h-10 rounded-lg" />
          <Skeleton className="h-10 rounded-lg" />
        </div>
      ) : usersError ? (
        <div className="flex flex-col items-start gap-3">
          <ErrorBanner message={usersError} />
          <Button variant="secondary" size="sm" onClick={loadUsers}>
            {t("common.retry")}
          </Button>
        </div>
      ) : (
        <div className="flex flex-col gap-3">
          <div className="grid grid-cols-[1fr_10rem_auto] items-center gap-2 px-1 text-xs font-semibold uppercase tracking-wide text-muted">
            <span>{t("kchat.memberUser")}</span>
            <span>{t("share.role")}</span>
            <span className="sr-only">{t("common.actions")}</span>
          </div>
          {members.map((member, index) => (
            <div key={index} className="grid grid-cols-[1fr_10rem_auto] items-center gap-2">
              <Select
                aria-label={t("kchat.memberUser")}
                value={member.user_id}
                onChange={(e) => setRow(index, { user_id: e.target.value })}
              >
                <option value="">{t("kchat.selectUser")}</option>
                {users.map((u) => (
                  <option
                    key={u.id}
                    value={u.id}
                    disabled={member.user_id !== u.id && usedIds.has(u.id)}
                  >
                    {u.name ? `${u.name} (${u.email})` : u.email}
                  </option>
                ))}
              </Select>
              <Select
                aria-label={t("share.role")}
                value={member.role}
                onChange={(e) => setRow(index, { role: e.target.value })}
              >
                {MEMBER_ROLES.map((r) => (
                  <option key={r} value={r}>
                    {t(`share.role${r.charAt(0).toUpperCase()}${r.slice(1)}`)}
                  </option>
                ))}
              </Select>
              <Button
                type="button"
                variant="ghost"
                size="sm"
                onClick={() => removeRow(index)}
                disabled={members.length === 1}
                aria-label={t("kchat.removeMember")}
              >
                <Trash2 className="h-4 w-4" aria-hidden />
              </Button>
            </div>
          ))}
          <div>
            <Button type="button" variant="secondary" size="sm" onClick={addRow}>
              <Plus className="h-4 w-4" aria-hidden />
              {t("kchat.addRow")}
            </Button>
          </div>
        </div>
      )}
    </Modal>
  );
}

function RoomsTableSkeleton({ rows = 4 }: { rows?: number }) {
  const { t } = useTranslation();
  return (
    <div className="flex flex-col gap-2" aria-label={t("common.loading")} aria-busy="true">
      {Array.from({ length: rows }).map((_, i) => (
        <Skeleton key={i} className="h-12 rounded-lg" />
      ))}
    </div>
  );
}

function formatDate(value: string): string {
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? value : d.toLocaleString();
}

// --- Shared admin chrome ------------------------------------------------
// Kept local to this file per the workstream's "build new primitives
// locally" rule; AdminPage renders an equivalent shell.

type AdminSection = "admin" | "placement" | "encryption" | "kchat" | "billing";

function AdminShell({ active, children }: { active: AdminSection; children: ReactNode }) {
  const { t } = useTranslation();
  const { logout } = useAuth();
  const { isEnabled } = useFeatures();

  return (
    <AppShell
      brand={<AdminBrand />}
      nav={<AdminNav active={active} kchatEnabled={isEnabled(Feature.KChat)} />}
      actions={
        <>
          <Link
            to="/drive"
            className="hidden rounded-full px-3 py-1.5 text-sm font-medium text-muted transition-colors hover:bg-surface-2 hover:text-fg sm:inline-flex"
          >
            {t("admin.backToDrive")}
          </Link>
          <ThemeToggle />
          <Button variant="secondary" size="sm" onClick={logout}>
            <LogOut className="h-4 w-4" aria-hidden />
            <span className="hidden sm:inline">{t("auth.logout")}</span>
          </Button>
        </>
      }
    >
      {children}
    </AppShell>
  );
}

function AdminBrand() {
  const { t } = useTranslation();
  return (
    <Link
      to="/admin"
      className="flex items-center gap-2 rounded-lg focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
    >
      <span className="flex h-7 w-7 items-center justify-center rounded-lg bg-brand-gradient text-xs font-bold text-white">
        zk
      </span>
      <span className="text-sm font-semibold text-fg">{t("nav.admin")}</span>
    </Link>
  );
}

function AdminNav({ active, kchatEnabled }: { active: AdminSection; kchatEnabled: boolean }) {
  const { t } = useTranslation();
  const items: { id: AdminSection; to: string; icon: LucideIcon; label: string; show: boolean }[] = [
    { id: "admin", to: "/admin", icon: UsersIcon, label: t("nav.admin"), show: true },
    { id: "placement", to: "/admin/placement", icon: MapPin, label: t("admin.placement"), show: true },
    { id: "encryption", to: "/admin/encryption", icon: KeyRound, label: t("admin.encryption"), show: true },
    { id: "kchat", to: "/admin/kchat", icon: MessagesSquare, label: t("nav.kchatRooms"), show: kchatEnabled },
    { id: "billing", to: "/billing", icon: CreditCard, label: t("nav.billing"), show: true },
  ];
  return (
    <div className="flex min-w-0 items-center gap-1 overflow-x-auto">
      {items
        .filter((i) => i.show)
        .map((i) => (
          <NavLink
            key={i.id}
            to={i.to}
            aria-current={active === i.id ? "page" : undefined}
            className={cn(
              "inline-flex shrink-0 items-center gap-1.5 rounded-full px-3 py-1.5 text-sm font-medium transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring",
              active === i.id
                ? "bg-brand/10 text-brand"
                : "text-muted hover:bg-surface-2 hover:text-fg",
            )}
          >
            <i.icon className="h-4 w-4" aria-hidden />
            <span className="hidden md:inline">{i.label}</span>
          </NavLink>
        ))}
    </div>
  );
}

function AccessDenied() {
  const { t } = useTranslation();
  const navigate = useNavigate();
  return (
    <AppShell brand={<AdminBrand />} actions={<ThemeToggle />} maxWidth="md">
      <div className="mx-auto mt-10 max-w-md">
        <EmptyState
          icon={<MessagesSquare className="h-6 w-6" aria-hidden />}
          title={t("admin.adminOnly")}
          description={t("admin.adminOnlyDescription")}
          action={
            <Button variant="secondary" size="sm" onClick={() => navigate("/drive")}>
              {t("admin.backToDrive")}
            </Button>
          }
        />
      </div>
    </AppShell>
  );
}

function Panel({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <section className={cn("rounded-card border border-border bg-surface p-5 sm:p-6", className)}>
      {children}
    </section>
  );
}

function SectionHeading({
  title,
  description,
  actions,
}: {
  title: string;
  description?: string;
  actions?: ReactNode;
}) {
  return (
    <div className="mb-5 flex flex-col gap-2 sm:flex-row sm:items-start sm:justify-between">
      <div className="min-w-0">
        <h2 className="text-lg font-semibold text-fg">{title}</h2>
        {description && <p className="mt-1 max-w-2xl text-sm text-muted">{description}</p>}
      </div>
      {actions && <div className="flex shrink-0 flex-wrap items-center gap-2">{actions}</div>}
    </div>
  );
}

function ErrorBanner({ message }: { message: string }) {
  return (
    <div
      role="alert"
      className="mb-4 rounded-lg border border-danger/30 bg-danger/10 px-4 py-3 text-sm text-danger"
    >
      {message}
    </div>
  );
}
