# Native Android App (`mobile/android`)

A native **Kotlin / Jetpack Compose** client for ZK Drive. It consumes the
shared [Rust FFI bridge](./MOBILE_BRIDGE.md) (UniFFI-generated Kotlin bindings +
`libzk_mobile_bridge.so`) for crypto, auth, the API client and the offline sync
engine, and talks to the ZK Drive REST API for metadata, sharing and search.

Target: **Android 12+ (minSdk 31)**, `targetSdk`/`compileSdk` 35.

---

## 1. Architecture

Clean **MVVM + repository** layering, dependency-injected with Hilt:

```
ui/            Jetpack Compose screens + ViewModels (one per screen)
  ├─ MainActivity        single-activity host; AuthState gate + biometric unlock
  ├─ ZkApp               bottom-nav Scaffold + NavHost (Files / Search / Settings)
  ├─ login/ browser/ preview/ share/ search/ settings/
data/          repositories (the only thing ViewModels depend on)
  ├─ auth/      OAuth2 PKCE (AppAuth), encrypted token store, AuthRepository
  ├─ drive/     folder tree, transfers (presigned URLs), client-side crypto
  ├─ search/ sharing/ push/ settings/ sync/
domain/        framework-free models (FileNode, FolderNode, WorkspaceUsage…)
bridge/        thin wrappers over the UniFFI bridge (CryptoProvider, BridgeHolder)
di/            Hilt modules (network, dispatchers, bridge, workers)
```

The bridge is the source of truth for security-critical paths: the
`TokenManager` is shared by the REST `AuthInterceptor` and the bridge
`ApiClient`, and `strict_zk` content is encrypted client-side with the bridge
`CryptoEngine` (object-context AAD) **before** the presigned `PUT` and decrypted
on download.

## 2. Screens

| Screen | Highlights |
| --- | --- |
| **Login** | OAuth2 Authorization Code + PKCE against iam-core via Chrome Custom Tabs (AppAuth), deep-link callback, tokens stored in `EncryptedSharedPreferences`, `BiometricPrompt` unlock for returning users. |
| **File Browser** | Grid/list toggle, pull-to-refresh, breadcrumbs, per-folder encryption badge (Confidential vs Zero-Knowledge), FAB for upload / create-folder, swipe + overflow actions (share/delete). |
| **Upload** | Storage Access Framework picker + camera capture; background upload via WorkManager with progress in the notification tray; direct-to-storage via presigned URLs. |
| **Download / Preview** | Inline preview for images (Coil), PDF (`PdfRenderer`) and text; system share sheet; offline access through the bridge's local SQLite catalogue. |
| **Sharing** | Share links with password / expiry / max-downloads, guest invite by email, view/edit/admin permission management, system share-sheet integration. |
| **Search** | Debounced (300 ms) full-text search, file-type icons + path breadcrumbs, recent searches. |
| **Settings** | Account info, workspace storage bar, notification opt-in, background-sync constraints, biometric lock, theme (light/dark/system). |

## 3. Background sync & push

* **WorkManager** runs a periodic sync (`SyncScheduler`) constrained by the
  user's Wi-Fi / charging preferences; uploads are queued and survive
  backgrounding (`UploadWorker`).
* The bridge `SyncEngine` keeps a local SQLite catalogue and polls
  `/api/changes?since={cursor}`.
* **FCM** push is initialised at runtime from build-injected config (no
  committed `google-services.json`); the device token is registered via
  `POST /api/push/register-device` and de-registered on logout / opt-out.

## 4. Build

The Gradle build drives the Rust bridge automatically: the `buildRustBridge`
task runs [`build-android.sh`](../mobile/rust-bridge/build-android.sh) to
cross-compile `libzk_mobile_bridge.so` for all ABIs and regenerate the Kotlin
bindings, then wires both onto the app source sets. You therefore need the Rust
Android toolchain available:

```bash
rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android
cargo install cargo-ndk
# Android NDK r26d — point the build at it via either:
#   local.properties:  ndk.dir=/path/to/android-ndk-r26d
#   or env var:        export ANDROID_NDK_HOME=/path/to/android-ndk-r26d
```

Then, from `mobile/android`:

```bash
./gradlew :app:assembleDebug      # build the debug APK (+ bridge)
./gradlew :app:lintDebug          # Android lint
./gradlew :app:compileDebugKotlin # fast compile check
```

> The app pins `ndkVersion = 26.3.11579264` (r26d) — the same NDK the bridge is
> compiled with — so AGP can strip debug symbols from the shipped `.so` files.

## 5. Configuration

OAuth / API / Firebase values come from a git-ignored `oauth.properties`
(developer machines) or environment variables (CI) — never hard-coded secrets.
Safe non-secret placeholders let a fresh checkout configure and compile.

| `oauth.properties` key | Env var | Purpose |
| --- | --- | --- |
| `oidc.issuer` | `ZK_OIDC_ISSUER` | iam-core OIDC issuer (discovery base) |
| `oidc.clientId` | `ZK_OIDC_CLIENT_ID` | Public client id |
| `oidc.scope` | `ZK_OIDC_SCOPE` | Requested scopes (`openid profile email offline_access`) |
| `oidc.redirectUri` | `ZK_OIDC_REDIRECT_URI` | PKCE callback (`<scheme>:/oauth2redirect`) |
| `api.baseUrl` | `ZK_API_BASE_URL` | REST + bridge API origin |
| `web.baseUrl` | `ZK_WEB_BASE_URL` | Public web origin serving `/share/{token}` |
| `fcm.projectId` etc. | `ZK_FCM_*` | Firebase config; push is disabled if absent |

The OAuth redirect scheme is injected into the manifest via
`manifestPlaceholders`, so deployments can rebrand the callback without touching
the manifest.

## 6. Release signing

No keystore is committed. Release signing reads an upload keystore entirely from
environment variables; when absent, the release variant builds unsigned so local
`assembleRelease` still works:

| Env var | Meaning |
| --- | --- |
| `ZK_ANDROID_KEYSTORE` | Path to the upload keystore file |
| `ZK_ANDROID_KEYSTORE_PASSWORD` | Keystore password |
| `ZK_ANDROID_KEY_ALIAS` | Key alias |
| `ZK_ANDROID_KEY_PASSWORD` | Key password |

CI ([`.github/workflows/mobile-android.yml`](../.github/workflows/mobile-android.yml))
decodes the keystore from the `ZK_ANDROID_KEYSTORE_BASE64` secret into a temp
file and supplies the passwords from secrets before `assembleRelease`.
