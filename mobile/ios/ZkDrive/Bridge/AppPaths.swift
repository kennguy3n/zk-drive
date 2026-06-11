import Foundation

/// Centralised on-device file locations. The sync catalogue and offline
/// blob cache live under Application Support (persists across launches,
/// not user-visible) and are flagged excluded-from-backup so encrypted
/// blobs are never copied into iCloud/iTunes backups.
enum AppPaths {
    /// `<AppSupport>/ZkDrive`, created on first access.
    static func baseDirectory() throws -> URL {
        let fm = FileManager.default
        let support = try fm.url(for: .applicationSupportDirectory, in: .userDomainMask, appropriateFor: nil, create: true)
        let dir = support.appendingPathComponent("ZkDrive", isDirectory: true)
        if !fm.fileExists(atPath: dir.path) {
            try fm.createDirectory(at: dir, withIntermediateDirectories: true)
            try excludeFromBackup(dir)
        }
        return dir
    }

    /// Per-workspace catalogue database path.
    static func catalogueURL(workspaceID: String) throws -> URL {
        let safe = try safeIdentifier(workspaceID)
        let dir = try baseDirectory().appendingPathComponent("catalogues", isDirectory: true)
        try ensureDirectory(dir)
        return dir.appendingPathComponent("\(safe).sqlite", isDirectory: false)
    }

    /// Defense-in-depth guard for IDs that become filesystem path components.
    /// Server-issued IDs are UUIDs, so anything containing a path separator,
    /// NUL, or a parent-directory traversal is malformed/hostile and must not
    /// be allowed to escape the intended directory.
    static func safeIdentifier(_ raw: String) throws -> String {
        let isUnsafe = raw.isEmpty
            || raw == "." || raw == ".."
            || raw.contains("/") || raw.contains("\\")
            || raw.contains("..") || raw.unicodeScalars.contains("\u{0}")
        guard !isUnsafe else {
            throw AppError(category: .invalidInput, message: "Invalid identifier", httpStatus: nil)
        }
        return raw
    }

    /// Directory holding encrypted offline blobs.
    static func offlineCacheDirectory() throws -> URL {
        let dir = try baseDirectory().appendingPathComponent("offline", isDirectory: true)
        try ensureDirectory(dir)
        return dir
    }

    private static func ensureDirectory(_ url: URL) throws {
        let fm = FileManager.default
        if !fm.fileExists(atPath: url.path) {
            try fm.createDirectory(at: url, withIntermediateDirectories: true)
            try excludeFromBackup(url)
        }
    }

    private static func excludeFromBackup(_ url: URL) throws {
        var url = url
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        try url.setResourceValues(values)
    }
}
