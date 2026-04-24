import { useState } from "react";
import {
  createGuestInvite,
  createShareLink,
  type FileItem,
  type Folder,
  type ShareLink,
  type GuestInvite,
} from "../api/client";

// ShareDialog is the single entry point for sharing a file or folder.
// It intentionally keeps both share-link and guest-invite flows in one
// modal because from the end-user's perspective these are two
// renderings of the same intent ("give this resource to someone") and
// switching modals mid-flow is jarring. Existing links and invites are
// rendered below the forms so the user can audit what they've already
// handed out without a separate "manage" surface.
interface Props {
  resource:
    | { type: "folder"; value: Folder }
    | { type: "file"; value: FileItem };
  onClose: () => void;
}

type Role = "viewer" | "commenter" | "editor";

export default function ShareDialog({ resource, onClose }: Props) {
  const [tab, setTab] = useState<"link" | "invite">("link");
  const [error, setError] = useState<string | null>(null);
  const [link, setLink] = useState<ShareLink | null>(null);
  const [invite, setInvite] = useState<GuestInvite | null>(null);

  // Share link form state
  const [linkRole, setLinkRole] = useState<Role>("viewer");
  const [linkPassword, setLinkPassword] = useState("");
  const [linkExpiresAt, setLinkExpiresAt] = useState("");
  const [linkMaxDownloads, setLinkMaxDownloads] = useState("");

  // Guest invite form state
  const [inviteEmail, setInviteEmail] = useState("");
  const [inviteRole, setInviteRole] = useState<Role>("viewer");
  const [inviteExpiresAt, setInviteExpiresAt] = useState("");

  const submitLink = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      const maxDownloads = linkMaxDownloads
        ? Number.parseInt(linkMaxDownloads, 10)
        : undefined;
      const created = await createShareLink({
        resource_type: resource.type,
        resource_id: resource.value.id,
        role: linkRole,
        password: linkPassword || undefined,
        // datetime-local inputs give "YYYY-MM-DDTHH:mm" without a
        // timezone; rely on the backend's permissive RFC3339 parser to
        // treat these as local-time ISO strings.
        expires_at: linkExpiresAt || undefined,
        max_downloads:
          Number.isFinite(maxDownloads) && (maxDownloads as number) > 0
            ? maxDownloads
            : undefined,
      });
      setLink(created);
    } catch (err) {
      setError(String((err as Error)?.message ?? err));
    }
  };

  const submitInvite = async (e: React.FormEvent) => {
    e.preventDefault();
    setError(null);
    try {
      const created = await createGuestInvite({
        resource_type: resource.type,
        resource_id: resource.value.id,
        email: inviteEmail,
        role: inviteRole,
        expires_at: inviteExpiresAt || undefined,
      });
      setInvite(created);
    } catch (err) {
      setError(String((err as Error)?.message ?? err));
    }
  };

  return (
    <div style={overlay} onClick={onClose}>
      <div style={modal} onClick={(e) => e.stopPropagation()}>
        <header style={header}>
          <div>
            <div style={{ fontSize: 12, color: "#6b7280", textTransform: "uppercase" }}>
              Share {resource.type}
            </div>
            <div style={{ fontSize: 16, fontWeight: 600 }}>{resource.value.name}</div>
          </div>
          <button onClick={onClose} style={closeBtn} aria-label="Close">×</button>
        </header>

        <nav style={tabs}>
          <TabButton active={tab === "link"} onClick={() => setTab("link")}>
            Share link
          </TabButton>
          <TabButton active={tab === "invite"} onClick={() => setTab("invite")}>
            Invite by email
          </TabButton>
        </nav>

        {error ? <div style={errorBox}>{error}</div> : null}

        {tab === "link" ? (
          <form onSubmit={submitLink} style={form}>
            <Field label="Role">
              <select
                value={linkRole}
                onChange={(e) => setLinkRole(e.target.value as Role)}
                style={input}
              >
                <option value="viewer">Viewer</option>
                <option value="commenter">Commenter</option>
                <option value="editor">Editor</option>
              </select>
            </Field>
            <Field label="Password (optional)">
              <input
                type="password"
                value={linkPassword}
                onChange={(e) => setLinkPassword(e.target.value)}
                style={input}
                placeholder="Leave blank for no password"
              />
            </Field>
            <Field label="Expires at (optional)">
              <input
                type="datetime-local"
                value={linkExpiresAt}
                onChange={(e) => setLinkExpiresAt(e.target.value)}
                style={input}
              />
            </Field>
            <Field label="Max downloads (optional)">
              <input
                type="number"
                min={1}
                value={linkMaxDownloads}
                onChange={(e) => setLinkMaxDownloads(e.target.value)}
                style={input}
                placeholder="Unlimited"
              />
            </Field>
            <button type="submit" style={submitBtn}>Create link</button>
            {link ? <ShareLinkCard link={link} /> : null}
          </form>
        ) : (
          <form onSubmit={submitInvite} style={form}>
            <Field label="Email">
              <input
                type="email"
                required
                value={inviteEmail}
                onChange={(e) => setInviteEmail(e.target.value)}
                style={input}
                placeholder="guest@example.com"
              />
            </Field>
            <Field label="Role">
              <select
                value={inviteRole}
                onChange={(e) => setInviteRole(e.target.value as Role)}
                style={input}
              >
                <option value="viewer">Viewer</option>
                <option value="commenter">Commenter</option>
                <option value="editor">Editor</option>
              </select>
            </Field>
            <Field label="Expires at (optional)">
              <input
                type="datetime-local"
                value={inviteExpiresAt}
                onChange={(e) => setInviteExpiresAt(e.target.value)}
                style={input}
              />
            </Field>
            <button type="submit" style={submitBtn}>Send invite</button>
            {invite ? <GuestInviteCard invite={invite} /> : null}
          </form>
        )}
      </div>
    </div>
  );
}

function TabButton({
  active,
  children,
  onClick,
}: {
  active: boolean;
  children: React.ReactNode;
  onClick: () => void;
}) {
  return (
    <button
      onClick={onClick}
      style={{
        padding: "8px 12px",
        border: "none",
        borderBottom: active ? "2px solid #2563eb" : "2px solid transparent",
        background: "transparent",
        fontSize: 13,
        color: active ? "#2563eb" : "#6b7280",
        cursor: "pointer",
      }}
    >
      {children}
    </button>
  );
}

function Field({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <label style={{ display: "grid", gap: 4, fontSize: 12, color: "#374151" }}>
      <span>{label}</span>
      {children}
    </label>
  );
}

function ShareLinkCard({ link }: { link: ShareLink }) {
  const url = `${window.location.origin}/share/${link.token}`;
  return (
    <div style={resultBox}>
      <div style={{ fontSize: 12, color: "#065f46" }}>Link created</div>
      <div style={{ wordBreak: "break-all", fontFamily: "monospace", fontSize: 12 }}>{url}</div>
      <button
        type="button"
        onClick={() => navigator.clipboard?.writeText(url)}
        style={{ ...submitBtn, marginTop: 8, background: "#ecfdf5", color: "#065f46" }}
      >
        Copy to clipboard
      </button>
    </div>
  );
}

function GuestInviteCard({ invite }: { invite: GuestInvite }) {
  return (
    <div style={resultBox}>
      <div style={{ fontSize: 12, color: "#065f46" }}>Invite sent</div>
      <div>
        <strong>{invite.email}</strong> as <em>{invite.role}</em>
      </div>
    </div>
  );
}

const overlay: React.CSSProperties = {
  position: "fixed",
  inset: 0,
  background: "rgba(17, 24, 39, 0.5)",
  display: "flex",
  alignItems: "center",
  justifyContent: "center",
  zIndex: 50,
};

const modal: React.CSSProperties = {
  width: 420,
  background: "white",
  borderRadius: 8,
  boxShadow: "0 20px 40px rgba(0,0,0,0.2)",
  padding: 20,
};

const header: React.CSSProperties = {
  display: "flex",
  justifyContent: "space-between",
  alignItems: "flex-start",
  marginBottom: 12,
};

const closeBtn: React.CSSProperties = {
  background: "transparent",
  border: "none",
  fontSize: 24,
  lineHeight: 1,
  cursor: "pointer",
  color: "#6b7280",
};

const tabs: React.CSSProperties = {
  display: "flex",
  gap: 8,
  borderBottom: "1px solid #e5e7eb",
  marginBottom: 12,
};

const form: React.CSSProperties = {
  display: "grid",
  gap: 10,
};

const input: React.CSSProperties = {
  padding: "6px 8px",
  border: "1px solid #d1d5db",
  borderRadius: 4,
  fontSize: 13,
  width: "100%",
  boxSizing: "border-box",
};

const submitBtn: React.CSSProperties = {
  padding: "8px 12px",
  background: "#2563eb",
  color: "white",
  border: "none",
  borderRadius: 4,
  fontSize: 13,
  cursor: "pointer",
};

const errorBox: React.CSSProperties = {
  padding: "8px 10px",
  background: "#fef2f2",
  color: "#b91c1c",
  border: "1px solid #fecaca",
  borderRadius: 4,
  fontSize: 12,
  marginBottom: 10,
};

const resultBox: React.CSSProperties = {
  padding: 10,
  background: "#ecfdf5",
  border: "1px solid #a7f3d0",
  borderRadius: 4,
  fontSize: 12,
  display: "grid",
  gap: 4,
};
