import Foundation

/// Encrypts downloaded file bytes at rest for offline access using the
/// bridge's `CryptoEngine` (the exact XChaCha20-Poly1305 envelope every
/// ZK Drive client uses). The device cache key is a 32-byte DEK minted
/// by the bridge (`generateDek()`) and stored in the Keychain, so blobs
/// on disk are unreadable without the device's secure enclave-protected
/// keychain item.
///
/// This is a real, self-contained use of the bridge crypto that does not
/// depend on server-side key wrapping: offline copies are confidential
/// even if the device filesystem is extracted.
final class OfflineStore: @unchecked Sendable {
    private let keychain: KeychainStore
    private static let dekAccount = "offline-cache-dek"

    /// The crypto engine is derived once from the device DEK and reused for
    /// the lifetime of the store. It is internally immutable — every seal
    /// generates a fresh random nonce — so reuse is safe and avoids a
    /// Keychain `SecItemCopyMatching` IPC on every offline read/write
    /// (which matters when caching many files at once). Access is guarded
    /// by a lock for the lazy initialisation.
    private let engineLock = NSLock()
    private var cachedEngine: CryptoEngine?

    init(keychain: KeychainStore) {
        self.keychain = keychain
    }

    /// Fetch (or mint + persist) the device offline-cache DEK.
    private func cacheKey() throws -> Data {
        if let existing = try keychain.readData(account: Self.dekAccount), existing.count == 32 {
            return existing
        }
        let dek = generateDek()
        try keychain.writeData(dek, account: Self.dekAccount)
        return dek
    }

    private func engine() throws -> CryptoEngine {
        engineLock.lock()
        defer { engineLock.unlock() }
        if let cachedEngine { return cachedEngine }
        let engine = try CryptoEngine(dek: try cacheKey())
        cachedEngine = engine
        return engine
    }

    private func blobURL(fileID: String) throws -> URL {
        try AppPaths.offlineCacheDirectory().appendingPathComponent("\(fileID).enc", isDirectory: false)
    }

    /// True if an encrypted offline copy exists for `fileID`.
    func hasOfflineCopy(fileID: String) -> Bool {
        guard let url = try? blobURL(fileID: fileID) else { return false }
        return FileManager.default.fileExists(atPath: url.path)
    }

    /// Encrypt `plaintext` and persist it as the offline copy for `fileID`.
    func store(fileID: String, plaintext: Data) async throws {
        let url = try blobURL(fileID: fileID)
        let ciphertext = try await bridgeCall {
            try self.engine().encrypt(plaintext: plaintext)
        }
        try ciphertext.write(to: url, options: .completeFileProtection)
    }

    /// Decrypt and return the offline copy for `fileID`, or nil if absent.
    func load(fileID: String) async throws -> Data? {
        let url = try blobURL(fileID: fileID)
        guard FileManager.default.fileExists(atPath: url.path) else { return nil }
        let ciphertext = try Data(contentsOf: url)
        return try await bridgeCall {
            try self.engine().decrypt(ciphertext: ciphertext)
        }
    }

    /// Remove a single offline copy.
    func evict(fileID: String) throws {
        let url = try blobURL(fileID: fileID)
        try? FileManager.default.removeItem(at: url)
    }

    /// Drop every offline blob (e.g. on sign-out or "clear offline data").
    func evictAll() throws {
        let dir = try AppPaths.offlineCacheDirectory()
        let fm = FileManager.default
        for entry in (try? fm.contentsOfDirectory(at: dir, includingPropertiesForKeys: nil)) ?? [] {
            try? fm.removeItem(at: entry)
        }
    }

    /// Total bytes consumed by offline blobs on disk.
    func totalBytes() -> Int64 {
        guard let dir = try? AppPaths.offlineCacheDirectory() else { return 0 }
        let fm = FileManager.default
        let entries = (try? fm.contentsOfDirectory(at: dir, includingPropertiesForKeys: [.fileSizeKey])) ?? []
        return entries.reduce(0) { sum, url in
            let size = (try? url.resourceValues(forKeys: [.fileSizeKey]).fileSize) ?? 0
            return sum + Int64(size)
        }
    }
}
