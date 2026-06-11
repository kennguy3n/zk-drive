import Foundation
import SwiftUI

// Domain models. Field names + Codable keys mirror the zk-drive Go API
// JSON exactly (snake_case via CodingKeys) so decoding is a direct,
// lossless map of the server contract.

/// Per-folder encryption posture. Mirrors the Go `folder` package's
/// `managed_encrypted` / `strict_zk` modes.
enum EncryptionMode: String, Codable, Equatable, Sendable {
    case managedEncrypted = "managed_encrypted"
    case strictZK = "strict_zk"

    /// Unknown / future server values decode to managed (the safe,
    /// server-processed default) rather than failing the whole payload.
    init(from decoder: Decoder) throws {
        let raw = try decoder.singleValueContainer().decode(String.self)
        self = EncryptionMode(rawValue: raw) ?? .managedEncrypted
    }

    var shortLabel: String {
        switch self {
        case .managedEncrypted: return "Confidential"
        case .strictZK: return "Zero-Knowledge"
        }
    }

    var accessibilityLabel: String {
        switch self {
        case .managedEncrypted: return "Server-managed encryption"
        case .strictZK: return "Zero-knowledge end-to-end encryption"
        }
    }

    var systemImage: String {
        switch self {
        case .managedEncrypted: return "lock.fill"
        case .strictZK: return "lock.shield.fill"
        }
    }

    var tint: Color {
        switch self {
        case .managedEncrypted: return Theme.Palette.brand
        case .strictZK: return Theme.Palette.brandSecondary
        }
    }
}

struct Workspace: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let name: String
    let storageQuotaBytes: Int64
    let storageUsedBytes: Int64
    let tier: String

    enum CodingKeys: String, CodingKey {
        case id, name, tier
        case storageQuotaBytes = "storage_quota_bytes"
        case storageUsedBytes = "storage_used_bytes"
    }

    var usedFraction: Double {
        guard storageQuotaBytes > 0 else { return 0 }
        return min(1, Double(storageUsedBytes) / Double(storageQuotaBytes))
    }
}

struct Folder: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let workspaceID: String
    let parentFolderID: String?
    let name: String
    let path: String
    let encryptionMode: EncryptionMode
    let createdAt: Date
    let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case id, name, path
        case workspaceID = "workspace_id"
        case parentFolderID = "parent_folder_id"
        case encryptionMode = "encryption_mode"
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }
}

struct FileItem: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let workspaceID: String
    let folderID: String
    let name: String
    let currentVersionID: String?
    let sizeBytes: Int64
    let mimeType: String
    let createdAt: Date
    let updatedAt: Date

    enum CodingKeys: String, CodingKey {
        case id, name
        case workspaceID = "workspace_id"
        case folderID = "folder_id"
        case currentVersionID = "current_version_id"
        case sizeBytes = "size_bytes"
        case mimeType = "mime_type"
        case createdAt = "created_at"
        case updatedAt = "updated_at"
    }
}

/// One row of folder contents — a discriminated union of folder/file so a
/// browser list can render a mixed, sorted collection.
enum DriveNode: Identifiable, Equatable, Sendable {
    case folder(Folder)
    case file(FileItem)

    var id: String {
        switch self {
        case .folder(let f): return "folder:\(f.id)"
        case .file(let f): return "file:\(f.id)"
        }
    }

    var name: String {
        switch self {
        case .folder(let f): return f.name
        case .file(let f): return f.name
        }
    }

    var isFolder: Bool { if case .folder = self { return true } else { return false } }
}

struct SearchHit: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let type: String
    let name: String
    let path: String
    let folderID: String?
    let createdAt: Date
    let rank: Float
    let tags: [String]?

    enum CodingKeys: String, CodingKey {
        case id, type, name, path, rank, tags
        case folderID = "folder_id"
        case createdAt = "created_at"
    }
}

struct ShareLink: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let resourceType: String
    let resourceID: String
    let token: String
    let expiresAt: Date?
    let maxDownloads: Int?
    let downloadCount: Int
    let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, token
        case resourceType = "resource_type"
        case resourceID = "resource_id"
        case expiresAt = "expires_at"
        case maxDownloads = "max_downloads"
        case downloadCount = "download_count"
        case createdAt = "created_at"
    }
}

struct GuestInvite: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let email: String
    let folderID: String
    let role: String
    let expiresAt: Date?
    let acceptedAt: Date?
    let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, email, role
        case folderID = "folder_id"
        case expiresAt = "expires_at"
        case acceptedAt = "accepted_at"
        case createdAt = "created_at"
    }
}

struct Permission: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let resourceType: String
    let resourceID: String
    let granteeType: String
    let granteeID: String
    let role: String
    let expiresAt: Date?

    enum CodingKeys: String, CodingKey {
        case id, role
        case resourceType = "resource_type"
        case resourceID = "resource_id"
        case granteeType = "grantee_type"
        case granteeID = "grantee_id"
        case expiresAt = "expires_at"
    }
}

/// Permission roles offered in the sharing UI, in increasing privilege.
enum ShareRole: String, CaseIterable, Identifiable, Sendable {
    case viewer
    case editor
    case admin

    var id: String { rawValue }
    var label: String {
        switch self {
        case .viewer: return "View"
        case .editor: return "Edit"
        case .admin: return "Admin"
        }
    }
}

struct AppNotification: Codable, Identifiable, Equatable, Sendable {
    let id: String
    let type: String
    let title: String
    let body: String
    let resourceType: String?
    let resourceID: String?
    let readAt: Date?
    let createdAt: Date

    enum CodingKeys: String, CodingKey {
        case id, type, title, body
        case resourceType = "resource_type"
        case resourceID = "resource_id"
        case readAt = "read_at"
        case createdAt = "created_at"
    }

    var isRead: Bool { readAt != nil }
}
