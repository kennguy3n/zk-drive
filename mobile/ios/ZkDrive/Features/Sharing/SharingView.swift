import SwiftUI
import UIKit

struct SharingView: View {
    @StateObject var viewModel: SharingViewModel
    @State private var showingShareSheet = false

    var body: some View {
        Form {
            shareLinkSection
            if viewModel.isFolder { guestInviteSection }
            permissionsSection
        }
        .navigationTitle("Share")
        .navigationBarTitleDisplayMode(.inline)
        .task { await viewModel.load() }
        .alert("Sharing error", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: { Text(viewModel.error?.userMessage ?? "") }
        .sheet(isPresented: $showingShareSheet) {
            if let urlString = viewModel.shareURLString { ActivityView(items: [urlString]) }
        }
    }

    // MARK: Share link

    private var shareLinkSection: some View {
        Section("Share link") {
            if let link = viewModel.shareLink {
                if let urlString = viewModel.shareURLString {
                    HStack {
                        Text(urlString).font(Theme.Typography.footnote).lineLimit(1).truncationMode(.middle)
                        Spacer()
                        Button { UIPasteboard.general.string = urlString } label: { Image(systemName: "doc.on.doc") }
                            .buttonStyle(.borderless)
                    }
                }
                if let expiry = link.expiresAt {
                    KeyValueRow("Expires", value: Format.shortDate(expiry))
                }
                if let max = link.maxDownloads {
                    KeyValueRow("Downloads", value: "\(link.downloadCount) / \(max)")
                }
                Button { showingShareSheet = true } label: { Label("Share link…", systemImage: "square.and.arrow.up") }
                Button(role: .destructive) { Task { await viewModel.revokeShareLink() } } label: { Label("Revoke link", systemImage: "trash") }
            } else {
                SecureField("Password (optional)", text: $viewModel.password)
                Toggle("Set expiry", isOn: $viewModel.useExpiry)
                if viewModel.useExpiry {
                    DatePicker("Expires", selection: $viewModel.expiryDate, in: Date()..., displayedComponents: [.date, .hourAndMinute])
                }
                Toggle("Limit downloads", isOn: $viewModel.useMaxDownloads)
                if viewModel.useMaxDownloads {
                    Stepper("Max downloads: \(viewModel.maxDownloads)", value: $viewModel.maxDownloads, in: 1...1000)
                }
                Button { Task { await viewModel.createShareLink() } } label: {
                    if viewModel.isWorking { ProgressView() } else { Text("Create share link") }
                }
                .disabled(viewModel.isWorking)
            }
        }
    }

    // MARK: Guest invite

    private var guestInviteSection: some View {
        Section("Invite a guest") {
            TextField("Email address", text: $viewModel.inviteEmail)
                .keyboardType(.emailAddress)
                .textInputAutocapitalization(.never)
                .autocorrectionDisabled()
            Picker("Permission", selection: $viewModel.inviteRole) {
                ForEach(ShareRole.allCases) { role in Text(role.label).tag(role) }
            }
            Toggle("Set expiry", isOn: $viewModel.inviteUseExpiry)
            if viewModel.inviteUseExpiry {
                DatePicker("Expires", selection: $viewModel.inviteExpiry, in: Date()..., displayedComponents: [.date])
            }
            Button { Task { await viewModel.inviteGuest() } } label: {
                if viewModel.isWorking { ProgressView() } else { Text("Send invitation") }
            }
            .disabled(viewModel.isWorking)
        }
    }

    // MARK: Permissions

    private var permissionsSection: some View {
        Section("People with access") {
            if viewModel.permissions.isEmpty {
                Text("No direct grants yet").font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary)
            } else {
                ForEach(viewModel.permissions) { permission in
                    HStack {
                        VStack(alignment: .leading, spacing: 2) {
                            Text(permission.granteeID).font(Theme.Typography.callout).lineLimit(1)
                            Text(permission.granteeType.capitalized).font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary)
                        }
                        Spacer()
                        Text(permission.role.capitalized)
                            .font(Theme.Typography.caption.weight(.semibold))
                            .padding(.horizontal, Theme.Spacing.sm).padding(.vertical, 2)
                            .background(Theme.Palette.brand.opacity(0.12)).foregroundColor(Theme.Palette.brand)
                            .clipShape(Capsule())
                    }
                    .swipeActions {
                        Button(role: .destructive) { Task { await viewModel.revoke(permission) } } label: { Label("Revoke", systemImage: "trash") }
                    }
                }
            }
        }
    }
}
