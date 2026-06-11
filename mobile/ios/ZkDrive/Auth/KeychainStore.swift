import Foundation
import Security

/// Thin, typed wrapper over the iOS Keychain (`kSecClassGenericPassword`).
/// Used to persist the OIDC token bundle and the offline-cache DEK.
///
/// Items are stored with `kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly`:
/// available to background sync after the first unlock, never migrated to
/// a new device via backup.
struct KeychainStore {
    enum KeychainError: Error, Equatable {
        case unexpectedStatus(OSStatus)
        case encodingFailed
    }

    let service: String

    init(service: String = "com.zkdrive.app") {
        self.service = service
    }

    // MARK: Data

    func writeData(_ data: Data, account: String) throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        let attributes: [String: Any] = [
            kSecValueData as String: data,
            kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly,
        ]
        let status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        switch status {
        case errSecSuccess:
            return
        case errSecItemNotFound:
            var insert = query
            insert.merge(attributes) { _, new in new }
            let addStatus = SecItemAdd(insert as CFDictionary, nil)
            guard addStatus == errSecSuccess else { throw KeychainError.unexpectedStatus(addStatus) }
        default:
            throw KeychainError.unexpectedStatus(status)
        }
    }

    func readData(account: String) throws -> Data? {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
            kSecReturnData as String: true,
            kSecMatchLimit as String: kSecMatchLimitOne,
        ]
        var item: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &item)
        switch status {
        case errSecSuccess:
            return item as? Data
        case errSecItemNotFound:
            return nil
        default:
            throw KeychainError.unexpectedStatus(status)
        }
    }

    func delete(account: String) throws {
        let query: [String: Any] = [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
        let status = SecItemDelete(query as CFDictionary)
        guard status == errSecSuccess || status == errSecItemNotFound else {
            throw KeychainError.unexpectedStatus(status)
        }
    }

    // MARK: Codable convenience

    func write<T: Encodable>(_ value: T, account: String) throws {
        let data = try JSONEncoder().encode(value)
        try writeData(data, account: account)
    }

    func read<T: Decodable>(_ type: T.Type, account: String) throws -> T? {
        guard let data = try readData(account: account) else { return nil }
        return try JSONDecoder().decode(type, from: data)
    }
}

/// A persistable copy of the bridge's `TokenBundle` (which is a generated
/// struct we can't conform to `Codable` directly). Round-trips through
/// the Keychain so a returning user is restored without a network call.
struct StoredTokenBundle: Codable, Equatable {
    let accessToken: String
    let refreshToken: String
    let expiresAtUnix: Int64
    let scope: String

    init(_ bundle: TokenBundle) {
        accessToken = bundle.accessToken
        refreshToken = bundle.refreshToken
        expiresAtUnix = bundle.expiresAtUnix
        scope = bundle.scope
    }

    var bridgeBundle: TokenBundle {
        TokenBundle(accessToken: accessToken, refreshToken: refreshToken, expiresAtUnix: expiresAtUnix, scope: scope)
    }
}
