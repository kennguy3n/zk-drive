# ZK Drive Desktop

Cross-platform (macOS / Windows / Linux) desktop sync client for ZK Drive,
built with [Tauri v2](https://tauri.app). The Rust host drives the existing
sync SDK (`sdk/crates/`) and a small React/TypeScript UI renders sync status,
selective-sync settings and conflict resolution.

## Architecture

```
                         ┌──────────────────────────────────────────┐
   React UI (Vite)       │  src-tauri (Rust host)                    │
  ┌─────────────────┐    │  ┌─────────────┐    ┌───────────────────┐ │
  │ App.tsx         │    │  │ commands.rs │───▶│ zk_sync_shell::App │ │
  │  Login          │◀──▶│  │ (#[command])│    │  .dispatch(Command)│ │
  │  SyncStatus     │ IPC│  └─────────────┘    └───────────────────┘ │
  │  Settings       │    │  ┌─────────────┐            │ BroadcastSink│
  │  ConflictDialog │    │  │ events.rs   │◀───────────┘ (ShellEvent) │
  └─────────────────┘    │  │ emit("sync")│──┐                        │
        ▲                │  └─────────────┘  │ window.emit            │
        │  listen("sync")│  ┌─────────────┐  │                        │
        └────────────────┼──┤ tray.rs     │◀─┘ (TrayState aggregate)  │
                         │  └─────────────┘                           │
                         │  ┌─────────────┐  OAuth2 PKCE + keychain    │
                         │  │ auth.rs     │  (zk_sync_auth)            │
                         │  └─────────────┘                           │
                         └──────────────────────────────────────────┘
```

| Layer | File | Responsibility |
| --- | --- | --- |
| Host entry | `src-tauri/src/main.rs`, `lib.rs` | Build the shell `App`, restore persisted workspaces, manage state, wire plugins, tray and the event forwarder. |
| Commands | `src-tauri/src/commands.rs` | `#[tauri::command]` handlers that map to `App::dispatch(Command::*)` (add/remove workspace, pause/resume, status, auth). |
| Events | `src-tauri/src/events.rs` | Subscribe to the `BroadcastSink` and forward each `ShellEvent` to the webview via `window.emit("sync", …)`; refresh the tray. |
| Tray | `src-tauri/src/tray.rs` | System-tray icon + tooltip rendered from `summary::TrayState`. |
| Auth | `src-tauri/src/auth.rs` | OAuth2 PKCE loopback login, OS-keychain token storage, and a refreshing `TokenProvider` for the api-client. |
| UI | `src/**` | React pages: `Login`, `SyncStatus` (+ workspace binding), `Settings` (selective sync), `ConflictDialog`. |

The Rust host depends on the SDK crates by path:

- `zk-sync-shell` → `../../sdk/crates/desktop-shell`
- `zk-sync-api` → `../../sdk/crates/api-client`
- `zk-sync-auth` → `../../sdk/crates/auth`

`src-tauri` is its own cargo workspace (it is **not** a member of the `sdk/`
workspace) so the desktop build can pull in Tauri without perturbing the SDK's
dependency graph.

## Prerequisites

- **Node.js** ≥ 18 and npm
- **Rust** stable toolchain
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

Bundles are emitted under `src-tauri/target/release/bundle/`:

| Platform | Artifact |
| --- | --- |
| macOS | `.dmg` (+ `.app`) |
| Windows | `.msi` |
| Linux | `.AppImage`, `.deb` |

CI should run `npm run tauri build` on a matrix of the three OSes to produce
all installers.

### Validate without a full native build

The full bundling step needs the system libraries above. To validate the code
on a headless machine:

```bash
cd desktop && npm install && npm run build   # type-check + Vite production build
cd src-tauri && cargo build                  # compile the Rust host
```

Both are run in CI-equivalent form during development and currently pass.

## Configuration

| Env var | Default | Purpose |
| --- | --- | --- |
| `ZK_DRIVE_BASE_URL` | `https://drive.example.com` | Backend gateway base URL. |
| `ZK_DRIVE_OAUTH_CLIENT_ID` | `zk-drive-desktop` | OAuth2 client id used for PKCE + refresh. |

State is persisted to `<config-dir>/zk-drive/app.json` (via the shell's
`resume_persisted`); tokens live in the OS keychain under the
`com.zkdrive.desktop` service.

## Auto-update

The Tauri updater plugin is configured in `tauri.conf.json` to poll a GitHub
Releases `latest.json` manifest. **Before shipping**, replace the placeholder
`plugins.updater.pubkey` with the public half of your signing key
(`npm run tauri signer generate`) and sign release artifacts with the matching
private key.

## Selective sync & conflict resolution — SDK status

The UI exposes a per-folder policy picker (`offline` / `online` / `ignore`) and
a conflict-resolution dialog. At the current SDK revision the desktop-shell
`Command` enum does **not** yet expose `SetFolderPolicy` or `ResolveConflict`,
so those command handlers return a structured `Unsupported` error and the UI
surfaces it verbatim ("coming soon") rather than stubbing engine behaviour. The
wiring is complete and will work unchanged once the SDK grows those commands.

## OAuth2 PKCE deviation

The task references opening the system browser at
`{ZK_DRIVE_BASE_URL}/api/auth/oauth/{provider}` and capturing a callback token.
The backend's current web flow keeps the PKCE verifier in an `HttpOnly` cookie
and returns a **session cookie**. This desktop client instead implements the
native-app loopback flow ([RFC 8252](https://www.rfc-editor.org/rfc/rfc8252)):

1. Generate a PKCE challenge (`zk_sync_auth::pkce`) and bind an ephemeral
   loopback listener on `127.0.0.1`.
2. Open the system browser at the provider authorize URL with
   `redirect_uri=http://127.0.0.1:<port>/callback` and the `code_challenge`.
3. Capture the `code` on the loopback listener, exchange it at
   `{base}/api/auth/oauth/token` with the `code_verifier`, and persist the
   returned `TokenSet` to the OS keychain.

This requires a **small additive backend change**: accept a loopback
`redirect_uri` for the desktop client id and return tokens from the token
endpoint (instead of only setting a session cookie). That change is owned by a
backend session and is intentionally **not** made here.
