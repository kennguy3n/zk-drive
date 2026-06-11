import Foundation

// Single-key list envelopes returned by the Go API
// (e.g. `{"folders": [...]}`). Kept together so the wire shape is
// documented in one place.

struct WorkspaceListEnvelope: Decodable { let workspaces: [Workspace] }
struct FolderListEnvelope: Decodable { let folders: [Folder] }
struct SearchEnvelope: Decodable { let hits: [SearchHit] }
struct PermissionEnvelope: Decodable { let permissions: [Permission] }
struct NotificationEnvelope: Decodable { let notifications: [AppNotification] }

/// `GET /api/folders/{id}` — the folder plus its immediate children and
/// files. `children`/`files` default to empty so a folder with no
/// contents (server may omit the keys) still decodes.
struct FolderContents: Decodable {
    let folder: Folder
    let children: [Folder]
    let files: [FileItem]

    enum CodingKeys: String, CodingKey { case folder, children, files }

    init(from decoder: Decoder) throws {
        let container = try decoder.container(keyedBy: CodingKeys.self)
        folder = try container.decode(Folder.self, forKey: .folder)
        children = try container.decodeIfPresent([Folder].self, forKey: .children) ?? []
        files = try container.decodeIfPresent([FileItem].self, forKey: .files) ?? []
    }

    /// Merge children + files into a single sorted node list: folders
    /// first, then files, each alphabetised — the conventional file
    /// browser ordering.
    var nodes: [DriveNode] {
        let folderNodes = children
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
            .map(DriveNode.folder)
        let fileNodes = files
            .sorted { $0.name.localizedCaseInsensitiveCompare($1.name) == .orderedAscending }
            .map(DriveNode.file)
        return folderNodes + fileNodes
    }
}
