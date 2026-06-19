# iOS app

The iOS client is a native **Swift / SwiftUI** app that puts a Northwind Trading
employee's workspace in their pocket: browse folders, preview files, upload from
the camera or the Files app, manage share links, and keep an encrypted offline
copy that catches up in the background. It is built for **iOS 15 and above**
(`mobile/ios/project.yml:15-16`) and lives in
[`mobile/ios`](../mobile/ios).

Like the Android client, it is a **thin shell**. Every security-critical
operation — holding and refreshing tokens, minting presigned transfer URLs,
sealing bytes, and tracking the change feed — runs in the shared
[Rust bridge](./MOBILE_BRIDGE.md), the same library the Android app and the
desktop sync client use. The Swift code owns the UI, the OS integrations
(Keychain, Face ID / Touch ID, background refresh, APNs, document and camera
pickers), and an on-device cache; it never reimplements cryptography.

---

## How it is put together

The app is MVVM with a single composition root. There is no global mutable
state: every long-lived service is built exactly once in `AppServices` and
injected down through the SwiftUI environment
(`mobile/ios/ZkDrive/App/AppServices.swift:6-49`).

```text
ZkDriveApp (@main)
└── AppBootstrap          loads config, builds AppServices, restores any session
    └── AppServices       composition root (one instance, @MainActor)
        ├── AppConfig        Info.plist baseline + /api/config overlay
        ├── BridgeSession    wraps the Rust bridge (TokenManager / ApiClient /
        │                    SyncEngine), keyed per workspace
        ├── KeychainStore    token bundle + offline-cache key
        ├── AuthService      OAuth2 PKCE + biometric-unlock state machine
        ├── DriveAPIClient   REST metadata plane (folders/files/shares/search/…)
        ├── OfflineStore     XChaCha20-Poly1305 encrypted on-device blob cache
        ├── TransferManager  background URLSession up/downloads (presigned URLs)
        ├── SyncCoordinator  drives the bridge SyncEngine change feed
        ├── PushManager      APNs registration → POST /api/push/register-device
        └── BackgroundSyncScheduler  BGAppRefreshTask periodic sync
```

| Layer | Location |
| --- | --- |
| App entry / coordination | `ZkDrive/App` |
| Config | `ZkDrive/Config` |
| Bridge integration | `ZkDrive/Bridge` |
| Auth (OAuth2 PKCE, Keychain, biometrics) | `ZkDrive/Auth` |
| Networking (REST + transfers) | `ZkDrive/Networking` |
| Background sync | `ZkDrive/Sync` |
| Push notifications | `ZkDrive/Notifications` |
| Domain models | `ZkDrive/Models` |
| Design system | `ZkDrive/DesignSystem` |
| Feature screens + view models | `ZkDrive/Features/*` |

The UI renders from two state machines. `RootView` switches on the bootstrap
state — a brand splash while config loads, a retry screen on a hard config
failure, or the app once `AppServices` is built — and inside the ready state
`AuthGate` switches on the `AuthService` state: `signedOut` shows Login,
`locked` shows the biometric prompt, `signedIn` shows the tab bar
(`mobile/ios/ZkDrive/App/ZkDriveApp.swift:22-61`).

### How the app consumes the bridge

`mobile/rust-bridge/build-ios.sh` emits two artifacts from the **same** UniFFI
crate metadata, so the Swift contract can never drift from the shipped binary:

- `build/ios/ZkMobileBridge.xcframework` — the compiled binary (device arm64 +
  simulator slice), linked and embedded by the app target.
- `build/ios/Sources/ZkMobileBridge/zk_mobile_bridge.swift` — the generated
  Swift API, compiled **inline** into the app target
  (`mobile/ios/project.yml:42-53`).

`BridgeSession` is the only type that talks to the generated API; everything
else depends on `BridgeSession`. Because the bridge's network and disk calls
block the calling thread by contract, `BridgeSession` runs them off the main
thread on a dedicated serial queue and bridges them into Swift `async`, while
pure in-memory accessors stay synchronous
(`mobile/ios/ZkDrive/Bridge/BridgeSession.swift:8-35`). Generated `BridgeError`s
are normalised into the app's single UI-facing `AppError`, so screens branch
uniformly on retry / re-auth / message.

---

## Signing in

Sign-in is OAuth2 Authorization Code with **PKCE**, run through the system
browser via `ASWebAuthenticationSession` so the user's identity-provider
session and password manager are available and the app never sees credentials
(`mobile/ios/ZkDrive/Auth/OAuthService.swift:5-39`).

The flow:

1. **Build the authorization request.** A fresh PKCE pair is generated per
   request — a 64-byte random `code_verifier`, its SHA-256 `S256` challenge, and
   a 32-byte CSRF `state` — using `SecRandomCopyBytes`
   (`mobile/ios/ZkDrive/Auth/PKCE.swift:7-38`). Only the challenge is sent to the
   authorize endpoint, so an intercepted authorization code is useless without
   the in-process verifier.
2. **Authorize in the system browser.** The session is started with
   `prefersEphemeralWebBrowserSession = true`, so no identity-provider cookie
   lingers between users on a shared device
   (`mobile/ios/ZkDrive/Auth/OAuthService.swift:59-84`).
3. **Validate and exchange.** The callback's `state` is checked against the one
   that was sent; a mismatch aborts the sign-in as a possible replay. The code
   plus verifier are then POSTed to the token endpoint for a bridge
   `TokenBundle` (`mobile/ios/ZkDrive/Auth/OAuthService.swift:87-129`).
4. **Load and persist atomically.** The tokens are loaded into the bridge
   `TokenManager` and written to the Keychain. If the Keychain write fails after
   the tokens were loaded, the bridge is rolled back so the app never sits in a
   half-signed-in state (`mobile/ios/ZkDrive/Auth/AuthService.swift:66-90`).

The authorize and token endpoints are derived from the configured OIDC issuer
using the conventional `oauth2/authorize` and `oauth2/token` paths; a deployment
can rotate the issuer, client id, redirect URI, and scopes at launch through the
server `/api/config` overlay without an app update
(`mobile/ios/ZkDrive/Config/AppConfig.swift:18-26`,
`:74-112`). The deeper OAuth/OIDC behavior is server-side; see
[`IAM_CORE.md`](./IAM_CORE.md) for the identity surface. The app describes only
the client side.

### Token storage and biometric unlock

The token bundle is persisted to the iOS Keychain as a `StoredTokenBundle`
(`kSecClassGenericPassword`) with the accessibility attribute
`kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`: it is available to
background sync after the first device unlock, never copied to a new device by a
backup (`mobile/ios/ZkDrive/Auth/KeychainStore.swift:4-9`, `:24-46`,
`:93-111`). A `keychain-access-groups` entitlement keeps the item readable
across reinstalls within the team's app suite
(`mobile/ios/ZkDrive/Resources/ZkDrive.entitlements:18-23`).

On launch the app restores the session from the Keychain with **no network
round-trip**: it re-seeds the bridge `TokenManager` from the stored bundle and
decodes the identity claims from the access token
(`mobile/ios/ZkDrive/Auth/AuthService.swift:46-63`, `:128-131`). When the access
token later expires, the bridge refreshes it transparently — the app never
re-implements refresh logic.

Biometric lock is opt-in. With it enabled and a biometric enrolled, a restored
session starts in the `locked` state and the tokens stay out of the bridge until
Face ID / Touch ID succeeds; the prompt falls back to the device passcode so a
user who cannot use biometrics is never locked out of their own data. Either
way the tokens are encrypted at rest in the Keychain
(`mobile/ios/ZkDrive/Auth/AuthService.swift:7-12`, `:46-58`, `:92-106`,
`mobile/ios/ZkDrive/Auth/BiometricAuth.swift:37-56`).

---

## Privacy modes on device

Every folder carries an `EncryptionMode` that the browser surfaces as a badge.
The model mirrors the Go `folder` package's two modes and decodes any unknown
server value to the safe, server-processed default rather than failing the whole
payload (`mobile/ios/ZkDrive/Models/DriveModels.swift:8-19`):

| Mode | Wire value | Badge | What it means |
| --- | --- | --- | --- |
| **Confidential** | `managed_encrypted` | "Confidential" | The gateway manages envelope encryption server-side, so it can generate previews, thumbnails, search, and malware scanning. |
| **Zero-Knowledge** | `strict_zk` | "Zero-Knowledge" | Content is end-to-end encrypted, so **the server cannot preview, index, search, or scan it** — the deliberate trade-off, surfaced wherever the mode appears. |

So a Northwind file in **Marketing Assets** (`managed_encrypted`) can be previewed
and searched server-side, while one in **Legal Contracts** or **Client Vault**
(`strict_zk`) cannot — by design.

### What the app encrypts on device

The iOS client's own use of the bridge's XChaCha20-Poly1305 envelope is the
**offline cache**. When a download is pinned for offline use, the plaintext is
sealed at rest under a 32-byte device key the bridge mints (`generateDek()`) and
stores in the Keychain, so cached blobs are unreadable even if the device
filesystem is extracted (`mobile/ios/ZkDrive/Bridge/OfflineStore.swift:1-77`).
Cached blobs are written with complete file protection
(`mobile/ios/ZkDrive/Bridge/OfflineStore.swift:60-67`) and the cache directory
is excluded from iCloud/iTunes backups
(`mobile/ios/ZkDrive/Bridge/AppPaths.swift:43-63`).

Uploads stream the file's bytes to the bridge-minted presigned URL
(`mobile/ios/ZkDrive/Networking/TransferManager.swift:90-118`); the iOS client
does not seal bytes before upload. Client-side sealing of `strict_zk` content
before the PUT is implemented in the shared bridge crypto and exercised by the
Android client — see [`ANDROID_APP.md`](./ANDROID_APP.md) and
[`MOBILE_BRIDGE.md`](./MOBILE_BRIDGE.md). On iOS, on-device confidentiality is
provided by the encrypted offline cache described above.

---

## Transfers

All byte movement is **direct to object storage** over presigned URLs minted by
the bridge `ApiClient`; the app server never proxies file bytes. Transfers run
on a **background `URLSession`**, so an upload or download continues when the app
is suspended and resumes after relaunch
(`mobile/ios/ZkDrive/Networking/TransferManager.swift:22-83`).

An upload follows the three-step presigned-PUT flow: ask the bridge for an
upload target (`upload-url`), PUT the bytes, then call `confirmUpload`. The
confirm step is driven from the session delegate, so it runs even if the PUT
finished while the app was backgrounded, and only a `2xx` PUT is confirmed
(`mobile/ios/ZkDrive/Networking/TransferManager.swift:90-118`, `:262-299`).
Pending upload and download bookkeeping is persisted to `UserDefaults`, so an
in-flight transfer survives an app relaunch and the system can re-deliver its
completion (`mobile/ios/ZkDrive/Networking/TransferManager.swift:42-66`,
`:249-258`). The transfers screen shows live progress and lets the user cancel
an in-flight job or clear finished ones
(`mobile/ios/ZkDrive/Networking/TransferManager.swift:141-156`).

A completed download is moved out of the system's temporary location
synchronously in the delegate (which deletes it on return), and — when the job
was pinned for offline — its plaintext is also sealed into the encrypted offline
store (`mobile/ios/ZkDrive/Networking/TransferManager.swift:226-238`,
`:301-313`).

Folder navigation, search, sharing, permissions, notifications, and push
registration use a small REST client (`DriveAPIClient`); transfers and the
change feed go through the bridge instead. Every REST request is authenticated
with the bridge-managed access token, so the native layer never re-implements
token logic (`mobile/ios/ZkDrive/Networking/DriveAPIClient.swift:1-28`,
`:207-225`).

---

## Preview and offline reads

The preview screen prefers an offline copy when one exists — instant, and works
with no signal — decrypting it through the bridge before display. Otherwise it
asks the bridge for a presigned `preview-url` and renders images, PDF, or text
inline; a type with no server-side preview (including `strict_zk` content)
falls back to a "download to view/share" affordance
(`mobile/ios/ZkDrive/Features/Preview/FilePreviewViewModel.swift:4-60`).

---

## Offline catalogue and background sync

The bridge `SyncEngine` keeps a local SQLite catalogue per workspace under
Application Support, so the file tree and the change-feed cursor survive
launches; the catalogue is excluded from backup like the offline cache
(`mobile/ios/ZkDrive/Bridge/BridgeSession.swift:100-116`,
`mobile/ios/ZkDrive/Bridge/AppPaths.swift:20-26`).

`SyncCoordinator` runs one incremental pass by calling `pollOnce(limit: 200)` in
a loop — each call fetches the next change-feed page from the durable cursor and
advances it — draining until there is no more, bounded to **25 pages** so a
long-offline device fully catches up in one invocation without unbounded work.
It guards against overlapping passes (a manual pull-to-refresh racing a
background run) and treats cooperative cancellation as a non-failure, letting the
durable cursor resume next time
(`mobile/ios/ZkDrive/Sync/SyncCoordinator.swift:47-89`).

Periodic background sync is a `BGAppRefreshTask`. The launch handler is
registered before the app finishes launching (Apple's contract) and resolves the
live scheduler at fire time; each run schedules the next occurrence first — with
an earliest start of **15 minutes** — so a crash mid-sync cannot break the
cadence, and a one-shot completion latch guarantees the task is completed exactly
once whether the work or the OS expiration deadline wins
(`mobile/ios/ZkDrive/Sync/BackgroundSyncScheduler.swift:5-93`). iOS ultimately
decides when to run it based on usage.

---

## Push notifications

Push uses APNs. After the user grants permission, the app registers for remote
notifications; on receiving the device token it registers with the server via
`POST /api/push/register-device` with `{ platform: "ios", token }`
(`mobile/ios/ZkDrive/Notifications/PushManager.swift:31-71`,
`mobile/ios/ZkDrive/Networking/DriveAPIClient.swift:170-183`). When the server
has mobile push disabled it answers `501`, which the app treats as "push
unavailable" and degrades gracefully rather than surfacing an error
(`mobile/ios/ZkDrive/Notifications/PushManager.swift:57-71`).

Notifications are shown as banners even in the foreground, and tapping one posts
a name the UI observes to deep-link to the relevant item
(`mobile/ios/ZkDrive/App/AppDelegate.swift:66-78`). Because the UIKit
`AppDelegate` callbacks (APNs tokens, background-URLSession events) can fire
before the async `AppServices` graph exists, they are routed through a shared
`AppDelegateRouter` that the services wire themselves into once ready
(`mobile/ios/ZkDrive/App/AppDelegate.swift:1-27`,
`mobile/ios/ZkDrive/App/AppServices.swift:37-41`).

---

## Privacy on sign-out

Logout is a clean teardown so the next account on a shared device inherits
nothing: the app unregisters the APNs token first (while the access token is
still valid), clears the bridge tokens, deletes the persisted Keychain bundle,
stops syncing the workspace, and erases every encrypted offline blob
(`mobile/ios/ZkDrive/Auth/AuthService.swift:108-124`). Combined with the
ephemeral browser session used at sign-in, no identity-provider cookie or
decrypted cache survives a sign-out.

---

## Configuration

Baseline runtime config is baked into `Info.plist` under the `ZKDriveConfig`
dictionary and overlaid at launch by the server `/api/config` endpoint, so a
deployment can rotate issuer and client id without an app update; a failed or
absent endpoint is non-fatal because the bundle config is fully functional on
its own (`mobile/ios/ZkDrive/Config/AppConfig.swift:1-26`, `:74-112`,
`mobile/ios/ZkDrive/Resources/Info.plist:83-98`).

| Key | Meaning |
| --- | --- |
| `APIBaseURL` | zk-drive API base URL |
| `OIDCIssuer` | iam-core issuer (authorize/token derived as `oauth2/authorize`, `oauth2/token`) |
| `OIDCClientID` | OAuth2 public client id for the iOS app (`zk-drive-ios`) |
| `OIDCRedirectURI` | PKCE callback (custom scheme `zkdrive://oauth/callback`, matching `CFBundleURLSchemes`) |
| `OIDCScopes` | space-separated scopes (`openid profile email offline_access`) |

The OAuth redirect also supports a universal link via the `applinks:` associated
domain in `ZkDrive.entitlements`; replace the example host with the deployment
domain and serve the matching `apple-app-site-association`
(`mobile/ios/ZkDrive/Resources/ZkDrive.entitlements:9-16`).

### Capabilities (`Info.plist` / entitlements)

- Usage strings: `NSFaceIDUsageDescription`, `NSCameraUsageDescription`,
  `NSPhotoLibraryUsageDescription`.
- `BGTaskSchedulerPermittedIdentifiers` = `com.zkdrive.app.sync.refresh`
  (matches `BackgroundSyncScheduler.taskIdentifier`).
- `UIBackgroundModes`: `fetch`, `processing`, `remote-notification`.
- Entitlements: `aps-environment`, `com.apple.developer.associated-domains`,
  `keychain-access-groups`
  (`mobile/ios/ZkDrive/Resources/Info.plist:44-64`,
  `mobile/ios/ZkDrive/Resources/ZkDrive.entitlements:1-24`).

---

## Building locally (macOS)

The Xcode project is generated from `project.yml` with
[XcodeGen](https://github.com/yonaskolb/XcodeGen) — the `.xcodeproj` is **not**
committed, so there is one declarative source of truth for targets, build
settings, and the linked bridge XCFramework
(`mobile/ios/project.yml:1-11`).

```bash
# 1. Build the Rust bridge XCFramework + generated Swift bindings.
#    (Requires the iOS Rust targets:
#     rustup target add aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios)
cd mobile/rust-bridge && ./build-ios.sh

# 2. Generate and open the Xcode project.
cd ../ios
brew install xcodegen   # once
xcodegen generate
open ZkDrive.xcodeproj
```

Set your team via the `ZKDRIVE_DEVELOPMENT_TEAM` build setting (or in Xcode's
Signing & Capabilities) for device builds; simulator builds need no signing
(`mobile/ios/project.yml:25-31`).

### Command-line build + test

```bash
cd mobile/ios
xcodegen generate
xcodebuild \
  -project ZkDrive.xcodeproj -scheme ZkDrive -configuration Debug \
  -destination 'platform=iOS Simulator,name=iPhone 15,OS=latest' \
  CODE_SIGNING_ALLOWED=NO clean build test
```

> The Rust bridge cross-compile and the XCFramework link step require the Apple
> SDK and Xcode, so they only run on macOS. On Linux, `build-ios.sh` still
> generates the Swift bindings and then stops before the macOS-only stages.

---

## Tests

Unit tests live in `mobile/ios/ZkDriveTests` and cover the deterministic core:

- `PKCETests` — RFC 7636 verifier/challenge derivation against a canonical vector.
- `AppErrorTests` — HTTP/bridge error → category, retry/re-auth semantics.
- `OAuthServiceTests` — token-request form encoding and token-response decoding.
- `ModelCodingTests` — snake_case API decoding and the forward-compatible
  `EncryptionMode` fallback.
- `FormatAndShareTargetTests` — byte formatting and node → share-target mapping.

Run them with the `xcodebuild ... test` command above.

---

## Continuous integration

[`.github/workflows/mobile-ios.yml`](../.github/workflows/mobile-ios.yml) runs on
a macOS runner: it installs the iOS Rust targets, runs `build-ios.sh` to produce
the XCFramework and bindings, installs XcodeGen, generates the project, then
builds and unit-tests the app on the iOS Simulator (selecting an available
simulator by UDID). It triggers only on changes under `mobile/**`, `sdk/**`, or
the workflow file itself, and — like the Android and bridge workflows — is **not
a merge gate**.
