# Native iOS App (Swift / SwiftUI)

The ZK Drive iOS app lives in [`mobile/ios`](../mobile/ios). It is a native
SwiftUI application (iOS 15+) that consumes the shared Rust FFI bridge
documented in [MOBILE_BRIDGE.md](./MOBILE_BRIDGE.md) for all crypto, auth,
API and sync logic — the app layers a production UI, OS integrations
(Keychain, biometrics, background refresh, APNs, document/camera pickers)
and an offline cache on top of that single source of truth.

---

## 1. Architecture

The app is MVVM with a single composition root. There is no global mutable
state: every long-lived service is constructed once in `AppServices` and
injected down through the SwiftUI environment.

```
ZkDriveApp (@main)
└── AppBootstrap            loads config, builds AppServices, restores session
    └── AppServices         composition root (one instance, @MainActor)
        ├── AppConfig        bundle + /api/config overlay
        ├── BridgeSession    wraps the Rust bridge (CryptoEngine/TokenManager/
        │                    ApiClient/SyncEngine), keyed per workspace
        ├── KeychainStore    token bundle + offline-cache DEK
        ├── AuthService      OAuth2 PKCE + biometric unlock state machine
        ├── DriveAPIClient   REST metadata plane (folders/files/shares/etc.)
        ├── OfflineStore     XChaCha20-Poly1305 encrypted on-device blob cache
        ├── TransferManager  background URLSession up/downloads (presigned URLs)
        ├── SyncCoordinator  drives the bridge SyncEngine changefeed
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
| Reusable utilities | `ZkDrive/Util` |

### How the app consumes the bridge

`mobile/rust-bridge/build-ios.sh` emits two artifacts from the **same**
UniFFI crate metadata:

- `build/ios/ZkMobileBridge.xcframework` — the compiled binary (device
  arm64 + simulator slice), linked + embedded by the app target.
- `build/ios/Sources/ZkMobileBridge/zk_mobile_bridge.swift` — the generated
  Swift API, compiled **inline** into the app target (see `project.yml`
  `sources`), so the Swift contract can never drift from the shipped binary.

`BridgeSession` is the only place that talks to the generated API; everything
else depends on `BridgeSession`. Generated `BridgeError`s are normalised into
the app's single UI-facing `AppError` so screens can branch uniformly on
retry / re-auth / message.

---

## 2. Screens

| Screen | Files | Highlights |
| --- | --- | --- |
| Login | `Auth/LoginView`, `OAuthService`, `PKCE`, `BiometricAuth` | iam-core OAuth2 Authorization Code + PKCE via `ASWebAuthenticationSession`; tokens in Keychain; Face ID / Touch ID unlock for returning users |
| File Browser | `Features/FileBrowser/*` | Grid/list toggle, pull-to-refresh, breadcrumb, per-folder encryption badge (Confidential vs Zero-Knowledge), FAB, swipe share/delete, empty states |
| Upload | `Util/SystemPickers`, `Networking/TransferManager` | Document picker + camera capture; background presigned-URL upload that survives backgrounding |
| Preview | `Features/Preview/*` | Inline image / PDF / text preview; native share sheet; offline copy served without network |
| Sharing | `Features/Sharing/*` | Share links with password/expiry/max-downloads, guest invite by email, permission management |
| Search | `Features/Search/*` | Debounced full-text search, file-type icons + path breadcrumbs, recent searches |
| Settings | `Features/Settings/*` | iam-core profile, storage usage bar, offline cache size + clear, biometric toggle, light/dark/system theme |
| Notifications | `Features/Notifications/*` | APNs registration + permission prompt, foreground/background handling, mark read/all-read |
| Transfers | `Features/Transfers/TransfersView` | Live upload/download progress with cancel + clear-finished |

---

## 3. Configuration

Baseline config is baked into `Info.plist` under the `ZKDriveConfig`
dictionary and overlaid at launch by the server `/api/config` endpoint (so a
deployment can rotate issuer / client_id without an app update). See
`AppConfig` / `AppConfigLoader`.

| Key | Meaning |
| --- | --- |
| `APIBaseURL` | zk-drive API base URL |
| `OIDCIssuer` | iam-core issuer (authorize/token derived as `oauth2/authorize`, `oauth2/token`) |
| `OIDCClientID` | OAuth2 public client id for the iOS app |
| `OIDCRedirectURI` | PKCE callback (custom scheme `zkdrive://oauth/callback`; matches `CFBundleURLSchemes`) |
| `OIDCScopes` | space-separated scopes (`openid profile email offline_access`) |

The OAuth redirect also supports a universal link via the
`applinks:` associated domain in `ZkDrive.entitlements`; replace the example
host with the deployment domain and serve the matching
`apple-app-site-association`.

### Required capabilities (`Info.plist` / entitlements)

- `NSFaceIDUsageDescription`, `NSCameraUsageDescription`, `NSPhotoLibraryUsageDescription`
- `BGTaskSchedulerPermittedIdentifiers` = `com.zkdrive.app.sync.refresh` (matches `BackgroundSyncScheduler.taskIdentifier`)
- `UIBackgroundModes`: `fetch`, `processing`, `remote-notification`
- `aps-environment`, `com.apple.developer.associated-domains`, `keychain-access-groups`

---

## 4. Building locally (macOS)

The project is generated from `project.yml` with
[XcodeGen](https://github.com/yonaskolb/XcodeGen) — the `.xcodeproj` is **not**
committed.

```bash
# 1. Build the Rust bridge XCFramework + generated Swift bindings.
#    (Requires the iOS Rust targets: rustup target add \
#     aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios)
cd mobile/rust-bridge && ./build-ios.sh

# 2. Generate and open the Xcode project.
cd ../ios
brew install xcodegen   # once
xcodegen generate
open ZkDrive.xcodeproj
```

Set your team via the `ZKDRIVE_DEVELOPMENT_TEAM` build setting (or in Xcode's
Signing & Capabilities) for device builds. Simulator builds need no signing.

### Command-line build + test

```bash
cd mobile/ios
xcodegen generate
xcodebuild \
  -project ZkDrive.xcodeproj -scheme ZkDrive -configuration Debug \
  -destination 'platform=iOS Simulator,name=iPhone 15,OS=latest' \
  CODE_SIGNING_ALLOWED=NO clean build test
```

> The Rust bridge cross-compile and the XCFramework link step require the
> Apple SDK + Xcode and therefore only run on macOS. On Linux,
> `build-ios.sh` still generates the Swift bindings and then stops before the
> macOS-only stages.

---

## 5. Tests

Unit tests live in `mobile/ios/ZkDriveTests` and cover the deterministic core:

- `PKCETests` — RFC 7636 verifier/challenge derivation (canonical vector).
- `AppErrorTests` — HTTP/bridge error → category, retry/re-auth semantics.
- `OAuthServiceTests` — token-request form encoding + token-response decoding.
- `ModelCodingTests` — snake_case API decoding, RFC3339 (incl. fractional)
  dates, forward-compatible `EncryptionMode` fallback.
- `FormatAndShareTargetTests` — byte formatting clamps; node → share-target
  mapping and collision-free list identities.

Run them via the `xcodebuild ... test` command above.

---

## 6. CI

[`.github/workflows/mobile-ios.yml`](../.github/workflows/mobile-ios.yml) runs
on a macOS runner: it installs the iOS Rust targets, runs `build-ios.sh` to
produce the XCFramework + bindings, installs XcodeGen, generates the project,
then builds and unit-tests the app on the iOS Simulator. It triggers only on
changes under `mobile/**`, `sdk/**`, or the workflow itself, and — like the
bridge workflow — is **not** a merge gate.
