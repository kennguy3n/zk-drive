# Mobile Rust FFI Bridge & Native Push

This document covers the shared foundation the native **iOS (Swift / SwiftUI)**
and **Android (Kotlin / Jetpack Compose)** apps build on:

1. The [`mobile/rust-bridge`](../mobile/rust-bridge) UniFFI crate that exposes
   the existing Rust SDK (crypto, auth, api-client, sync-engine) to both
   platforms from a single source of truth.
2. The server-side native push backend (`POST /api/push/register-device` +
   APNs / FCM fan-out from the notification publisher).

The native apps themselves (BGAppRefreshTask / WorkManager wiring, UI) are
delivered in follow-up workstreams; this is the contract they consume.

---

## 1. Rust FFI bridge (`mobile/rust-bridge`)

### 1.1 Why UniFFI

The bridge is a single [`uniffi`](https://mozilla.github.io/uniffi-rs/) crate
using the proc-macro (attribute) source of truth — there is **no hand-written
`.udl`**. The `#[uniffi::export]` / `#[derive(uniffi::Record|Enum|Object)]`
annotations on the Rust types are read straight out of the compiled library by
`uniffi-bindgen` in *library mode*, so the generated Swift and Kotlin bindings
can never drift from the symbols actually shipped in the `.so` / `.a`. Both
platforms therefore consume a byte-for-byte identical contract.

### 1.2 Module map

| Module | Backing SDK crate (`sdk/crates/…`) | FFI surface |
|--------|-------------------------------------|-------------|
| `crypto` | `crypto` | `CryptoEngine` — XChaCha20-Poly1305 `encrypt`/`decrypt`, object-context AAD binding; `generateDek()` |
| `auth`   | `auth`   | `TokenManager` — OAuth2 token storage + transparent refresh |
| `api`    | `api-client` | `ApiClient` — presigned upload/download/preview URLs, `/api/changes` changefeed |
| `sync`   | `sync-engine` | `SyncEngine` — local SQLite catalogue + changefeed polling, `ChangeObserver` callback |

### 1.3 Exported API

All UUIDs cross the FFI as hyphenated strings, all timestamps as Unix
milliseconds (`i64`), and binary data as byte arrays. Every fallible call
returns `BridgeError` (see §1.5).

#### `CryptoEngine` (object)
```text
constructor new(dek: bytes)                       -> CryptoEngine        // dek MUST be 32 bytes
constructor withObjectContext(dek, tenant,
        bucket, objectId, versionId, isMultipart)  -> CryptoEngine        // binds AAD to the object identity
encrypt(plaintext: bytes)                          -> bytes               // XChaCha20-Poly1305 seal
decrypt(ciphertext: bytes)                         -> bytes               // open + authenticate
// free function
generateDek()                                      -> bytes               // 32-byte CSPRNG data-encryption key
```
Crypto is CPU-bound and returns inline. `withObjectContext` binds the
ZK object-fabric AAD (tenant / bucket / object / version / multipart flag) into
the AEAD envelope, so a ciphertext sealed for one object identity fails the tag
check if opened under another.

#### `TokenManager` (object)
```text
constructor new(clientId: string, tokenUrl: string) -> TokenManager
setTokens(bundle: TokenBundle)                       -> void
accessToken()                                        -> string   // refreshes transparently if near expiry
snapshot()                                           -> TokenBundle?   // current bundle for secure persistence
hasTokens()                                          -> bool
clear()                                              -> void
```
`TokenBundle { accessToken, refreshToken, expiresAtUnix, scope }`. The native
layer is responsible for persisting `snapshot()` into the platform keychain /
keystore and re-hydrating with `setTokens` on launch.

#### `ApiClient` (object)
```text
constructor new(baseUrl: string, tokens: TokenManager) -> ApiClient
uploadUrl(folderId, filename, mimeType?)               -> UploadTarget    // POST /api/files/upload-url
confirmUpload(fileId, objectKey, sizeBytes, checksum?) -> string          // POST /api/files/confirm-upload -> new version id
downloadUrl(fileId)                                    -> DownloadTarget  // GET  /api/files/{id}/download-url
previewUrl(fileId)                                     -> PreviewTarget   // GET  /api/files/{id}/preview-url
getChanges(since: i64, limit?: u32)                    -> ChangePage      // GET  /api/changes?since=
```
The client mints presigned, direct-to-storage URLs; the native layer PUTs the
**already client-side-encrypted** bytes to `uploadUrl`, then calls
`confirmUpload` echoing the `fileId` + `objectKey`. Plaintext never reaches the
server.

#### `SyncEngine` (object) — background sync
```text
constructor new(cataloguePath, workspaceId, api)  -> SyncEngine
workspaceId()                                      -> string
cursor()                                           -> i64
setCursor(cursor)                                  -> void   // explicit resync/reset
commitCursor(cursor)                               -> void   // monotonic; rejects a regress
getChanges(since, limit?)                          -> ChangePage   // ad-hoc, does not touch cursor
fetchChanges(limit?)                               -> ChangePage   // next page from durable cursor, no advance
pollOnce(limit?)                                   -> ChangePage   // one tick: fetch + advance cursor
start(intervalMillis, limit?, observer)            -> void   // continuous foreground loop -> ChangeObserver
stop()                                             -> void
// local catalogue CRUD (offline file tree)
upsert(entry: FileEntry) / get(id) / byLocalPath(path)
setStatus(id, status) / setLocalState(...) / listAll()
pendingUploads() / pendingDownloads()
```
`ChangeObserver` is a **foreign-implemented** callback trait
(`onChanges(page)` / `onError(message)`) invoked from a background runtime
thread.

##### Background-sync contract
- **iOS `BGAppRefreshTask` / Android `WorkManager`**: call `pollOnce(limit)` per
  wake. It fetches the next page from the durable cursor and advances the cursor
  atomically, so it is the simple at-least-once primitive for short background
  windows.
- **Crash-safe apply**: for stronger ordering, call `fetchChanges()`, apply the
  page to your platform state, persist, then `commitCursor(page.cursor)`.
  `commitCursor` rejects a regressing cursor so a retried/out-of-order call can
  never rewind the feed.
- **Foreground**: `start(intervalMillis, limit, observer)` runs a continuous
  poll loop on the shared runtime and pushes pages to the `ChangeObserver`;
  `stop()` tears it down.

### 1.4 Threading contract

Crypto returns inline (CPU-bound). **Every method that does network or disk I/O
blocks the calling thread** while driving the work on a shared internal Tokio
runtime, so native callers MUST invoke them off the UI thread:

- iOS: `Task.detached` or a background `DispatchQueue`.
- Android: `Dispatchers.IO`.

`SyncEngine.start` is the one exception — it spawns its own background loop and
calls back via the `ChangeObserver`.

### 1.5 Error model

Every fallible call returns `BridgeError`, a flat typed enum the native layers
branch on:

| Variant | Meaning | Native handling |
|---------|---------|-----------------|
| `Network(msg)` | transport failure (offline, timeout, TLS) | retryable — back off and retry |
| `Api { status, msg }` | backend returned a non-2xx HTTP status | branch on `status` (401 ⇒ re-auth, 4xx ⇒ surface) |
| `Auth(msg)` | token missing / refresh failed | force re-login |
| `InvalidInput(msg)` | caller-side bug (bad UUID, wrong key size, …) | programmer error — fix the call site |
| `Storage(msg)` | local SQLite catalogue error | surface; usually transient disk pressure |

---

## 2. Cross-compilation & packaging

Build scripts live in `mobile/rust-bridge/` and emit reproducible artifacts.

### 2.1 Android — `build-android.sh`

```bash
export ANDROID_NDK_HOME=/path/to/android-ndk-r26d   # r26+ required
cd mobile/rust-bridge
./build-android.sh            # PROFILE=release, ANDROID_API=24 by default
```
Prerequisites:
```bash
rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android
cargo install cargo-ndk
```
Output (`build/android/`):
```
jniLibs/arm64-v8a/libzk_mobile_bridge.so
jniLibs/armeabi-v7a/libzk_mobile_bridge.so
jniLibs/x86_64/libzk_mobile_bridge.so
kotlin/uniffi/zk_mobile_bridge/zk_mobile_bridge.kt
```
The Android app puts `jniLibs/` on `sourceSets[...].jniLibs.srcDirs` and the
generated `.kt` on its source path (or wraps both in an AAR).

### 2.2 iOS — `build-ios.sh`

```bash
cd mobile/rust-bridge
./build-ios.sh                # PROFILE=release by default
```
Prerequisites:
```bash
rustup target add aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios
```
Output (`build/ios/`):
```
ZkMobileBridge.xcframework/                       (device arm64 + fat simulator slice)
Sources/ZkMobileBridge/zk_mobile_bridge.swift     (generated Swift API)
```
> The cross-compile + Swift binding-generation steps run on any host with the
> Apple targets installed, but the final `lipo` + `xcodebuild
> -create-xcframework` link step requires **macOS with Xcode**. On a non-macOS
> host the script stops with a clear message before the macOS-only stage rather
> than failing cryptically — this is why the XCFramework link runs on the macOS
> CI runner (see §4).

### 2.3 Binding generation only

To regenerate bindings without packaging (e.g. to inspect the contract):
```bash
cd mobile/rust-bridge
cargo build --release
cargo run --bin uniffi-bindgen -- generate \
  --library target/release/libzk_mobile_bridge.so \
  --language kotlin --out-dir /tmp/kt
cargo run --bin uniffi-bindgen -- generate \
  --library target/release/libzk_mobile_bridge.so \
  --language swift  --out-dir /tmp/swift
```

---

## 3. Server-side native push

See [`docs/CONFIGURATION.md`](./CONFIGURATION.md#native-mobile-push-apns--fcm)
for the full env-var reference. Summary below.

### 3.1 `POST /api/push/register-device`

Registers (upserts) the device token the platform push service hands the app so
the server can deliver notifications while the app is backgrounded or killed —
the mobile counterpart to the Web Push subscriptions used by the PWA.

- **Auth**: standard session bearer; the token is scoped to the authenticated
  `(workspace, user)`.
- **Body**:
  ```json
  { "platform": "ios" | "android", "token": "<APNs or FCM device token>" }
  ```
- **Responses**:
  | Status | When |
  |--------|------|
  | `204 No Content` | registered / refreshed (idempotent upsert) |
  | `400 Bad Request` | unknown `platform`, empty token, or token > 4096 bytes |
  | `401 Unauthorized` | missing / invalid session |
  | `501 Not Implemented` | the server has no provider configured for that platform |

`platform` maps to the delivery provider: `ios` → APNs, `android` → FCM.
Re-registering the same token is an upsert (refreshes `updated_at`), so the app
can register unconditionally on every cold start and OS token rotation.

### 3.2 `DELETE /api/push/register-device`

Same body shape; removes the token (on sign-out / when the user disables
notifications). Idempotent — deleting an absent token still returns `204`.

### 3.3 Delivery & lifecycle

- The notification publisher fans every `notification` event out across
  **WebSocket → Web Push → native push** (APNs + FCM) so one event reaches a
  live tab, an installed PWA, and the user's phones.
- **Fail-soft & pluggable**: an unconfigured provider is simply skipped; a
  per-token provider error is logged but never aborts the notification or blocks
  the other tokens. Fan-out runs in a detached goroutine with a per-push timeout,
  so push latency never delays the HTTP response that triggered the event.
- **Dead-token pruning**: a permanent "this token is dead" signal — APNs `410
  Unregistered` / `400 BadDeviceToken` / `DeviceTokenNotForTopic`, or FCM
  `UNREGISTERED` / `SENDER_ID_MISMATCH` — deletes the row automatically, exactly
  mirroring how a Web Push `410 Gone` subscription is pruned. Transient failures
  (auth-token expiry, 5xx, network) are retried on the next event and never
  prune.
- **Isolation**: tokens live in the `device_push_tokens` table (migration 039)
  with the same row-level-security `tenant_isolation` policy as the rest of the
  schema, so a workspace can only ever see its own device tokens.

---

## 4. CI scaffolding

`.github/workflows/mobile-bridge.yml` builds the bridge for every target
architecture and generates the Swift + Kotlin bindings on each push touching
`mobile/`, `sdk/`, or the workflow itself:

- **Linux job**: installs the Android targets + `cargo-ndk` + NDK, runs
  `cargo test`, `build-android.sh`, and generates both Swift and Kotlin bindings
  (library mode works cross-platform), uploading the jniLibs + bindings as
  artifacts.
- **macOS job**: installs the Apple targets and runs `build-ios.sh` to assemble
  the `ZkMobileBridge.xcframework`, uploading it as an artifact.

The workflow is **not a merge gate** — it is scaffolding for the follow-up iOS /
Android app workstreams to consume.
