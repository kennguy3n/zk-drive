import Foundation
import Combine

/// Bridges the Rust `SyncEngine` to the SwiftUI world. It owns the
/// per-workspace engine, exposes the last sync result/time for the UI,
/// and offers a single `syncNow()` entry point used by both the
/// foreground "pull to refresh sync" and the background refresh task.
///
/// The engine persists its changefeed cursor in the local SQLite
/// catalogue, so `pollOnce` is always incremental and durable across
/// launches — exactly what a background task needs.
@MainActor
final class SyncCoordinator: ObservableObject {
    enum SyncState: Equatable {
        case idle
        case syncing
        case succeeded(applied: Int, at: Date)
        case failed(String)
    }

    @Published private(set) var state: SyncState = .idle
    @Published private(set) var lastCursor: Int64 = 0

    private let bridge: BridgeSession
    private(set) var workspaceID: String?

    /// Guards against overlapping syncs (e.g. a manual pull-to-refresh
    /// racing a background task).
    private var isSyncing = false

    init(bridge: BridgeSession) {
        self.bridge = bridge
    }

    /// Bind the coordinator to the active workspace, opening its
    /// catalogue. Safe to call repeatedly; a no-op if unchanged.
    func activate(workspaceID: String) {
        self.workspaceID = workspaceID
    }

    func deactivate() {
        workspaceID = nil
        state = .idle
        lastCursor = 0
    }

    /// Run one incremental sync pass. Returns the number of change
    /// records applied. Drains the changefeed until `hasMore` is false so
    /// a long-offline device fully catches up in one invocation.
    @discardableResult
    func syncNow(maxPages: Int = 25) async -> Int {
        guard let workspaceID, !isSyncing else { return 0 }
        isSyncing = true
        state = .syncing
        defer { isSyncing = false }

        do {
            let applied = try await drain(workspaceID: workspaceID, maxPages: maxPages)
            let cursor = try await bridgeCall { try self.bridge.syncEngine(forWorkspace: workspaceID).cursor() }
            lastCursor = cursor
            state = .succeeded(applied: applied, at: Date())
            return applied
        } catch {
            state = .failed(error.asAppError().userMessage)
            return 0
        }
    }

    private func drain(workspaceID: String, maxPages: Int) async throws -> Int {
        var total = 0
        for _ in 0..<maxPages {
            let page = try await bridgeCall {
                try self.bridge.syncEngine(forWorkspace: workspaceID).pollOnce(limit: 200)
            }
            total += page.mutations.count
            if !page.hasMore { break }
        }
        return total
    }

    // MARK: Catalogue reads for the UI

    /// All catalogue entries (synced/offline files) for display.
    func allEntries() async -> [FileEntry] {
        guard let workspaceID else { return [] }
        return (try? await bridgeCall { try self.bridge.syncEngine(forWorkspace: workspaceID).listAll() }) ?? []
    }

    func pendingUploads() async -> [FileEntry] {
        guard let workspaceID else { return [] }
        return (try? await bridgeCall { try self.bridge.syncEngine(forWorkspace: workspaceID).pendingUploads() }) ?? []
    }

    func pendingDownloads() async -> [FileEntry] {
        guard let workspaceID else { return [] }
        return (try? await bridgeCall { try self.bridge.syncEngine(forWorkspace: workspaceID).pendingDownloads() }) ?? []
    }

    /// Mark a file pinned-for-offline in the catalogue, recording its
    /// content hash and size so the engine tracks it as a managed entry.
    func registerOffline(fileID: String, versionID: String, localPath: String, sizeBytes: UInt64, contentHashHex: String) async {
        guard let workspaceID else { return }
        let entry = FileEntry(
            remoteFileId: fileID,
            remoteVersionId: versionID,
            localPath: localPath,
            sizeBytes: sizeBytes,
            contentHashHex: contentHashHex,
            status: .upToDate,
            pinned: true,
            updatedAtUnixMs: Int64(Date().timeIntervalSince1970 * 1000)
        )
        _ = try? await bridgeCall { try self.bridge.syncEngine(forWorkspace: workspaceID).upsert(entry: entry) }
    }
}
