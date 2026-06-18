import Foundation
import SwiftUI

/// Loads an inline preview for a file. Strategy:
///   - If the server can render a preview (`preview-url`, e.g. images,
///     PDF, text) we load that presigned URL inline.
///   - Otherwise we fall back to a "download to view/share" affordance.
///   - If an encrypted offline copy exists, it is served without network.
@MainActor
final class FilePreviewViewModel: ObservableObject {
    enum PreviewContent: Equatable {
        case loading
        case image(URL)
        case pdf(URL)
        case text(String)
        case unsupported
        case offline(URL)
    }

    @Published private(set) var content: PreviewContent = .loading
    @Published var error: AppError?
    @Published var isPreparingShare = false
    @Published var shareURL: URL?

    let file: FileItem
    private let bridge: BridgeSession
    let transfers: TransferManager
    private let offline: OfflineStore

    init(file: FileItem, bridge: BridgeSession, transfers: TransferManager, offline: OfflineStore) {
        self.file = file
        self.bridge = bridge
        self.transfers = transfers
        self.offline = offline
    }

    var hasOfflineCopy: Bool { offline.hasOfflineCopy(fileID: file.id) }

    func load() async {
        // Prefer an offline copy when present — instant and works on a
        // plane.
        if offline.hasOfflineCopy(fileID: file.id) {
            if let url = await writeOfflineToTemp() {
                content = classifyOffline(url: url)
                return
            }
        }
        do {
            let target = try await bridge.previewTarget(fileID: file.id)
            guard let url = URL(string: target.previewUrl) else {
                content = .unsupported
                return
            }
            content = try await classifyRemote(url: url, mime: target.mimeType)
        } catch let appError as AppError where appError.httpStatus == 404 {
            // No server-side preview available for this type.
            content = .unsupported
        } catch {
            self.error = error.asAppError()
            content = .unsupported
        }
    }

    /// Download the file (caching it offline) and surface a local URL for
    /// the native share sheet.
    func prepareShare() async {
        isPreparingShare = true
        defer { isPreparingShare = false }
        // If we have an offline copy, share it directly.
        if let url = await writeOfflineToTemp() {
            shareURL = url
            return
        }
        do {
            let target = try await bridge.downloadTarget(fileID: file.id)
            guard let remote = URL(string: target.downloadUrl) else { return }
            let (data, response) = try await URLSession.shared.data(from: remote)
            guard let http = response as? HTTPURLResponse, (200..<300).contains(http.statusCode) else {
                throw AppError.network("Download failed")
            }
            let dest = FileManager.default.temporaryDirectory.appendingPathComponent(file.name)
            try? FileManager.default.removeItem(at: dest)
            try data.write(to: dest)
            try? await offline.store(fileID: file.id, plaintext: data)
            shareURL = dest
        } catch {
            self.error = error.asAppError()
        }
    }

    /// Pin this file for offline use by downloading + caching it.
    func saveOffline() async {
        await transfers.download(fileID: file.id, title: file.name, cacheOffline: true)
    }

    // MARK: Helpers

    private func writeOfflineToTemp() async -> URL? {
        guard let data = try? await offline.load(fileID: file.id) else { return nil }
        let dest = FileManager.default.temporaryDirectory.appendingPathComponent(file.name)
        try? FileManager.default.removeItem(at: dest)
        do {
            try data.write(to: dest)
            return dest
        } catch {
            return nil
        }
    }

    private func classifyOffline(url: URL) -> PreviewContent {
        if file.mimeType.hasPrefix("image/") { return .image(url) }
        if file.mimeType == "application/pdf" { return .pdf(url) }
        if file.mimeType.hasPrefix("text/"), let text = try? String(contentsOf: url, encoding: .utf8) { return .text(text) }
        return .offline(url)
    }

    private func classifyRemote(url: URL, mime: String) async throws -> PreviewContent {
        if mime.hasPrefix("image/") { return .image(url) }
        if mime == "application/pdf" { return .pdf(url) }
        if mime.hasPrefix("text/") {
            let (data, _) = try await URLSession.shared.data(from: url)
            return .text(String(data: data, encoding: .utf8) ?? "")
        }
        return .unsupported
    }
}
