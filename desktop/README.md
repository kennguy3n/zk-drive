# ZK Drive Desktop

The desktop sync client keeps a Northwind Trading workspace mirrored to a folder
on a Mac, Windows, or Linux machine: bind a local folder to a workspace, watch
it sync in the background, and resolve the occasional conflict — with a
system-tray status indicator that is always one glance away.

It is built with [Tauri v2](https://tauri.app). A small Rust host drives the
shared [sync SDK](../sdk/README.md) (`sdk/crates/`) and a React/TypeScript UI
renders sync status, selective-sync settings, and conflict resolution. The host
contains no sync logic of its own — it adapts the SDK's command surface to the
window and the tray, exactly as the SDK's own GUI-host guide describes.

## Architecture

```text
                         ┌──────────────────────────────────────────────┐
   React UI (Vite)       │  src-tauri (Rust host)                        │
  ┌─────────────────┐    │  ┌─────────────┐    ┌────────────────────────┐│
  │ App.tsx         │    │  │ commands.rs │───▶│ zk_sync_shell::App     ││
  │  Login          │◀──▶│  │ (#[command])│    │   .dispatch(Command)   ││
  │  SyncStatus     │ IPC│  └─────────────┘    └────────────────────────┘│
  │  Settings       │    │  ┌─────────────┐               │ BroadcastSink │
  │  ConflictDialog │    │  │ events.rs   │◀──────────────┘ (ShellEvent)  │
  └─────────────────┘    │  │ emit("sync")│──┐                            │
        ▲                │  └─────────────┘  │ AppHandle::emit            │
        │  listen("sync")│  ┌─────────────┐  │                            │
        └────────────────┼──┤ tray.rs     │◀─┘ (TrayState aggregate)      │
                         │  └─────────────┘                               │
                         │  ┌─────────────┐  OAuth2 PKCE loopback +        │
                         │  │ auth.rs     │  OS keychain (zk_sync_auth)    │
                         │  └─────────────┘                               │
                         └──────────────────────────────────────────────┘
```

| Layer | File | Responsibility |
| --- | --- | --- |
| Host entry | `src-tauri/src/main.rs`, `lib.rs` | Build the shell `App`, restore persisted workspaces, attach an API client when a token is present, start the health loop, and wire plugins, tray, and the event forwarder (`src-tauri/src/lib.rs:60-153`). |
| Commands | `src-tauri/src/commands.rs` | `#[tauri::command]` handlers that map each UI action to `App::dispatch(Command::*)` and return the typed `CommandResult`/`DesktopError` (`src-tauri/src/commands.rs:33-176`). |
| Events | `src-tauri/src/events.rs` | Subscribe to the shell `BroadcastSink` and forward each `ShellEvent` to the webview on the `"sync"` channel; refresh the tray on `TrayChanged` (`src-tauri/src/events.rs:34-63`). |
| Tray | `src-tauri/src/tray.rs` | System-tray icon, tooltip, and title rendered from the cross-workspace `TrayState` (`src-tauri/src/tray.rs:23-107`). |
| Auth | `src-tauri/src/auth.rs` | OAuth2 PKCE loopback login, OS-keychain token storage, and a refreshing `TokenProvider` for the API client (`src-tauri/src/auth.rs:100-263`). |
| UI | `src/**` | React pages: `Login`, `SyncStatus` (+ workspace binding), `Settings` (selective sync), and the `ConflictDialog`. |

The Rust host depends on the SDK crates by path (`src-tauri/Cargo.toml:46-50`):

- `zk-sync-shell` → `../../sdk/crates/desktop-shell`
- `zk-sync-api` → `../../sdk/crates/api-client`
- `zk-sync-auth` → `../../sdk/crates/auth`

`src-tauri` is its **own** cargo workspace — it is deliberately not a member of
the `sdk/` workspace — so the desktop build can pull in Tauri (and the WebKit /
Cocoa / WebView2 toolchains) without weighing down the headless SDK build
(`src-tauri/Cargo.toml:1-12`).

## What it does

- **Bind a folder to a workspace.** On the Sync tab, pick a local folder and
  give it a label; the host issues `AddWorkspace` and the SDK seeds a local
  catalogue for it (`src/pages/SyncStatus.tsx:38-53`,
  `src-tauri/src/commands.rs:33-49`).
- **Live sync status.** Each workspace row shows a health pill (Up to date /
  Syncing / Conflicts / Error / Starting / Paused), a progress bar, and
  synced / pending / conflict counts, driven by the shell's `WorkspaceState`
  summary (`src/pages/SyncStatus.tsx:171-208`, `src/types.ts:16-48`).
- **Pause, resume, restart.** A running workspace can be paused (`StopSync`); a
  paused one resumed (`StartSync`); an errored one is restarted with a
  stop-then-start so no engine task is left orphaned
  (`src/pages/SyncStatus.tsx:124-165`, `src-tauri/src/commands.rs:79-106`).
- **Tray at a glance.** The native tray aggregates every workspace into one
  health with a tooltip such as
  `ZK Drive — Up to date (2/3 workspaces, 4 pending, 0 conflicts)`; left-click
  opens the window, and closing the window hides it to the tray rather than
  quitting (`src-tauri/src/tray.rs:91-107`, `src-tauri/src/lib.rs:126-136`).
- **Manage the local cache.** A workspace can be removed from the registry
  (catalogue preserved) and its local catalogue deleted as a separate, explicit
  step, so a misclicked removal is recoverable
  (`src-tauri/src/commands.rs:51-77`).
- **Stay current.** The Tauri updater plugin checks a release feed and installs
  an update on the next launch (see [Auto-update](#auto-update)).

### Selective sync and conflict resolution

The Settings tab presents a per-folder policy picker — **offline** (keep a full
local copy), **online** (metadata only, fetch on demand), or **ignore** (don't
sync) — and the Sync tab opens a conflict dialog offering a keep-local /
keep-remote choice when a workspace reports conflicts
(`src/pages/Settings.tsx:12-18`, `src/components/ConflictDialog.tsx:45-71`).

These two actions are honest about their backing: the SDK's `Command` surface
covers workspace registration, start/stop sync, status, and the tray aggregate,
and does not expose a per-folder policy or a per-file conflict-resolution
command (`sdk/crates/desktop-shell/src/command.rs:36-75`). The host's
`set_folder_policy` and `resolve_conflict` handlers therefore return a structured
`Unsupported` error, which the UI surfaces verbatim rather than silently
no-op'ing or faking engine behaviour
(`src-tauri/src/commands.rs:184-211`, `src-tauri/src/error.rs:27-32`). Conflicts
are still **detected** and counted by the engine and shown in the tray and the
workspace row; what the command surface omits is the one-click resolution
itself.

## How it authenticates

Sign-in is OAuth2 Authorization Code with **PKCE**, run as the native-app
loopback flow from [RFC 8252](https://www.rfc-editor.org/rfc/rfc8252) §7.3 so the
user logs in through their real system browser, never an embedded webview
(`src-tauri/src/auth.rs:1-34`, `:100-126`):

1. Generate a fresh PKCE verifier/challenge and an anti-CSRF `state` from the
   auth crate's CSPRNG (`src-tauri/src/auth.rs:103-104`).
2. Bind an ephemeral `127.0.0.1` listener and advertise
   `http://127.0.0.1:<port>/callback` as the `redirect_uri`
   (`src-tauri/src/auth.rs:108-110`).
3. Open the system browser at `{base}/api/auth/oauth/{provider}` (Google or
   Microsoft) with the `S256` `code_challenge` and `state`
   (`src-tauri/src/auth.rs:272-292`).
4. Capture the redirect on the loopback, verify `state` matches, and exchange
   the code (plus the original `code_verifier`) at `{base}/api/auth/oauth/token`
   (`src-tauri/src/auth.rs:307-363`, `:446-504`).
5. Persist the resulting token set in the OS keychain and attach a refreshing
   API client to the shell so sync can start
   (`src-tauri/src/auth.rs:124-125`, `src-tauri/src/commands.rs:156-170`).

Tokens are vended on every request by a keychain-backed `TokenProvider` that
refreshes within a 60-second expiry skew and coalesces concurrent refreshes onto
a single in-flight request, matching the SDK auth crate's own `TokenSource`
discipline (`src-tauri/src/auth.rs:163-263`). The keychain entry is namespaced by
backend base URL, so dev, staging, and production installs keep independent
credentials (`src-tauri/src/auth.rs:85-98`). The deeper OAuth/OIDC surface is
server-side; see [`docs/IAM_CORE.md`](../docs/IAM_CORE.md) for the identity
plane. This client describes only its own behaviour.

> **Honest callout.** The loopback flow depends on the backend honouring a
> loopback `redirect_uri` for the desktop client id and returning the token
> bundle from `/api/auth/oauth/token`. The backend's browser OAuth handler keeps
> the PKCE verifier in an `HttpOnly` cookie and issues a session on the browser
> origin instead (`api/auth/oauth.go`). This module implements the client half
> of the native flow.

## How it binds workspaces

A workspace binding is a `(label, local folder)` pair the host registers with a
fresh UUID via `AddWorkspace` (`src-tauri/src/commands.rs:33-49`). The SDK keeps
each workspace's state — file counts by status, the change-feed cursor, and
health — in a per-workspace local catalogue, and the shell persists the registry
to a JSON sidecar at `<config-dir>/zk-drive/app.json`
(`src-tauri/src/lib.rs:49-56`).

On launch the host calls `resume_persisted` to re-register every saved
workspace, and — if a bearer is already in the keychain — builds a refreshing API
client up front so autostart workspaces sync without a fresh sign-in. It then
starts the shell's 1 Hz health loop, which samples each catalogue and broadcasts
state changes (`src-tauri/src/lib.rs:101-119`). Every `ShellEvent` is forwarded
to the React UI on the `"sync"` channel; the UI re-pulls the full state on each
event rather than maintaining a parallel reducer, so a dropped broadcast never
leaves it stale (`src/App.tsx:38-48`, `src-tauri/src/events.rs:31-63`). The
engine's change-feed and conflict model are documented in the
[sync SDK README](../sdk/README.md).

## Prerequisites

- **Node.js** ≥ 18 and npm.
- **Rust** stable toolchain (the host builds with Rust 1.86;
  `src-tauri/Cargo.toml:22`).
- **Tauri system dependencies** — see the
  [official guide](https://tauri.app/start/prerequisites/). On Debian/Ubuntu:

  ```bash
  sudo apt-get install -y \
    libwebkit2gtk-4.1-dev libgtk-3-dev libsoup-3.0-dev librsvg2-dev \
    libayatana-appindicator3-dev libssl-dev libglib2.0-dev pkg-config
  ```

  macOS needs the Xcode command-line tools; Windows needs the WebView2 runtime
  and the MSVC build tools.

## Develop

```bash
cd desktop
npm install
npm run tauri dev      # launches the app with HMR (Vite on :1420)
```

Run only the web UI (no native window):

```bash
npm run dev
```

## Build

```bash
cd desktop
npm install
npm run tauri build
```

Installers are emitted under `src-tauri/target/release/bundle/`
(`src-tauri/tauri.conf.json:29-44`):

| Platform | Artifact |
| --- | --- |
| macOS | `.dmg` (+ `.app`) |
| Windows | `.msi` |
| Linux | `.AppImage`, `.deb` |

A release pipeline runs `npm run tauri build` on a matrix of the three OSes to
produce all installers.

### Validate without a full native build

The full bundling step needs the system libraries above. To validate the code on
a headless machine:

```bash
cd desktop && npm install && npm run build   # type-check + Vite production build
cd src-tauri && cargo build                  # compile the Rust host
```

## Configuration

| Env var | Default | Purpose |
| --- | --- | --- |
| `ZK_DRIVE_BASE_URL` | `https://drive.example.com` | Backend gateway base URL the OAuth flow and API client target (`src-tauri/src/lib.rs:35-38`, `:70-71`). |
| `ZK_DRIVE_OAUTH_CLIENT_ID` | `zk-drive-desktop` | OAuth2 public client id used for PKCE and refresh (`src-tauri/src/auth.rs:301-302`). |

State is persisted to `<config-dir>/zk-drive/app.json`; tokens live in the OS
keychain under the `com.zkdrive.desktop` service, keyed by backend base URL
(`src-tauri/src/auth.rs:49-98`).

## Auto-update

The Tauri updater plugin is configured in `tauri.conf.json` to poll a GitHub
Releases `latest.json` manifest and install an update on the next launch
(`src-tauri/src/lib.rs:88`, `src-tauri/src/tauri.conf.json:45-55`). The committed
`plugins.updater.pubkey` is a placeholder: a distributor replaces it with the
public half of their signing key (`npm run tauri signer generate`) and signs
release artifacts with the matching private key.

## Continuous integration

There is no separate desktop workflow. Because `src-tauri` is its own cargo
workspace, the repository's `sdk` CI job — `cargo fmt --check`,
`cargo clippy --workspace -D warnings`, and `cargo test --workspace` over
`sdk/` — exercises the SDK crates the host depends on but does not compile the
Tauri host itself (`.github/workflows/ci.yml:114-148`). The host is validated
with the headless type-check + `cargo build` steps above.
