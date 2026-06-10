import Foundation
import Combine

/// A live upload or download as shown in the transfers UI.
struct TransferJob: Identifiable, Equatable {
    enum Kind: Equatable { case upload, download }
    enum Status: Equatable { case inProgress, completed, failed(String) }

    let id: String
    let kind: Kind
    let title: String
    var fraction: Double
    var status: Status
    /// For a completed download, where the bytes landed.
    var localURL: URL?
    /// For a completed upload, the server file id.
    var fileID: String?

    var isActive: Bool { if case .inProgress = status { return true } else { return false } }
}

/// Manages direct-to-storage transfers over a **background** URLSession so
/// uploads/downloads continue when the app is suspended and resume after
/// relaunch. Uploads follow the three-step presigned-PUT flow
/// (upload-url → PUT bytes → confirm-upload); the confirm step is driven
/// from the session delegate so it runs even if the PUT finished while
/// the app was backgrounded.
@MainActor
final class TransferManager: NSObject, ObservableObject {
    @Published private(set) var jobs: [TransferJob] = []

    private let bridge: BridgeSession
    private let offlineStore: OfflineStore
    private let sessionIdentifier: String

    /// Jobs the user explicitly cancelled. The background task still fires a
    /// `didCompleteWithError` (NSURLErrorCancelled) afterwards; this set lets
    /// `finishTask` ignore that late callback so the terminal "Cancelled"
    /// status isn't overwritten by the system error string.
    private var cancelledJobs: Set<String> = []

    /// Pending upload metadata keyed by task description (a UUID). Needed
    /// to call confirm-upload after the PUT completes, surviving relaunch
    /// via UserDefaults.
    private struct PendingUpload: Codable {
        let jobID: String
        let fileID: String
        let objectKey: String
        let sizeBytes: Int64
        let tempPath: String
        let title: String
    }
    private struct PendingDownload: Codable {
        let jobID: String
        let fileID: String
        let title: String
        /// Whether the completed download should also be encrypted into the
        /// offline cache. Persisted so the intent survives an app relaunch
        /// while the download is still in flight.
        let cacheOffline: Bool
    }

    private let defaults = UserDefaults.standard
    private static let pendingUploadsKey = "transfers.pendingUploads"
    private static let pendingDownloadsKey = "transfers.pendingDownloads"

    private lazy var session: URLSession = {
        let config = URLSessionConfiguration.background(withIdentifier: sessionIdentifier)
        config.isDiscretionary = false
        config.sessionSendsLaunchEvents = true
        config.allowsCellularAccess = true
        return URLSession(configuration: config, delegate: self, delegateQueue: nil)
    }()

    init(bridge: BridgeSession, offlineStore: OfflineStore, sessionIdentifier: String = "com.zkdrive.transfers") {
        self.bridge = bridge
        self.offlineStore = offlineStore
        self.sessionIdentifier = sessionIdentifier
        super.init()
        // Touch the session so it reconnects to any in-flight background
        // tasks created in a previous launch.
        _ = session
    }

    // MARK: Public API

    /// Upload a local file (already on disk) to the given folder. Returns
    /// once the transfer has been *enqueued*; progress/result are
    /// observed via `jobs`.
    func upload(fileURL: URL, folderID: String, mimeType: String?) async {
        let jobID = UUID().uuidString
        let filename = fileURL.lastPathComponent
        appendJob(TransferJob(id: jobID, kind: .upload, title: filename, fraction: 0, status: .inProgress, localURL: nil, fileID: nil))
        do {
            let size = (try? fileURL.resourceValues(forKeys: [.fileSizeKey]).fileSize).map(Int64.init) ?? 0
            let target = try await bridge.uploadTarget(folderID: folderID, filename: filename, mimeType: mimeType)
            guard let url = URL(string: target.uploadUrl) else {
                throw AppError(category: .invalidInput, message: "Bad upload URL", httpStatus: nil)
            }
            // Copy into a stable temp location the background session owns.
            let temp = FileManager.default.temporaryDirectory.appendingPathComponent("up-\(jobID)")
            try? FileManager.default.removeItem(at: temp)
            try FileManager.default.copyItem(at: fileURL, to: temp)

            var request = URLRequest(url: url)
            request.httpMethod = "PUT"
            if let mimeType { request.setValue(mimeType, forHTTPHeaderField: "Content-Type") }

            let pending = PendingUpload(jobID: jobID, fileID: target.fileId, objectKey: target.objectKey, sizeBytes: size, tempPath: temp.path, title: filename)
            storePending(pending)

            let task = session.uploadTask(with: request, fromFile: temp)
            task.taskDescription = jobID
            task.resume()
        } catch {
            updateJob(jobID) { $0.status = .failed(error.asAppError().userMessage) }
        }
    }

    /// Download a file by id to local storage. On success the plaintext
    /// is also cached in the encrypted offline store.
    func download(fileID: String, title: String, cacheOffline: Bool = true) async {
        let jobID = UUID().uuidString
        appendJob(TransferJob(id: jobID, kind: .download, title: title, fraction: 0, status: .inProgress, localURL: nil, fileID: fileID))
        do {
            let target = try await bridge.downloadTarget(fileID: fileID)
            guard let url = URL(string: target.downloadUrl) else {
                throw AppError(category: .invalidInput, message: "Bad download URL", httpStatus: nil)
            }
            storePending(PendingDownload(jobID: jobID, fileID: fileID, title: title, cacheOffline: cacheOffline))
            var request = URLRequest(url: url)
            request.httpMethod = "GET"
            let task = session.downloadTask(with: request)
            task.taskDescription = jobID
            task.resume()
        } catch {
            updateJob(jobID) { $0.status = .failed(error.asAppError().userMessage) }
        }
    }

    func clearFinished() {
        jobs.removeAll { if case .inProgress = $0.status { return false } else { return true } }
    }

    /// Cancel an in-flight transfer: cancel the underlying background task
    /// and drop its pending bookkeeping so it isn't resumed on relaunch.
    func cancel(_ job: TransferJob) {
        guard case .inProgress = job.status else { return }
        session.getAllTasks { tasks in
            for task in tasks where task.taskDescription == job.id { task.cancel() }
        }
        cancelledJobs.insert(job.id)
        removePendingUpload(jobID: job.id)
        removePendingDownload(jobID: job.id)
        updateJob(job.id) { $0.status = .failed("Cancelled") }
    }

    // MARK: Job bookkeeping

    private func appendJob(_ job: TransferJob) {
        jobs.removeAll { $0.id == job.id }
        jobs.insert(job, at: 0)
    }

    private func updateJob(_ id: String, _ mutate: (inout TransferJob) -> Void) {
        guard let index = jobs.firstIndex(where: { $0.id == id }) else { return }
        mutate(&jobs[index])
    }

    // MARK: Pending persistence

    private func storePending(_ pending: PendingUpload) {
        var all = loadPendingUploads()
        all[pending.jobID] = pending
        if let data = try? JSONEncoder().encode(all) { defaults.set(data, forKey: Self.pendingUploadsKey) }
    }

    private func loadPendingUploads() -> [String: PendingUpload] {
        guard let data = defaults.data(forKey: Self.pendingUploadsKey),
              let all = try? JSONDecoder().decode([String: PendingUpload].self, from: data) else { return [:] }
        return all
    }

    private func removePendingUpload(jobID: String) {
        var all = loadPendingUploads()
        all.removeValue(forKey: jobID)
        if let data = try? JSONEncoder().encode(all) { defaults.set(data, forKey: Self.pendingUploadsKey) }
    }

    private func storePending(_ pending: PendingDownload) {
        var all = loadPendingDownloads()
        all[pending.jobID] = pending
        if let data = try? JSONEncoder().encode(all) { defaults.set(data, forKey: Self.pendingDownloadsKey) }
    }

    private func loadPendingDownloads() -> [String: PendingDownload] {
        guard let data = defaults.data(forKey: Self.pendingDownloadsKey),
              let all = try? JSONDecoder().decode([String: PendingDownload].self, from: data) else { return [:] }
        return all
    }

    private func removePendingDownload(jobID: String) {
        var all = loadPendingDownloads()
        all.removeValue(forKey: jobID)
        if let data = try? JSONEncoder().encode(all) { defaults.set(data, forKey: Self.pendingDownloadsKey) }
    }
}

// MARK: - URLSession delegates
//
// Delegate callbacks arrive on the session's background queue, so each
// hops back to the main actor before mutating published state.
extension TransferManager: URLSessionDataDelegate, URLSessionDownloadDelegate {
    nonisolated func urlSession(_ session: URLSession, task: URLSessionTask, didSendBodyData bytesSent: Int64, totalBytesSent: Int64, totalBytesExpectedToSend: Int64) {
        guard totalBytesExpectedToSend > 0, let jobID = task.taskDescription else { return }
        let fraction = Double(totalBytesSent) / Double(totalBytesExpectedToSend)
        Task { @MainActor in self.updateJob(jobID) { $0.fraction = fraction } }
    }

    nonisolated func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didWriteData bytesWritten: Int64, totalBytesWritten: Int64, totalBytesExpectedToWrite: Int64) {
        guard totalBytesExpectedToWrite > 0, let jobID = downloadTask.taskDescription else { return }
        let fraction = Double(totalBytesWritten) / Double(totalBytesExpectedToWrite)
        Task { @MainActor in self.updateJob(jobID) { $0.fraction = fraction } }
    }

    nonisolated func urlSession(_ session: URLSession, downloadTask: URLSessionDownloadTask, didFinishDownloadingTo location: URL) {
        guard let jobID = downloadTask.taskDescription else { return }
        let statusCode = (downloadTask.response as? HTTPURLResponse)?.statusCode ?? 0
        // Move the file synchronously here — `location` is deleted when
        // this delegate returns.
        let fm = FileManager.default
        let dest = fm.temporaryDirectory.appendingPathComponent("dl-\(jobID)")
        try? fm.removeItem(at: dest)
        let moved = (try? fm.moveItem(at: location, to: dest)) != nil
        Task { @MainActor in
            await self.finishDownload(jobID: jobID, statusCode: statusCode, localURL: moved ? dest : nil)
        }
    }

    nonisolated func urlSession(_ session: URLSession, task: URLSessionTask, didCompleteWithError error: Error?) {
        guard let jobID = task.taskDescription else { return }
        let statusCode = (task.response as? HTTPURLResponse)?.statusCode ?? 0
        let errorMessage = error?.localizedDescription
        Task { @MainActor in
            await self.finishTask(jobID: jobID, isUpload: task is URLSessionUploadTask || task is URLSessionDataTask, statusCode: statusCode, errorMessage: errorMessage)
        }
    }

    nonisolated func urlSessionDidFinishEvents(forBackgroundURLSession session: URLSession) {
        // The completion handler lives on the router (it may have been stored
        // before this instance existed, during a background relaunch), so read
        // and clear it there.
        Task { @MainActor in
            let handler = AppDelegateRouter.shared.pendingBackgroundCompletionHandler
            AppDelegateRouter.shared.pendingBackgroundCompletionHandler = nil
            handler?()
        }
    }

    // MARK: Completion handling (main actor)

    private func finishTask(jobID: String, isUpload: Bool, statusCode: Int, errorMessage: String?) async {
        // A user-initiated cancel already set the terminal status and cleaned
        // up; swallow the late delegate callback so it can't overwrite it.
        if cancelledJobs.remove(jobID) != nil { return }
        guard isUpload else {
            // A *successful* download is finalised in `didFinishDownloadingTo`.
            // That delegate never fires for a transport-level failure (timeout,
            // connection lost), so this is the only place such a download can be
            // surfaced as failed and its pending bookkeeping cleaned up.
            if let errorMessage {
                updateJob(jobID) { $0.status = .failed(errorMessage) }
                removePendingDownload(jobID: jobID)
            }
            return
        }
        guard let pending = loadPendingUploads()[jobID] else {
            if let errorMessage { updateJob(jobID) { $0.status = .failed(errorMessage) } }
            return
        }
        defer { try? FileManager.default.removeItem(atPath: pending.tempPath) }
        if let errorMessage {
            updateJob(jobID) { $0.status = .failed(errorMessage) }
            removePendingUpload(jobID: jobID)
            return
        }
        guard (200..<300).contains(statusCode) else {
            updateJob(jobID) { $0.status = .failed("Upload failed (HTTP \(statusCode))") }
            removePendingUpload(jobID: jobID)
            return
        }
        do {
            _ = try await bridge.confirmUpload(fileID: pending.fileID, objectKey: pending.objectKey, sizeBytes: pending.sizeBytes, checksum: nil)
            updateJob(jobID) { $0.fraction = 1; $0.status = .completed; $0.fileID = pending.fileID }
        } catch {
            updateJob(jobID) { $0.status = .failed(error.asAppError().userMessage) }
        }
        removePendingUpload(jobID: jobID)
    }

    private func finishDownload(jobID: String, statusCode: Int, localURL: URL?) async {
        guard (200..<300).contains(statusCode), let localURL else {
            updateJob(jobID) { $0.status = .failed("Download failed (HTTP \(statusCode))") }
            removePendingDownload(jobID: jobID)
            return
        }
        let pending = loadPendingDownloads()[jobID]
        updateJob(jobID) { $0.fraction = 1; $0.status = .completed; $0.localURL = localURL }
        if let pending, pending.cacheOffline, let data = try? Data(contentsOf: localURL) {
            try? await offlineStore.store(fileID: pending.fileID, plaintext: data)
        }
        removePendingDownload(jobID: jobID)
    }
}
