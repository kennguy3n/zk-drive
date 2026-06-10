import SwiftUI

struct SettingsView: View {
    @StateObject var viewModel: SettingsViewModel
    @EnvironmentObject private var auth: AuthService

    @AppStorage(SettingsStore.appearanceKey) private var appearanceRaw = AppearancePreference.system.rawValue
    @AppStorage(SettingsStore.wifiOnlySyncKey) private var wifiOnlySync = true
    @AppStorage(SettingsStore.autoOfflineKey) private var autoOffline = false
    @AppStorage(SettingsStore.biometricLockKey) private var biometricLock = false

    @State private var showingSignOutConfirm = false

    var body: some View {
        Form {
            accountSection
            storageSection
            syncSection
            securitySection
            appearanceSection
            aboutSection
        }
        .navigationTitle("Settings")
        .task { await viewModel.load() }
        .confirmationDialog("Sign out of ZK Drive?", isPresented: $showingSignOutConfirm, titleVisibility: .visible) {
            Button("Sign Out", role: .destructive) { Task { await auth.signOut() } }
            Button("Cancel", role: .cancel) {}
        }
        .alert("Error", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: { Text(viewModel.error?.userMessage ?? "") }
    }

    // MARK: Account

    private var accountSection: some View {
        Section {
            HStack(spacing: Theme.Spacing.md) {
                ZStack {
                    Circle().fill(Theme.Palette.brandGradient).frame(width: 52, height: 52)
                    Text(auth.identity?.initials ?? "?").font(.headline).foregroundColor(.white)
                }
                VStack(alignment: .leading, spacing: 2) {
                    Text(auth.identity?.displayName ?? "Signed in").font(Theme.Typography.headline)
                    if let email = auth.identity?.email {
                        Text(email).font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary)
                    }
                    if let org = auth.identity?.orgID {
                        Text("Org: \(org)").font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary)
                    }
                }
            }
            .padding(.vertical, 4)
        }
    }

    // MARK: Storage

    private var storageSection: some View {
        Section("Storage") {
            VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
                HStack {
                    Text(viewModel.workspace?.name ?? "Workspace").font(Theme.Typography.callout)
                    Spacer()
                    Text(viewModel.storageText).font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary)
                }
                ProgressView(value: viewModel.workspace?.usedFraction ?? 0)
                    .tint(Theme.Palette.brand)
            }
            .padding(.vertical, 4)
            HStack {
                Label("Offline cache", systemImage: "internaldrive")
                Spacer()
                Text(Format.bytes(viewModel.offlineBytes)).foregroundColor(Theme.Palette.textSecondary)
            }
            Button("Clear offline cache", role: .destructive) { Task { await viewModel.clearOfflineCache() } }
                .disabled(viewModel.offlineBytes == 0)
        }
    }

    // MARK: Sync

    private var syncSection: some View {
        Section {
            Toggle(isOn: $wifiOnlySync) { Label("Sync on Wi-Fi only", systemImage: "wifi") }
            Toggle(isOn: $autoOffline) { Label("Auto-save pinned files offline", systemImage: "arrow.down.circle") }
        } header: {
            Text("Sync")
        } footer: {
            Text("Background sync runs every 15–30 minutes when the system allows it.")
        }
    }

    // MARK: Security

    private var securitySection: some View {
        Section("Security") {
            Toggle(isOn: $biometricLock) {
                Label(biometricLabel, systemImage: biometricIcon)
            }
            .disabled(!viewModel.biometricAvailable)
            if !viewModel.biometricAvailable {
                Text("Biometric unlock is unavailable on this device.")
                    .font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary)
            }
        }
    }

    private var biometricLabel: String {
        switch BiometricAuth.availability() {
        case .faceID: return "Require Face ID to unlock"
        case .touchID: return "Require Touch ID to unlock"
        case .opticID: return "Require Optic ID to unlock"
        case .none: return "Require biometric unlock"
        }
    }

    private var biometricIcon: String {
        switch BiometricAuth.availability() {
        case .faceID, .opticID: return "faceid"
        case .touchID: return "touchid"
        case .none: return "lock"
        }
    }

    // MARK: Appearance

    private var appearanceSection: some View {
        Section("Appearance") {
            Picker(selection: $appearanceRaw) {
                ForEach(AppearancePreference.allCases) { pref in
                    Text(pref.label).tag(pref.rawValue)
                }
            } label: {
                Label("Theme", systemImage: "paintbrush")
            }
            .pickerStyle(.menu)
        }
    }

    // MARK: About

    private var aboutSection: some View {
        Section {
            KeyValueRow("Version", value: Bundle.appVersionString)
            Button("Sign Out", role: .destructive) { showingSignOutConfirm = true }
        }
    }
}

extension Bundle {
    static var appVersionString: String {
        let version = main.infoDictionary?["CFBundleShortVersionString"] as? String ?? "1.0"
        let build = main.infoDictionary?["CFBundleVersion"] as? String ?? "1"
        return "\(version) (\(build))"
    }
}
