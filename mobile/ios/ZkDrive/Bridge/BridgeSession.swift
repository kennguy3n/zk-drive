import Foundation

/// Runs a synchronous, potentially-blocking Rust bridge call off the main
/// thread and bridges it into Swift `async`, normalising thrown
/// `BridgeError`s into `AppError`. The bridge's network/disk methods
/// block the calling thread by contract, so they must never run on the
/// UI thread.
func bridgeCall<T>(
    on queue: DispatchQueue = BridgeSession.workQueue,
    _ body: @escaping () throws -> T
) async throws -> T {
    try await withCheckedThrowingContinuation { continuation in
        queue.async {
            do {
                continuation.resume(returning: try body())
            } catch {
                continuation.resume(throwing: error.asAppError())
            }
        }
    }
}

/// Owns the long-lived Rust bridge objects (token manager + API client)
/// and lazily constructs a per-workspace `SyncEngine`. One instance lives
/// for the lifetime of the app and is injected through `AppEnvironment`.
///
/// Threading: every method that touches the network or disk hops onto
/// `workQueue` via `bridgeCall`. Pure in-memory accessors
/// (`hasTokens`) stay synchronous.
final class BridgeSession: @unchecked Sendable {
    /// Serial queue for blocking bridge work. Serial (not concurrent) so
    /// token refreshes and catalogue writes can't race each other; the
    /// foreground poll loop runs on the Rust runtime's own threads, not
    /// this queue.
    static let workQueue = DispatchQueue(label: "com.zkdrive.bridge", qos: .userInitiated)

    let config: AppConfig
    let tokenManager: TokenManager
    let apiClient: ApiClient

    private let lock = NSLock()
    private var syncEngine: SyncEngine?
    private var syncWorkspaceID: String?

    init(config: AppConfig) throws {
        self.config = config
        self.tokenManager = try TokenManager(
            clientId: config.oidcClientID,
            tokenUrl: config.tokenEndpoint.absoluteString
        )
        self.apiClient = try ApiClient(
            baseUrl: config.apiBaseURL.absoluteString,
            tokens: tokenManager
        )
    }

    // MARK: Token lifecycle

    /// True if a token bundle is currently held (does not validate it).
    var hasTokens: Bool { tokenManager.hasTokens() }

    func setTokens(_ bundle: TokenBundle) async throws {
        try await bridgeCall { try self.tokenManager.setTokens(tokens: bundle) }
    }

    /// Returns a valid access token, transparently refreshing if needed.
    func accessToken() async throws -> String {
        try await bridgeCall { try self.tokenManager.accessToken() }
    }

    func tokenSnapshot() async throws -> TokenBundle? {
        try await bridgeCall { try self.tokenManager.snapshot() }
    }

    func clearTokens() async throws {
        try await bridgeCall { try self.tokenManager.clear() }
        lock.lock(); syncEngine = nil; syncWorkspaceID = nil; lock.unlock()
    }

    // MARK: API surface backed by the bridge

    func uploadTarget(folderID: String, filename: String, mimeType: String?) async throws -> UploadTarget {
        try await bridgeCall { try self.apiClient.uploadUrl(folderId: folderID, filename: filename, mimeType: mimeType) }
    }

    func confirmUpload(fileID: String, objectKey: String, sizeBytes: Int64, checksum: String?) async throws -> String {
        try await bridgeCall {
            try self.apiClient.confirmUpload(fileId: fileID, objectKey: objectKey, sizeBytes: sizeBytes, checksum: checksum)
        }
    }

    func downloadTarget(fileID: String) async throws -> DownloadTarget {
        try await bridgeCall { try self.apiClient.downloadUrl(fileId: fileID) }
    }

    func previewTarget(fileID: String) async throws -> PreviewTarget {
        try await bridgeCall { try self.apiClient.previewUrl(fileId: fileID) }
    }

    // MARK: Per-workspace sync engine

    /// Open (or reuse) the SQLite-backed sync engine for `workspaceID`.
    /// The catalogue lives in Application Support so it survives launches
    /// and is excluded from iCloud backup by `AppPaths`.
    func syncEngine(forWorkspace workspaceID: String) throws -> SyncEngine {
        lock.lock()
        defer { lock.unlock() }
        if let engine = syncEngine, syncWorkspaceID == workspaceID {
            return engine
        }
        let path = try AppPaths.catalogueURL(workspaceID: workspaceID).path
        let engine = try SyncEngine(cataloguePath: path, workspaceId: workspaceID, api: apiClient)
        syncEngine = engine
        syncWorkspaceID = workspaceID
        return engine
    }

    /// The current sync engine if one has been opened, else nil.
    func currentSyncEngine() -> SyncEngine? {
        lock.lock(); defer { lock.unlock() }
        return syncEngine
    }
}
