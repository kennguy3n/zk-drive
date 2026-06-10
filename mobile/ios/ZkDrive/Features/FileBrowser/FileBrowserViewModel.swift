import Foundation
import SwiftUI
import UniformTypeIdentifiers

/// Drives one level of the file browser. A view model is created per
/// folder level (root or a specific folder) so navigation is a stack of
/// independent, cancellable loaders.
@MainActor
final class FileBrowserViewModel: ObservableObject {
    enum Location: Equatable {
        case root
        case folder(Folder)

        var title: String {
            switch self {
            case .root: return "My Drive"
            case .folder(let f): return f.name
            }
        }

        var folderID: String? {
            switch self {
            case .root: return nil
            case .folder(let f): return f.id
            }
        }

        /// Encryption mode for the upload/create context. Root inherits
        /// nothing, so default to managed.
        var encryptionMode: EncryptionMode {
            switch self {
            case .root: return .managedEncrypted
            case .folder(let f): return f.encryptionMode
            }
        }
    }

    @Published private(set) var nodes: [DriveNode] = []
    @Published private(set) var isLoading = false
    @Published var error: AppError?
    @Published var workspaceID: String?

    let location: Location
    let api: DriveAPIClient
    let bridge: BridgeSession
    let offline: OfflineStore
    let sync: SyncCoordinator
    let transfers: TransferManager

    init(location: Location = .root, api: DriveAPIClient, bridge: BridgeSession, offline: OfflineStore, sync: SyncCoordinator, transfers: TransferManager) {
        self.location = location
        self.api = api
        self.bridge = bridge
        self.offline = offline
        self.sync = sync
        self.transfers = transfers
    }

    /// Build a child VM for a subfolder, reusing the same dependencies.
    func child(for folder: Folder) -> FileBrowserViewModel {
        FileBrowserViewModel(location: .folder(folder), api: api, bridge: bridge, offline: offline, sync: sync, transfers: transfers)
    }

    /// Build the preview VM for a file, reusing the same dependencies.
    func preview(for file: FileItem) -> FilePreviewViewModel {
        FilePreviewViewModel(file: file, bridge: bridge, transfers: transfers, offline: offline)
    }

    /// Build the sharing VM for a node target.
    func sharing(for target: ShareTarget) -> SharingViewModel {
        SharingViewModel(target: target, api: api, shareBaseURL: bridge.config.apiBaseURL)
    }

    var canUpload: Bool { location.folderID != nil }

    func load() async {
        isLoading = true
        defer { isLoading = false }
        do {
            switch location {
            case .root:
                // Resolve the active workspace to scope folder creation.
                if workspaceID == nil {
                    workspaceID = try await api.listWorkspaces().first?.id
                }
                let folders = try await api.listRootFolders()
                nodes = folders
                    .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
                    .map(DriveNode.folder)
            case .folder(let folder):
                workspaceID = folder.workspaceID
                let contents = try await api.folderContents(folderID: folder.id)
                nodes = contents.nodes
            }
            error = nil
        } catch {
            self.error = error.asAppError()
        }
    }

    func refresh() async {
        await sync.syncNow()
        await load()
    }

    func createFolder(name: String, mode: EncryptionMode) async {
        guard let workspaceID else {
            error = AppError(category: .invalidInput, message: "No active workspace", httpStatus: nil)
            return
        }
        do {
            _ = try await api.createFolder(workspaceID: workspaceID, parentID: location.folderID, name: name, mode: mode)
            await load()
        } catch {
            self.error = error.asAppError()
        }
    }

    func upload(urls: [URL]) async {
        guard let folderID = location.folderID else { return }
        for url in urls {
            let mime = Self.mimeType(for: url)
            await transfers.upload(fileURL: url, folderID: folderID, mimeType: mime)
        }
        // Reflect the new uploads after a short sync.
        await refresh()
    }

    func delete(_ node: DriveNode) async {
        do {
            switch node {
            case .file(let f): try await api.deleteFile(fileID: f.id)
            case .folder(let f): try await api.deleteFolder(folderID: f.id)
            }
            nodes.removeAll { $0.id == node.id }
        } catch {
            self.error = error.asAppError()
        }
    }

    static func mimeType(for url: URL) -> String? {
        if let type = UTType(filenameExtension: url.pathExtension), let mime = type.preferredMIMEType {
            return mime
        }
        return nil
    }
}
