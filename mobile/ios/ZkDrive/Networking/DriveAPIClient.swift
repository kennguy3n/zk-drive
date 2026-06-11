import Foundation

/// REST client for the parts of the zk-drive API not exposed through the
/// Rust bridge: folder/file listing, search, sharing, permissions,
/// notifications, workspaces, and push-device registration. Transfer
/// URLs and the changefeed go through the bridge (`BridgeSession`); this
/// client covers the metadata plane.
///
/// Every request is authenticated with the bridge-managed OIDC access
/// token (`BridgeSession.accessToken()`), which transparently refreshes,
/// so the native layer never re-implements token logic.
actor DriveAPIClient {
    private let baseURL: URL
    private let session: URLSession
    private let tokenProvider: () async throws -> String

    init(baseURL: URL, session: URLSession = .shared, tokenProvider: @escaping () async throws -> String) {
        self.baseURL = baseURL
        self.session = session
        self.tokenProvider = tokenProvider
    }

    /// Convenience initialiser binding the token provider to a session.
    init(bridge: BridgeSession, session: URLSession = .shared) {
        self.init(baseURL: bridge.config.apiBaseURL, session: session) {
            try await bridge.accessToken()
        }
    }

    // MARK: Workspaces

    func listWorkspaces() async throws -> [Workspace] {
        try await get("api/workspaces", decode: WorkspaceListEnvelope.self).workspaces
    }

    // MARK: Folders & files

    func listRootFolders() async throws -> [Folder] {
        try await listFolders(parentID: nil)
    }

    func listFolders(parentID: String?) async throws -> [Folder] {
        let query = [URLQueryItem(name: "parent_folder_id", value: parentID ?? "root")]
        return try await get("api/folders", query: query, decode: FolderListEnvelope.self).folders
    }

    /// Folder detail: the folder, its subfolders and its files.
    func folderContents(folderID: String) async throws -> FolderContents {
        try await get("api/folders/\(folderID)", decode: FolderContents.self)
    }

    func createFolder(workspaceID: String, parentID: String?, name: String, mode: EncryptionMode) async throws -> Folder {
        struct Body: Encodable {
            let workspace_id: String
            let parent_folder_id: String?
            let name: String
            let encryption_mode: String
        }
        let body = Body(workspace_id: workspaceID, parent_folder_id: parentID, name: name, encryption_mode: mode.rawValue)
        return try await send("api/folders", method: "POST", body: body, decode: Folder.self)
    }

    func deleteFolder(folderID: String) async throws {
        try await sendNoContent("api/folders/\(folderID)", method: "DELETE")
    }

    func renameFile(fileID: String, name: String) async throws -> FileItem {
        struct Body: Encodable { let name: String }
        return try await send("api/files/\(fileID)", method: "PUT", body: Body(name: name), decode: FileItem.self)
    }

    func deleteFile(fileID: String) async throws {
        try await sendNoContent("api/files/\(fileID)", method: "DELETE")
    }

    func moveFile(fileID: String, toFolderID: String) async throws {
        struct Body: Encodable { let new_folder_id: String }
        try await sendNoContent("api/files/\(fileID)/move", method: "POST", body: Body(new_folder_id: toFolderID))
    }

    // MARK: Search

    func search(query: String, limit: Int = 25, offset: Int = 0, fuzzy: Bool = true) async throws -> [SearchHit] {
        let items = [
            URLQueryItem(name: "q", value: query),
            URLQueryItem(name: "limit", value: String(limit)),
            URLQueryItem(name: "offset", value: String(offset)),
            URLQueryItem(name: "fuzzy", value: fuzzy ? "true" : "false"),
        ]
        return try await get("api/search", query: items, decode: SearchEnvelope.self).hits
    }

    // MARK: Sharing

    func createShareLink(resourceType: String, resourceID: String, password: String?, expiresAt: Date?, maxDownloads: Int?) async throws -> ShareLink {
        struct Body: Encodable {
            let resource_type: String
            let resource_id: String
            let password: String?
            let expires_at: String?
            let max_downloads: Int?
        }
        let body = Body(
            resource_type: resourceType,
            resource_id: resourceID,
            password: password?.isEmpty == true ? nil : password,
            expires_at: expiresAt.map { JSONCoding.iso8601String($0) },
            max_downloads: maxDownloads
        )
        return try await send("api/share-links", method: "POST", body: body, decode: ShareLink.self)
    }

    func revokeShareLink(id: String) async throws {
        try await sendNoContent("api/share-links/\(id)", method: "DELETE")
    }

    func createGuestInvite(email: String, folderID: String, role: ShareRole, expiresAt: Date?) async throws -> GuestInvite {
        struct Body: Encodable {
            let email: String
            let folder_id: String
            let role: String
            let expires_at: String?
        }
        let body = Body(email: email, folder_id: folderID, role: role.rawValue, expires_at: expiresAt.map { JSONCoding.iso8601String($0) })
        return try await send("api/guest-invites", method: "POST", body: body, decode: GuestInvite.self)
    }

    func listPermissions(resourceType: String, resourceID: String) async throws -> [Permission] {
        let items = [
            URLQueryItem(name: "resource_type", value: resourceType),
            URLQueryItem(name: "resource_id", value: resourceID),
        ]
        return try await get("api/permissions", query: items, decode: PermissionEnvelope.self).permissions
    }

    func grantPermission(resourceType: String, resourceID: String, granteeType: String, granteeID: String, role: ShareRole) async throws -> Permission {
        struct Body: Encodable {
            let resource_type: String
            let resource_id: String
            let grantee_type: String
            let grantee_id: String
            let role: String
        }
        let body = Body(resource_type: resourceType, resource_id: resourceID, grantee_type: granteeType, grantee_id: granteeID, role: role.rawValue)
        return try await send("api/permissions", method: "POST", body: body, decode: Permission.self)
    }

    func revokePermission(id: String) async throws {
        try await sendNoContent("api/permissions/\(id)", method: "DELETE")
    }

    // MARK: Notifications

    func listNotifications(limit: Int = 50, offset: Int = 0) async throws -> [AppNotification] {
        let items = [
            URLQueryItem(name: "limit", value: String(limit)),
            URLQueryItem(name: "offset", value: String(offset)),
        ]
        return try await get("api/notifications", query: items, decode: NotificationEnvelope.self).notifications
    }

    func markNotificationRead(id: String) async throws {
        try await sendNoContent("api/notifications/\(id)/read", method: "POST")
    }

    func markAllNotificationsRead() async throws {
        try await sendNoContent("api/notifications/read-all", method: "POST")
    }

    // MARK: Push

    /// Register an APNs device token. The server responds 501 when mobile
    /// push is not configured; that surfaces as `.notFound`-adjacent and
    /// is handled gracefully by the caller (push simply stays off).
    func registerDevice(token: String) async throws {
        struct Body: Encodable { let platform: String; let token: String }
        try await sendNoContent("api/push/register-device", method: "POST", body: Body(platform: "ios", token: token))
    }

    func unregisterDevice(token: String) async throws {
        struct Body: Encodable { let platform: String; let token: String }
        try await sendNoContent("api/push/register-device", method: "DELETE", body: Body(platform: "ios", token: token))
    }

    // MARK: Request plumbing

    private func get<T: Decodable>(_ path: String, query: [URLQueryItem] = [], decode: T.Type) async throws -> T {
        let request = try await makeRequest(path: path, method: "GET", query: query, body: Optional<Int>.none)
        return try await perform(request, decode: T.self)
    }

    private func send<Body: Encodable, T: Decodable>(_ path: String, method: String, body: Body, decode: T.Type) async throws -> T {
        let request = try await makeRequest(path: path, method: method, query: [], body: body)
        return try await perform(request, decode: T.self)
    }

    private func sendNoContent(_ path: String, method: String) async throws {
        let request = try await makeRequest(path: path, method: method, query: [], body: Optional<Int>.none)
        try await performNoContent(request)
    }

    private func sendNoContent<Body: Encodable>(_ path: String, method: String, body: Body) async throws {
        let request = try await makeRequest(path: path, method: method, query: [], body: body)
        try await performNoContent(request)
    }

    private func makeRequest<Body: Encodable>(path: String, method: String, query: [URLQueryItem], body: Body?) async throws -> URLRequest {
        guard var components = URLComponents(url: baseURL.appendingPathComponent(path), resolvingAgainstBaseURL: false) else {
            throw AppError(category: .invalidInput, message: "Bad URL for \(path)", httpStatus: nil)
        }
        if !query.isEmpty { components.queryItems = query }
        guard let url = components.url else {
            throw AppError(category: .invalidInput, message: "Bad URL for \(path)", httpStatus: nil)
        }
        var request = URLRequest(url: url)
        request.httpMethod = method
        request.setValue("application/json", forHTTPHeaderField: "Accept")
        let token = try await tokenProvider()
        request.setValue("Bearer \(token)", forHTTPHeaderField: "Authorization")
        if let body {
            request.setValue("application/json", forHTTPHeaderField: "Content-Type")
            request.httpBody = try JSONCoding.encoder.encode(body)
        }
        return request
    }

    private func perform<T: Decodable>(_ request: URLRequest, decode: T.Type) async throws -> T {
        let (data, response) = try await dataOrThrow(request)
        do {
            return try JSONCoding.decoder.decode(T.self, from: data)
        } catch {
            _ = response
            throw AppError.unknown("Failed to decode \(T.self): \(error.localizedDescription)")
        }
    }

    private func performNoContent(_ request: URLRequest) async throws {
        _ = try await dataOrThrow(request)
    }

    private func dataOrThrow(_ request: URLRequest) async throws -> (Data, HTTPURLResponse) {
        let (data, response): (Data, URLResponse)
        do {
            (data, response) = try await session.data(for: request)
        } catch {
            throw error.asAppError()
        }
        guard let http = response as? HTTPURLResponse else {
            throw AppError.network("No HTTP response")
        }
        guard (200..<300).contains(http.statusCode) else {
            let message = Self.decodeErrorMessage(data) ?? "Request failed"
            throw AppError.fromHTTP(status: http.statusCode, message: message)
        }
        return (data, http)
    }

    private static func decodeErrorMessage(_ data: Data) -> String? {
        struct ServerError: Decodable { let code: String?; let message: String? }
        return (try? JSONDecoder().decode(ServerError.self, from: data))?.message
    }
}
