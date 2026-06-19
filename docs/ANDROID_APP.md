# Android app

The Android client is a native **Kotlin / Jetpack Compose** app that puts a
Northwind Trading employee's workspace on their phone: browse folders, open and
preview files, upload from the camera or the system file picker, manage share
links, and keep an offline copy that stays current in the background.

It is a **thin shell**. Every security-critical operation — sealing a file,
holding tokens, refreshing them, minting transfer URLs, tracking the change
feed — runs in the shared [Rust bridge](./MOBILE_BRIDGE.md), the same library
the iOS app and the desktop sync client use. The Kotlin code orchestrates UI,
storage, and platform plumbing; it never reimplements cryptography.

The app targets **Android 12 and above** (`minSdk = 31`), built and tested
against `targetSdk` / `compileSdk` 35 (`mobile/android/app/build.gradle.kts:84-94`).

---

## How it is put together

The app follows MVVM with a repository layer, wired by Hilt. ViewModels depend
only on repositories; repositories own the bridge handles, the network clients,
and platform storage.

```text
ui/        Jetpack Compose screens + one ViewModel each
  MainActivity   single-activity host; gates on AuthState, drives biometric unlock
  ZkApp          bottom-nav scaffold + NavHost (Files / Search / Settings)
  login/ browser/ preview/ share/ search/ settings/
data/      repositories — the only thing ViewModels touch
  auth/      OAuth2 PKCE (AppAuth), EncryptedSharedPreferences token store
  drive/     folder tree, presigned transfers, client-side crypto, DEK custody
  remote/    Retrofit REST surface + bearer interceptor
  sync/      WorkManager scheduler + worker + the sync coordinator
  push/ search/ sharing/ settings/
domain/    framework-free models (FileNode, FolderNode, EncryptionMode, …)
bridge/    thin wrappers over the UniFFI bridge (BridgeHolder, BridgeSession, CryptoProvider)
di/        Hilt modules (network, dispatchers, scopes)
```

### One signed-in session owns the native handles

When the user signs in, the app constructs three native bridge objects bound to
that user and workspace — a `TokenManager`, an `ApiClient`, and a `SyncEngine` —
and wraps them in a `BridgeSession`
(`mobile/android/app/src/main/java/com/zkdrive/app/bridge/BridgeSession.kt:18-23`).
A process-wide `BridgeHolder` holds the one live session; `AuthRepository` is
the single source of truth for "is the user signed in"
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/AuthRepository.kt:43-64`).

Because the native handles are freed on sign-out, every operation that touches
them first takes a **lease**. `BridgeSession.acquire()` returns `false` if a
concurrent logout already retired the session, and disposal of the Rust handles
is deferred until the last in-flight lease is released — so an upload or a sync
tick can never touch a freed handle
(`mobile/android/app/src/main/java/com/zkdrive/app/bridge/BridgeSession.kt:28-69`).
Disposal order is fixed (engine, then API client, then token manager) because
the engine borrows the API client.

The shared `TokenManager` is what keeps the REST and transfer paths honest: the
OkHttp `AuthInterceptor` reads the bearer from the same `TokenManager` the
bridge `ApiClient` uses, so the two transports can never disagree about the
active token, and refresh logic lives in exactly one place — the Rust auth
module (`mobile/android/app/src/main/java/com/zkdrive/app/data/remote/AuthInterceptor.kt:10-49`).

---

## Signing in

Sign-in is OAuth2 Authorization Code with **PKCE**, implemented with
AppAuth-Android over Chrome Custom Tabs and a deep-link redirect
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/OAuthService.kt:34-48`).
PKCE is mandatory: AppAuth generates a random `code_verifier` per request and
only sends its S256 challenge to the authorize endpoint, so an intercepted
authorization code is useless without the in-process verifier.

The flow:

1. **Resolve the IdP.** The app prefers OIDC discovery from the configured
   issuer, and falls back to iam-core's documented `/oauth2/authorize` and
   `/oauth2/token` endpoints if the discovery document is unreachable, so
   sign-in still works offline of discovery
   (`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/OAuthService.kt:50-77`).
2. **Authorize in a Custom Tab.** `MainActivity` launches the authorization
   intent (a ViewModel cannot start activities) and receives the redirect back
   through an `ActivityResultLauncher`
   (`mobile/android/app/src/main/java/com/zkdrive/app/ui/MainActivity.kt:46-65`).
   The redirect scheme is injected into the manifest from the build config, so
   a deployment can rebrand the callback without editing the manifest
   (`mobile/android/app/src/main/AndroidManifest.xml:51-64`).
3. **Exchange the code** for a `TokenBundle` and resolve the user's workspace by
   calling `GET /api/workspaces` and selecting the first accessible workspace
   (`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/AuthRepository.kt:88-121`,
   `:219-238`).
4. **Persist the session** encrypted at rest and route to the file browser.

The deeper OAuth/OIDC behavior is server-side; see [`IAM_CORE.md`](./IAM_CORE.md)
for the identity surface. The app describes only the client side.

### Token storage and biometric unlock

Tokens and session metadata are written to `EncryptedSharedPreferences`
(AES-256-GCM values, AES-256-SIV keys) under the file `zk_secure_tokens`, with
the master key held in the Android Keystore (StrongBox when the device offers
it). Token material never touches disk in plaintext, and the master key is
non-exportable. A corrupt keystore entry — e.g. after a device restore — drops
the file and starts clean so the user simply signs in again rather than being
hard-locked out
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/TokenStore.kt:28-63`).

On launch the app re-seeds the bridge `TokenManager` from this store with no
network round-trip. If the user has enabled biometric lock, `MainActivity`
gates the session behind a `BiometricPrompt` (strong biometric or device
credential), falling back to materializing the session directly when no
biometric is enrolled — the tokens are still encrypted at rest either way
(`mobile/android/app/src/main/java/com/zkdrive/app/ui/MainActivity.kt:94-120`).

After the bridge transparently refreshes an access token, the app reads
`TokenManager.snapshot()` and writes the refreshed bundle back to the encrypted
store — driven from `MainActivity.onStop()` so a refresh that happened during
the session is durable
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/AuthRepository.kt:147-159`).

---

## Privacy modes and client-side encryption

Every folder carries an `EncryptionMode` that the browser shows as a badge and
the upload path branches on
(`mobile/android/app/src/main/java/com/zkdrive/app/domain/DriveModels.kt:3-16`):

| Mode | Wire value | What the app does |
| --- | --- | --- |
| **Confidential** | `managed_encrypted` | Streams plaintext straight into the presigned PUT; the gateway manages envelope encryption server-side. |
| **Zero-Knowledge** | `strict_zk` | Seals the bytes client-side with the bridge `CryptoEngine` **before** the PUT; the server only ever stores opaque ciphertext. |

`managed_encrypted` is the default and lets the server generate previews,
thumbnails, search, and malware scanning. `strict_zk` content is sealed on the
phone, so **the server cannot preview, index, search, or scan it** — that is the
deliberate trade-off, stated plainly wherever the mode appears.

For a `strict_zk` upload the app
(`mobile/android/app/src/main/java/com/zkdrive/app/data/drive/TransferManager.kt:60-124`,
`:154-185`):

1. Mints a fresh random 32-byte DEK via the bridge (`generateDek()`).
2. Builds an object-context `CryptoEngine` whose additional authenticated data
   (AAD) is the canonical `tenantId | bucket | objectKeyHashHex | versionId`
   tuple. The presigned `objectKey` is used as the version component, because
   it is the unique-per-upload storage identity the gateway mints up front
   (the server's version id only exists after `confirmUpload`). Binding to it
   pins the ciphertext to this specific upload.
3. Seals the plaintext, PUTs the ciphertext, then calls
   `ApiClient.confirmUpload(...)` to record the version.
4. **Only after the confirm succeeds** persists the DEK, so a failed or retried
   upload never leaves an orphaned key.

The bridge AEAD is single-shot, so a `strict_zk` upload buffers plaintext and
ciphertext in memory (bounded by the file size); `managed_encrypted` uploads
stream from disk and hash in a streaming pass, keeping heap flat regardless of
size.

### Device-local key custody (an honest limit)

Strict-ZK DEKs are held in `FileKeyStore`, again on `EncryptedSharedPreferences`
with an Android Keystore master key, in two stores: `zk_object_keys` maps each
`objectKey` to its sealed `EnvelopeKey`, and `zk_object_key_index` maps a
`fileId` to the set of object keys it has produced, so deleting a file purges
every version's key
(`mobile/android/app/src/main/java/com/zkdrive/app/data/drive/FileKeyStore.kt:50-103`).

These keys are **device-local by design**: the same device that sealed a
strict-ZK object can transparently decrypt its downloads, but another device
cannot until cross-device key escrow is delivered by the server key-wrap
surface. That is why the DEKs are excluded from cloud backup, and why the
inability to open your own strict-ZK file on a second device is expected
behavior rather than a bug
(`mobile/android/app/src/main/java/com/zkdrive/app/data/drive/FileKeyStore.kt:50-59`).

---

## Transfers

All byte movement is **direct to object storage** over presigned URLs minted by
the bridge `ApiClient`; the app server never proxies file bytes. Downloads are
decrypted in place when the device holds the DEK, then materialized in the app
cache and handed to other apps as a `content://` URI through a `FileProvider`,
namespaced by file id so two files whose names sanitize to the same string do
not collide
(`mobile/android/app/src/main/java/com/zkdrive/app/data/drive/TransferManager.kt:126-149`,
`:223-235`).

Folder navigation, search, sharing, permissions, and push registration use a
small Retrofit REST surface (`ZkDriveApi`); transfers and the change feed go
through the bridge instead
(`mobile/android/app/src/main/java/com/zkdrive/app/data/remote/ZkDriveApi.kt:26-31`).
The system share sheet is wired as a `SEND` target so a file from another app
can be uploaded into the current workspace
(`mobile/android/app/src/main/AndroidManifest.xml:42-48`).

---

## Offline catalogue and background sync

The bridge `SyncEngine` keeps a local SQLite catalogue per workspace at
`filesDir/catalogue/<workspaceId>.db`, so the file tree and the change-feed
cursor survive launches
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/AuthRepository.kt:213-217`).

`SyncCoordinator` runs one sync pass by calling `SyncEngine.pollOnce(200)` in a
loop — each call fetches the next change-feed page from the durable cursor and
advances it — draining up to **25 pages** per run, then persists the refreshed
token snapshot. Each mutation is reflected into the catalogue: a known file that
changed upstream is flagged `RemoteDirty`, a delete becomes `RemoteDeleted`, and
an unknown file is inserted as `RemoteDirty` to be pulled later. The pass is
safely re-entrant because the catalogue is keyed by remote file id and the
cursor only moves forward
(`mobile/android/app/src/main/java/com/zkdrive/app/data/sync/SyncCoordinator.kt:36-98`).

`SyncScheduler` enqueues a **15-minute periodic** `SyncWorker` through
WorkManager, constrained by the user's preferences — Wi-Fi-only maps to an
unmetered network requirement, charging-only to a charging requirement — and
reschedules whenever those settings change. The worker is a no-op success when
signed out (so WorkManager does not retry pointlessly) and asks for a backoff
retry on transient network or storage errors
(`mobile/android/app/src/main/java/com/zkdrive/app/data/sync/SyncScheduler.kt:21-52`,
`mobile/android/app/src/main/java/com/zkdrive/app/data/sync/SyncWorker.kt:12-38`).
Expedited uploads can run as a `dataSync` foreground service so they survive
backgrounding on Android 14+
(`mobile/android/app/src/main/AndroidManifest.xml:105-111`).

---

## Push notifications

Native push uses Firebase Cloud Messaging, initialized **at runtime** from
build-injected config rather than the google-services Gradle plugin — so the
app builds and runs cleanly with no committed `google-services.json`. When the
`FCM_*` config is absent (local dev), push is simply disabled, never a crash
(`mobile/android/app/src/main/java/com/zkdrive/app/push/PushInitializer.kt:14-56`).

When configured, the app fetches the device token and registers it with the
server via `POST /api/push/register-device` (payload `{ platform: "android",
token }`), forwards token rotations the same way, and on sign-out unregisters
the token with `DELETE /api/push/register-device` before deleting it locally.
Registration failures are swallowed — push is best-effort and must never break
the foreground experience
(`mobile/android/app/src/main/java/com/zkdrive/app/data/push/PushRepository.kt:11-27`,
`mobile/android/app/src/main/java/com/zkdrive/app/data/push/PushInitializer.kt:58-71`).

Incoming messages are posted to one of three notification channels — push,
transfers, sync — registered at application start, and tapping a push deep-links
back into the app (`POST_NOTIFICATIONS` is honored, so nothing is posted without
the runtime permission)
(`mobile/android/app/src/main/java/com/zkdrive/app/ZkDriveApplication.kt:35-55`,
`mobile/android/app/src/main/java/com/zkdrive/app/push/ZkFirebaseMessagingService.kt:42-79`).

---

## Privacy on sign-out

Logout is a clean teardown: it cancels periodic sync, retires and disposes the
bridge session, clears the encrypted token store, **wipes every device-local
DEK** so the next account on the device cannot reach the previous user's
strict-ZK keys, and purges the entire app cache so no decrypted download, staged
upload body, or camera capture survives the sign-out
(`mobile/android/app/src/main/java/com/zkdrive/app/data/auth/AuthRepository.kt:161-186`).

---

## Building

The Gradle build drives the Rust bridge automatically. The `buildRustBridge`
task runs [`build-android.sh`](../mobile/rust-bridge/build-android.sh) to
cross-compile `libzk_mobile_bridge.so` for every shipped ABI
(`arm64-v8a`, `armeabi-v7a`, `x86_64`) and regenerate the Kotlin bindings, then
puts both on the app's source sets. Kotlin compilation depends on that task, so
a plain build produces the bridge first
(`mobile/android/app/build.gradle.kts:40-63`, `:174-203`).

You therefore need the Rust Android toolchain available:

```bash
rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android
cargo install cargo-ndk
# Android NDK r26d — point the build at it via either:
#   local.properties:  ndk.dir=/path/to/android-ndk-r26d
#   or env var:        export ANDROID_NDK_HOME=/path/to/android-ndk-r26d
```

Then, from `mobile/android`:

```bash
./gradlew :app:assembleDebug      # build the debug APK (and the bridge)
./gradlew :app:lintDebug          # Android lint
./gradlew :app:compileDebugKotlin # fast compile check
```

The app pins `ndkVersion = 26.3.11579264` (r26d) — the same NDK
`build-android.sh` cross-compiles with — so AGP can strip debug symbols from the
shipped `.so` files (`mobile/android/app/build.gradle.kts:86-89`). The UniFFI
Kotlin bindings call into the native library through JNA
(`mobile/android/app/build.gradle.kts:246-247`).

---

## Configuration

OAuth, API, and Firebase values come from a git-ignored `oauth.properties` on
developer machines or environment variables in CI — never hard-coded secrets.
Safe non-secret placeholders let a fresh checkout configure and compile
(`mobile/android/app/build.gradle.kts:65-126`).

| `oauth.properties` key | Env var | Purpose |
| --- | --- | --- |
| `oidc.issuer` | `ZK_OIDC_ISSUER` | iam-core OIDC issuer (discovery base) |
| `oidc.clientId` | `ZK_OIDC_CLIENT_ID` | Public OAuth client id (default `zk-drive-android`) |
| `oidc.scope` | `ZK_OIDC_SCOPE` | Requested scopes (`openid profile email offline_access`) |
| `oidc.redirectScheme` | `ZK_OIDC_REDIRECT_SCHEME` | Redirect scheme; the callback is `<scheme>:/oauth2redirect` |
| `api.baseUrl` | `ZK_API_BASE_URL` | REST + bridge API origin |
| `web.baseUrl` | `ZK_WEB_BASE_URL` | Public web origin serving `/share/{token}` |
| `fcm.projectId`, `fcm.appId`, `fcm.apiKey`, `fcm.senderId` | `ZK_FCM_*` | Firebase config; push is disabled when any is blank |

The redirect scheme feeds both the AppAuth redirect URI and the manifest
intent-filter through `manifestPlaceholders`.

---

## Release signing

No keystore is committed. Release signing reads an upload keystore entirely from
environment variables; when they are absent the release variant builds unsigned,
so a local `assembleRelease` still works
(`mobile/android/app/build.gradle.kts:128-157`).

| Env var | Meaning |
| --- | --- |
| `ZK_ANDROID_KEYSTORE` | Path to the upload keystore file |
| `ZK_ANDROID_KEYSTORE_PASSWORD` | Keystore password |
| `ZK_ANDROID_KEY_ALIAS` | Key alias |
| `ZK_ANDROID_KEY_PASSWORD` | Key password |

---

## Continuous integration

[`.github/workflows/mobile-android.yml`](../.github/workflows/mobile-android.yml)
sets up JDK 17, the Rust Android targets, `cargo-ndk`, and NDK r26d, then runs
`:app:lintDebug` and `:app:assembleDebug` — which transitively build the Rust
bridge. When the `ZK_ANDROID_KEYSTORE_BASE64` secret is present it decodes the
upload keystore and also assembles a signed release; on forks without the
secret the release step is skipped and the debug APK is uploaded as the
artifact. The workflow runs only when `mobile/android/**`,
`mobile/rust-bridge/**`, `sdk/**`, or the workflow file change, and is **not a
merge gate** (`.github/workflows/mobile-android.yml:7-22`, `:39-105`).
