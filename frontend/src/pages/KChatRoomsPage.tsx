import { useCallback, useEffect, useState } from "react";
import { Link } from "react-router-dom";
import { useTranslation } from "react-i18next";
import {
  createKChatRoom,
  deleteKChatRoom,
  fetchKChatRooms,
  syncKChatMembers,
  type KChatRoom,
} from "../api/client";
import { translateApiError } from "../api/errors";
import { useAuth } from "../hooks/useAuth";

// KChatRoomsPage lets admins manage KChat room → folder mappings:
// create a new mapping (provisions a folder), delete an existing
// mapping, and sync the room's member roster against the backing
// folder's permission grants.
export default function KChatRoomsPage() {
  const { isAdmin } = useAuth();
  const { t } = useTranslation();
  const [rooms, setRooms] = useState<KChatRoom[]>([]);
  const [error, setError] = useState<string | null>(null);
  const [newRoomID, setNewRoomID] = useState("");
  const [syncTarget, setSyncTarget] = useState<KChatRoom | null>(null);

  const refresh = useCallback(async () => {
    setError(null);
    try {
      setRooms(await fetchKChatRooms());
    } catch (e) {
      setError(translateApiError(e, t));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, []);

  useEffect(() => {
    if (isAdmin) refresh();
  }, [isAdmin, refresh]);

  if (!isAdmin) {
    return (
      <div style={{ padding: 32 }}>
        <h2>{t("admin.adminOnly")}</h2>
        <p>
          {t("admin.adminOnlyDescription")} <Link to="/drive">{t("admin.backToDrive")}</Link>
        </p>
      </div>
    );
  }

  return (
    <div style={{ padding: 24 }}>
      <header
        style={{
          display: "flex",
          alignItems: "center",
          justifyContent: "space-between",
          marginBottom: 16,
        }}
      >
        <h1 style={{ margin: 0 }}>{t("kchat.title")}</h1>
        <Link to="/admin">{t("admin.backToAdmin")}</Link>
      </header>
      {error ? <p style={{ color: "#b91c1c" }}>{error}</p> : null}

      <form
        onSubmit={async (e) => {
          e.preventDefault();
          setError(null);
          try {
            await createKChatRoom(newRoomID.trim());
            setNewRoomID("");
            refresh();
          } catch (err) {
            setError(translateApiError(err, t));
          }
        }}
        style={{ display: "flex", gap: 8, marginBottom: 16 }}
      >
        <input
          placeholder={t("kchat.roomIdPlaceholder")}
          value={newRoomID}
          onChange={(e) => setNewRoomID(e.target.value)}
          required
        />
        <button type="submit">{t("common.create")}</button>
      </form>

      <table style={{ width: "100%", borderCollapse: "collapse" }}>
        <thead>
          <tr style={{ textAlign: "left", borderBottom: "1px solid #e5e7eb" }}>
            <th style={th}>{t("kchat.roomIdColumn")}</th>
            <th style={th}>{t("kchat.folderIdColumn")}</th>
            <th style={th}>{t("kchat.createdAtColumn")}</th>
            <th style={th}>{t("common.actions")}</th>
          </tr>
        </thead>
        <tbody>
          {rooms.map((r) => (
            <tr key={r.id} style={{ borderBottom: "1px solid #f3f4f6" }}>
              <td style={td}>{r.kchat_room_id}</td>
              <td style={td}>{r.folder_id}</td>
              <td style={td}>{new Date(r.created_at).toLocaleString()}</td>
              <td style={td}>
                <button
                  onClick={() => setSyncTarget(r)}
                  style={{ marginRight: 8 }}
                >
                  {t("kchat.sync")}
                </button>
                <button
                  style={{ color: "#b91c1c" }}
                  onClick={async () => {
                    if (!confirm(t("kchat.deleteMappingPrompt", { id: r.kchat_room_id })))
                      return;
                    try {
                      await deleteKChatRoom(r.id);
                      refresh();
                    } catch (err) {
                      setError(translateApiError(err, t));
                    }
                  }}
                >
                  {t("common.delete")}
                </button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      {syncTarget ? (
        <SyncDialog
          room={syncTarget}
          onClose={() => setSyncTarget(null)}
          onError={(m) => setError(m)}
        />
      ) : null}
    </div>
  );
}

interface MemberDraft {
  user_id: string;
  role: string;
}

function SyncDialog({
  room,
  onClose,
  onError,
}: {
  room: KChatRoom;
  onClose: () => void;
  onError: (m: string) => void;
}) {
  const { t } = useTranslation();
  const [members, setMembers] = useState<MemberDraft[]>([{ user_id: "", role: "viewer" }]);
  const [info, setInfo] = useState<string | null>(null);

  const addRow = () =>
    setMembers((prev) => [...prev, { user_id: "", role: "viewer" }]);
  const removeRow = (idx: number) =>
    setMembers((prev) => prev.filter((_, i) => i !== idx));
  const setRow = (idx: number, patch: Partial<MemberDraft>) =>
    setMembers((prev) =>
      prev.map((m, i) => (i === idx ? { ...m, ...patch } : m)),
    );

  return (
    <div
      style={{
        position: "fixed",
        inset: 0,
        background: "rgba(15, 23, 42, 0.35)",
        display: "flex",
        alignItems: "center",
        justifyContent: "center",
      }}
    >
      <div
        style={{
          background: "white",
          padding: 20,
          borderRadius: 8,
          minWidth: 420,
          maxWidth: 600,
        }}
      >
        <h3 style={{ marginTop: 0 }}>{t("kchat.syncTitle", { id: room.kchat_room_id })}</h3>
        <p style={{ fontSize: 13, color: "#6b7280" }}>{t("kchat.syncDescription")}</p>
        {info ? <p style={{ color: "#047857" }}>{info}</p> : null}
        <div style={{ display: "grid", gap: 8 }}>
          {members.map((m, idx) => (
            <div key={idx} style={{ display: "flex", gap: 8 }}>
              <input
                style={{ flex: 1 }}
                placeholder={t("kchat.userIdPlaceholder")}
                value={m.user_id}
                onChange={(e) => setRow(idx, { user_id: e.target.value })}
              />
              <select
                value={m.role}
                onChange={(e) => setRow(idx, { role: e.target.value })}
              >
                <option value="viewer">{t("share.roleViewer")}</option>
                <option value="editor">{t("share.roleEditor")}</option>
                <option value="admin">{t("share.roleAdmin")}</option>
              </select>
              <button onClick={() => removeRow(idx)} type="button">
                −
              </button>
            </div>
          ))}
        </div>
        <div style={{ display: "flex", gap: 8, marginTop: 16 }}>
          <button onClick={addRow} type="button">
            {t("kchat.addRow")}
          </button>
          <button
            onClick={async () => {
              const payload = members
                .filter((m) => m.user_id.trim() !== "")
                .map((m) => ({ user_id: m.user_id.trim(), role: m.role }));
              try {
                const r = await syncKChatMembers(room.id, payload);
                setInfo(t("kchat.syncedCount", { count: r.synced }));
              } catch (err) {
                onError(translateApiError(err, t));
                onClose();
              }
            }}
            type="button"
          >
            {t("kchat.sync")}
          </button>
          <button onClick={onClose} type="button" style={{ marginLeft: "auto" }}>
            {t("common.close")}
          </button>
        </div>
      </div>
    </div>
  );
}



const th: React.CSSProperties = {
  padding: "8px 12px",
  fontSize: 12,
  color: "#6b7280",
};
const td: React.CSSProperties = {
  padding: "8px 12px",
  fontSize: 13,
  color: "#374151",
};
