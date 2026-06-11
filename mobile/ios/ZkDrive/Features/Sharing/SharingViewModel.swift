import Foundation

/// Backs the sharing screen: create/revoke share links, invite guests by
/// email (folders only), and view/manage permission grants.
@MainActor
final class SharingViewModel: ObservableObject {
    @Published var shareLink: ShareLink?
    @Published var permissions: [Permission] = []
    @Published var isWorking = false
    @Published var error: AppError?
    @Published var infoMessage: String?

    // Share-link form
    @Published var password = ""
    @Published var useExpiry = false
    @Published var expiryDate = Date().addingTimeInterval(7 * 24 * 3600)
    @Published var useMaxDownloads = false
    @Published var maxDownloads = 10

    // Guest invite form
    @Published var inviteEmail = ""
    @Published var inviteRole: ShareRole = .viewer
    @Published var inviteUseExpiry = false
    @Published var inviteExpiry = Date().addingTimeInterval(30 * 24 * 3600)

    let target: ShareTarget
    private let api: DriveAPIClient
    private let shareBaseURL: URL

    init(target: ShareTarget, api: DriveAPIClient, shareBaseURL: URL) {
        self.target = target
        self.api = api
        self.shareBaseURL = shareBaseURL
    }

    var isFolder: Bool { target.resourceType == "folder" }

    func load() async {
        await loadPermissions()
    }

    func loadPermissions() async {
        do {
            permissions = try await api.listPermissions(resourceType: target.resourceType, resourceID: target.resourceID)
        } catch {
            self.error = error.asAppError()
        }
    }

    func createShareLink() async {
        isWorking = true
        defer { isWorking = false }
        do {
            shareLink = try await api.createShareLink(
                resourceType: target.resourceType,
                resourceID: target.resourceID,
                password: password.isEmpty ? nil : password,
                expiresAt: useExpiry ? expiryDate : nil,
                maxDownloads: useMaxDownloads ? maxDownloads : nil
            )
            infoMessage = "Share link created"
        } catch {
            self.error = error.asAppError()
        }
    }

    func revokeShareLink() async {
        guard let link = shareLink else { return }
        isWorking = true
        defer { isWorking = false }
        do {
            try await api.revokeShareLink(id: link.id)
            shareLink = nil
            infoMessage = "Share link revoked"
        } catch {
            self.error = error.asAppError()
        }
    }

    func inviteGuest() async {
        let email = inviteEmail.trimmingCharacters(in: .whitespacesAndNewlines)
        guard email.contains("@") else {
            error = AppError(category: .invalidInput, message: "Enter a valid email address", httpStatus: nil)
            return
        }
        isWorking = true
        defer { isWorking = false }
        do {
            _ = try await api.createGuestInvite(email: email, folderID: target.resourceID, role: inviteRole, expiresAt: inviteUseExpiry ? inviteExpiry : nil)
            inviteEmail = ""
            infoMessage = "Invitation sent to \(email)"
            await loadPermissions()
        } catch {
            self.error = error.asAppError()
        }
    }

    func revoke(_ permission: Permission) async {
        do {
            try await api.revokePermission(id: permission.id)
            permissions.removeAll { $0.id == permission.id }
        } catch {
            self.error = error.asAppError()
        }
    }

    /// The full share URL the user can copy/share, derived from the link
    /// token against the API base.
    var shareURLString: String? {
        guard let link = shareLink else { return nil }
        return shareBaseURL.appendingPathComponent("s/\(link.token)").absoluteString
    }
}
