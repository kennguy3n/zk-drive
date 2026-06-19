# Mobile Rust bridge

The iOS and Android apps do not reimplement zk-drive's cryptography, token
handling, HTTP contract, or sync bookkeeping. They link **one shared Rust
library** — the [`mobile/rust-bridge`](../mobile/rust-bridge) crate — and call
into it through generated bindings. The same Rust code that powers the desktop
sync client (the [`sdk/crates`](../sdk) workspace) is what runs inside the
phone.

That single source of truth is the point: a file sealed on an Android phone
opens on an iPhone, on the desktop client, and on the server, because every one
of them uses the exact same envelope code. The bridge is the seam that makes
the two native apps thin.

This document is the contract the native shells consume. The shells themselves
(SwiftUI screens, Jetpack Compose screens, background-task wiring) are
documented in [`MOBILE_IOS.md`](./MOBILE_IOS.md) and
[`ANDROID_APP.md`](./ANDROID_APP.md).

---

## What the bridge is

`mobile/rust-bridge` is a single [UniFFI](https://mozilla.github.io/uniffi-rs/)
crate (`uniffi = "0.29"`, `mobile/rust-bridge/Cargo.toml:38`). It wraps four
SDK crates and re-exports them across the foreign-function-interface (FFI)
boundary:

| Bridge module | Backing SDK crate | What the native app gets |
| --- | --- | --- |
| `crypto` | [`zk-sync-crypto`](../sdk/crates/crypto) | `CryptoEngine` — XChaCha20-Poly1305 seal/open, plus `generateDek()` |
| `auth` | [`zk-sync-auth`](../sdk/crates/auth) | `TokenManager` — OAuth2 token store with transparent refresh |
| `api` | [`zk-sync-api`](../sdk/crates/api-client) | `ApiClient` — presigned upload/download/preview URLs and the change feed |
| `sync` | [`zk-sync-engine`](../sdk/crates/sync-engine) | `SyncEngine` — local SQLite catalogue and change-feed polling |

The module map is declared in `mobile/rust-bridge/src/lib.rs:37-50`.

### One source of truth, two platforms

The bridge uses UniFFI's **proc-macro** mode — there is no hand-written `.udl`
interface file. The `#[uniffi::export]`, `#[derive(uniffi::Record)]`,
`#[derive(uniffi::Enum)]`, and `#[derive(uniffi::Object)]` attributes on the
Rust types *are* the interface definition. `uniffi-bindgen` reads the contract
back out of the compiled library (`--library` mode), so the generated Swift and
Kotlin bindings can never drift from the symbols actually shipped in the `.so`
/ `.a`. Both platforms consume a byte-for-byte identical contract generated
from the same crate metadata (`mobile/rust-bridge/src/lib.rs:1-8`).

The crate builds as three library types from that one source
(`mobile/rust-bridge/Cargo.toml:16-18`):

- `cdylib` — the `.so` Android loads with `System.loadLibrary`.
- `staticlib` — the `.a` linked into the iOS app via an XCFramework.
- `lib` — so `cargo test` exercises the bridge in-process and the pinned
  `uniffi-bindgen` binary can share the crate.

---

## Threading contract

This is the single rule a native developer must internalize before calling the
bridge.

- **Crypto is CPU-bound and returns inline** on the calling thread. No runtime,
  no blocking on I/O.
- **Every method that touches the network or disk blocks the calling thread**
  while it drives the work to completion on a shared internal Tokio runtime.
  Native callers MUST invoke these off the platform UI thread — iOS
  `Task.detached` or a background `DispatchQueue`; Android `Dispatchers.IO`
  (`mobile/rust-bridge/src/lib.rs:19-27`).
- **`SyncEngine.start` is the one exception.** It spawns its own background
  loop on the shared runtime and delivers results by calling back into a
  foreign `ChangeObserver`, so the caller does not block
  (`mobile/rust-bridge/src/sync.rs:275-321`).

The shared runtime is process-wide and built on first use: a multi-threaded
Tokio runtime capped at **4 worker threads**, thread-named `zk-bridge`, with
all drivers enabled (`mobile/rust-bridge/src/runtime.rs:39-48`). The cap is
deliberate — a phone's bridge workload is I/O-bound (HTTPS to the backend,
SQLite on a blocking thread), so a larger pool would waste memory without
improving throughput. The runtime lives for the life of the process; there is
no "shut down the whole bridge" step on mobile short of process death
(`mobile/rust-bridge/src/runtime.rs:15-23`).

The bridge exposes a **blocking** FFI rather than `async fn` on purpose: mobile
network and crypto work already runs off the main thread on both platforms, so
a blocking call that internally drives async work on the shared runtime is the
simpler, stabler contract than asking each platform to host a foreign async
executor (`mobile/rust-bridge/src/runtime.rs:1-13`).

---

## Error model

Every fallible call returns `BridgeError`, a flat typed enum
(`mobile/rust-bridge/src/error.rs:20-54`). UniFFI flattens it into a Swift
`enum BridgeError: Error` and a Kotlin `sealed class BridgeException`, so the
native layers branch on the category instead of parsing strings.

| Variant | Meaning | Native handling |
| --- | --- | --- |
| `Crypto(message)` | bad key length, truncated ciphertext, or AEAD tag mismatch | not retryable — the bytes or key are wrong |
| `Auth(message)` | no token set, or refresh failed / token revoked | drive the user back through sign-in |
| `Network(message)` | DNS, TLS, timeout, connection reset | retryable with backoff |
| `Api { status, message }` | backend returned a non-2xx HTTP status | branch on `status` (401 ⇒ re-auth, 403 ⇒ permission, 404 ⇒ gone, 5xx ⇒ retry) |
| `Catalogue(message)` | local SQLite catalogue failure (open, migration, query) | surface; usually transient disk pressure |
| `InvalidInput(message)` | malformed UUID, empty path, wrong key size | a programming error on the native side — fix the call site |

The category split is what matters: the `From` conversions map each underlying
crate error into the right bucket so call sites stay clean. A transport failure
*during a token refresh* is mapped to `Network` (retryable), not `Auth`, so a
flaky link does not bounce the user to the sign-in screen
(`mobile/rust-bridge/src/error.rs:62-100`). An HTTP error body echoed back
through `Api` is capped at 512 bytes on a UTF-8 boundary so a misbehaving
server cannot push an unbounded string into the native UI
(`mobile/rust-bridge/src/error.rs:108-123`).

---

## Exported API

UniFFI conventions across the whole surface: UUIDs cross the FFI as hyphenated
strings, timestamps as Unix values (`i64`), and binary data as byte arrays.
Method names render in each platform's idiom (`with_object_context` becomes
`withObjectContext` in Swift/Kotlin). The signatures below use the
platform-binding spelling.

### `CryptoEngine` (module `crypto`)

```text
constructor new(dek: bytes) -> CryptoEngine
constructor withObjectContext(dek, tenantId, bucket,
        objectKeyHashHex, versionId, convergentNonce) -> CryptoEngine
encrypt(plaintext: bytes) -> bytes
decrypt(ciphertext: bytes) -> bytes

generateDek() -> bytes   // free function: a fresh random 32-byte key
```

A `CryptoEngine` is bound to one 32-byte data-encryption key (DEK) and a fixed
AAD / nonce policy. `new` rejects any DEK that is not exactly 32 bytes with
`InvalidInput` (`mobile/rust-bridge/src/crypto.rs:42-50`, `:126-134`).

- `new(dek)` seals with random nonces and no per-chunk additional authenticated
  data (AAD).
- `withObjectContext(...)` binds every chunk to its object identity via the
  canonical `tenantId | bucket | objectKeyHashHex | versionId` AAD. Ciphertext
  sealed under one object context fails the tag check if opened under another —
  a chunk cannot be replayed under a different object key. This is the form
  uploads use (`mobile/rust-bridge/src/crypto.rs:52-85`). The bridge's own test
  proves the property: changing only `versionId` makes `decrypt` fail
  (`mobile/rust-bridge/src/lib.rs:70-96`).
- `convergentNonce` (default behavior is off) switches to deterministic,
  content-derived nonces for intra-tenant deduplication. It trades away forward
  secrecy for stored ciphertext, so it is only set when the workspace has opted
  into convergent encryption (`mobile/rust-bridge/src/crypto.rs:59-62`).

`encrypt`/`decrypt` process the whole buffer in one call — correct for the
typical mobile case (documents, photos). The ciphertext is byte-identical to
every other zk-drive client for the same DEK and AAD, which is what lets a file
uploaded from a phone download cleanly on the desktop and vice versa
(`mobile/rust-bridge/src/crypto.rs:1-8`). `generateDek()` mints a fresh random
32-byte key without the native side reimplementing a CSPRNG
(`mobile/rust-bridge/src/crypto.rs:117-124`).

### `TokenManager` (module `auth`)

```text
constructor new(clientId: string, tokenUrl: string) -> TokenManager
setTokens(bundle: TokenBundle) -> void
accessToken() -> string             // refreshes transparently near expiry
snapshot() -> TokenBundle?          // current bundle, for secure persistence
hasTokens() -> bool
clear() -> void                     // sign-out; idempotent
```

`TokenBundle { accessToken, refreshToken, expiresAtUnix, scope }`
(`mobile/rust-bridge/src/auth.rs:31-40`). `new` validates `tokenUrl` eagerly so
a typo fails at construction rather than on the first silent refresh
(`mobile/rust-bridge/src/auth.rs:80-89`). `accessToken()` returns a still-valid
token, refreshing transparently when the stored one is within a 60-second
expiry skew, and blocks on the refresh network round-trip when one is needed
(`mobile/rust-bridge/src/auth.rs:98-108`).

The bridge keeps tokens in an **in-memory** store, not in a keychain. On mobile
the platform owns secure storage (iOS Keychain, Android
`EncryptedSharedPreferences` / Keystore), and the generic `keyring` crate the
desktop client uses has no first-class iOS/Android backend. So the contract is
explicit (`mobile/rust-bridge/src/auth.rs:1-17`):

1. After sign-in, the native layer persists the bundle in platform-secure
   storage.
2. On launch it re-seeds the bridge with `setTokens(...)`.
3. The bridge refreshes access tokens on demand; the native layer reads
   `snapshot()` after an authenticated call to pick up a freshly refreshed
   token and write it back to secure storage.

The refresh endpoint is the backend's absolute `/oauth/token` URL — for the
demo deployment, `https://<host>/api/auth/oauth/token`. The deeper OAuth/OIDC
behavior lives server-side; see [`IAM_CORE.md`](./IAM_CORE.md).

### `ApiClient` (module `api`)

```text
constructor new(baseUrl: string, tokens: TokenManager) -> ApiClient
uploadUrl(folderId, filename, mimeType?) -> UploadTarget        // POST /api/files/upload-url
confirmUpload(fileId, objectKey, sizeBytes, checksum?) -> string // POST /api/files/confirm-upload -> version id
downloadUrl(fileId) -> DownloadTarget                            // GET  /api/files/{id}/download-url
previewUrl(fileId) -> PreviewTarget                              // GET  /api/files/{id}/preview-url
getChanges(since: i64, limit?: u32) -> ChangePage                // GET  /api/changes?since=&limit=
```

The client mints presigned, direct-to-storage URLs. The native layer PUTs the
**already client-side-encrypted** bytes straight to `uploadUrl`, then calls
`confirmUpload` echoing the `fileId` and `objectKey`; the confirm records the
new current version and returns its id. Skipping the confirm leaves the file
row version-less and later downloads return 404
(`mobile/rust-bridge/src/api.rs:124-201`). Plaintext never reaches the server.

`getChanges` fetches one catch-up page of change-feed mutations with
`sequence > since`; the workspace is resolved server-side from the bearer
token, not passed in the path (`mobile/rust-bridge/src/api.rs:238-243`). The
records it returns:

- `UploadTarget { uploadUrl, fileId, objectKey }`
- `DownloadTarget { downloadUrl, objectKey }`
- `PreviewTarget { previewUrl, objectKey, mimeType }`
- `ChangePage { mutations: [ChangeRecord], cursor, hasMore }` — `cursor` is the
  sequence to pass as `since` on the next poll; `hasMore` means the server
  truncated the page at the limit and the caller should poll again immediately
  rather than wait for the next interval
  (`mobile/rust-bridge/src/api.rs:55-64`).
- `ChangeRecord { sequence, workspaceId, actorId?, kind, op, resourceId, parentId?, name, metadataJson?, occurredAtUnixMs }`
  — UUIDs rendered as strings, the timestamp as Unix-millis, and `metadata`
  passed through as its raw JSON string so the native layer parses lazily
  (`mobile/rust-bridge/src/api.rs:66-99`). The `kind`/`op` vocabulary is the
  server's change feed; see [`../sdk/README.md`](../sdk/README.md).

`ApiClient` wraps `zk_sync_api::Client` purely for its authenticated transport
(bearer injection via the shared `TokenManager`, TLS, timeouts) and then speaks
the backend's deployed REST contract directly. The SDK's higher-level
`StorageClient` / `FsClient` target a different `/api/v1/...` surface that this
server does not expose, so the bridge calls the deployed `/api/...` endpoints
itself (`mobile/rust-bridge/src/api.rs:1-11`). File-id path segments are
validated against slash / dot-dot / control-character injection before they go
into a URL (`mobile/rust-bridge/src/api.rs:327-342`).

### `SyncEngine` (module `sync`)

```text
constructor new(cataloguePath, workspaceId, api) -> SyncEngine
workspaceId() -> string
cursor() -> i64
setCursor(cursor) -> void           // explicit resync / reset
commitCursor(cursor) -> void        // monotonic; rejects a regress
getChanges(since, limit?) -> ChangePage   // ad-hoc; does not touch the cursor
fetchChanges(limit?) -> ChangePage        // next page from the durable cursor, no advance
pollOnce(limit?) -> ChangePage            // one tick: fetch + advance the cursor
start(observer, intervalMillis, limit?) -> void  // continuous foreground loop
stop() -> void
// local catalogue (the offline file tree)
upsert(entry) / get(id) / byLocalPath(path)
setStatus(id, status) / setLocalState(id, status, contentHashHex, sizeBytes)
listAll() / pendingUploads() / pendingDownloads()
```

`SyncEngine` opens (creating if absent) a SQLite catalogue at `cataloguePath`,
bound to one workspace, polling through an `ApiClient`. The catalogue path
should live in the platform's app-support directory so it survives launches and
can be excluded from cloud backup (`mobile/rust-bridge/src/sync.rs:179-204`).

A `FileEntry` is one catalogue row:
`{ remoteFileId, remoteVersionId, localPath, sizeBytes, contentHashHex, status, pinned, updatedAtUnixMs }`.
`contentHashHex` is the lowercase hex of a 32-byte content hash; an empty string
is rejected on upsert, and `pinned` files are exempt from offline-cache LRU
eviction (`mobile/rust-bridge/src/sync.rs:91-109`).

`status` is a `SyncStatus` enum with eight variants
(`mobile/rust-bridge/src/sync.rs:49-59`):

| Variant | Meaning |
| --- | --- |
| `UpToDate` | local and remote agree |
| `LocalDirty` | local edit awaiting upload |
| `LocalDeleted` | local delete awaiting upload |
| `RemoteDirty` | remote change awaiting download |
| `RemoteDeleted` | remote delete awaiting local unlink |
| `Conflict` | local and remote both changed |
| `InFlight` | a transfer is running |
| `Evicted` | content dropped from the offline cache |

#### Background-sync contract

The poll cursor lives in the catalogue's SQLite, so it survives process death.
Three patterns drive sync, from simplest to strictest:

- **One-shot wake** (iOS `BGAppRefreshTask`, Android `WorkManager`): call
  `pollOnce(limit)`. It fetches the next page from the durable cursor and
  advances the cursor in one step — the simple at-least-once primitive for a
  short background window (`mobile/rust-bridge/src/sync.rs:256-268`).
- **Crash-safe apply**: call `fetchChanges()`, apply the page to platform
  state, persist it, then `commitCursor(page.cursor)`. `commitCursor` rejects a
  regressing cursor so a retried or out-of-order call can never rewind the feed
  and re-deliver old mutations forever (`mobile/rust-bridge/src/sync.rs:225-238`).
- **Continuous foreground**: `start(observer, intervalMillis, limit)` runs a
  poll loop on the shared runtime, delivering non-empty pages to a foreign
  `ChangeObserver` and advancing the cursor after each. The interval is floored
  at 1000 ms to protect the backend, the call is idempotent (a second `start`
  while running is a no-op), and `stop()` tears the loop down
  (`mobile/rust-bridge/src/sync.rs:270-335`).

Because `pollOnce` advances the cursor only after the page reaches the caller,
a crash between "page returned" and "platform persisted the changes"
re-delivers that page on the next wake. Change application must therefore be
**idempotent on `sequence`** — and it already is, because catalogue `upsert` is
keyed by `remoteFileId` (`mobile/rust-bridge/src/sync.rs:19-29`).

`ChangeObserver` is a **foreign-implemented** trait with two methods, both
invoked from a background runtime thread (never the UI thread):
`onChanges(page)` (apply the mutations) and `onError(message)` (a poll failed;
the loop logs it and retries on the next interval, leaving the cursor untouched
so nothing is skipped) (`mobile/rust-bridge/src/sync.rs:154-167`).

The platform drains pending work using the catalogue queues:
`pendingUploads()` returns the `LocalDirty` / `LocalDeleted` rows (mint an
`uploadUrl` per entry), and `pendingDownloads()` returns the `RemoteDirty` /
`RemoteDeleted` rows (mint a `downloadUrl`, or unlink for deletes)
(`mobile/rust-bridge/src/sync.rs:399-411`).

---

## Building and packaging

The build scripts live in `mobile/rust-bridge/` and emit reproducible
artifacts. The release profile deliberately does **not** strip symbols: UniFFI
library-mode binding generation reads the FFI contract from the metadata
symbols baked into the compiled library, and stripping would make
`uniffi-bindgen generate --library …` silently emit nothing. The shipped
artifacts are stripped explicitly during packaging instead
(`mobile/rust-bridge/Cargo.toml:54-65`).

### Android — `build-android.sh`

```bash
rustup target add aarch64-linux-android armv7-linux-androideabi x86_64-linux-android
cargo install cargo-ndk

export ANDROID_NDK_HOME=/path/to/android-ndk-r26d   # r26+ required
cd mobile/rust-bridge
./build-android.sh                 # PROFILE=release, ANDROID_API=24 by default
```

Output under `build/android/` (`mobile/rust-bridge/build-android.sh:6-16`):

```text
jniLibs/arm64-v8a/libzk_mobile_bridge.so
jniLibs/armeabi-v7a/libzk_mobile_bridge.so
jniLibs/x86_64/libzk_mobile_bridge.so
kotlin/uniffi/zk_mobile_bridge/zk_mobile_bridge.kt
```

`cargo-ndk` cross-compiles each ABI and copies the `.so` into its `jniLibs`
subdirectory; `uniffi-bindgen` then generates the Kotlin binding from the
compiled arm64 library, and the shipped `.so` copies are stripped with the
NDK's `llvm-strip` after the binding is generated
(`mobile/rust-bridge/build-android.sh:77-111`). The Android app module puts
`jniLibs/` on its `sourceSets[...].jniLibs.srcDirs` and the generated `.kt` on
its source path.

### iOS — `build-ios.sh`

```bash
rustup target add aarch64-apple-ios aarch64-apple-ios-sim x86_64-apple-ios
cd mobile/rust-bridge
./build-ios.sh                     # PROFILE=release by default
```

Output under `build/ios/` (`mobile/rust-bridge/build-ios.sh:6-12`):

```text
ZkMobileBridge.xcframework/                     (device arm64 + fat simulator slice)
Sources/ZkMobileBridge/zk_mobile_bridge.swift   (generated Swift API)
```

Swift binding generation is target-independent (it reads crate metadata), so
the script generates the Swift API on any host. The iOS cross-compile and the
final `lipo` + `xcodebuild -create-xcframework` link step need **macOS with
Xcode**, so on a non-macOS host the script generates the bindings and then
stops with a clear message before the macOS-only stages rather than failing
cryptically (`mobile/rust-bridge/build-ios.sh:13-26`, `:120-133`).

### Bindings only

To inspect the contract without packaging:

```bash
cd mobile/rust-bridge
cargo build --release
cargo run --bin uniffi-bindgen -- generate \
  --library target/release/libzk_mobile_bridge.so \
  --language kotlin --out-dir /tmp/kt
cargo run --bin uniffi-bindgen -- generate \
  --library target/release/libzk_mobile_bridge.so \
  --language swift --out-dir /tmp/swift
```

The `uniffi-bindgen` binary is pinned in the crate so generation always runs
the exact UniFFI version the library was compiled against
(`mobile/rust-bridge/Cargo.toml:20-26`).

---

## Continuous integration

`.github/workflows/mobile-bridge.yml` builds the bridge for every target
architecture and generates both bindings whenever `mobile/**`, `sdk/**`, or the
workflow file changes (`.github/workflows/mobile-bridge.yml:10-22`):

- **`android` job (Ubuntu).** Installs the Android Rust targets, `cargo-ndk`,
  and NDK r26d; runs `cargo test --release` to exercise the bridge in-process
  (crypto round-trips, token manager, catalogue); runs `build-android.sh`; and
  generates the Swift bindings on Linux too, since library-mode generation is
  host-agnostic. It uploads the jniLibs and both bindings as artifacts
  (`.github/workflows/mobile-bridge.yml:38-89`).
- **`ios` job (macOS).** Installs the Apple Rust targets and runs
  `build-ios.sh` to assemble `ZkMobileBridge.xcframework`, uploaded as an
  artifact (`.github/workflows/mobile-bridge.yml:95-119`).

The workflow is intentionally **not a merge gate** — it produces the framework
artifacts the native apps consume rather than guarding the server build
(`.github/workflows/mobile-bridge.yml:6-9`).
