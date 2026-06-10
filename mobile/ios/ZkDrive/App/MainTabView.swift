import SwiftUI

/// The signed-in shell: a five-tab layout (Files, Search, Transfers,
/// Notifications, Settings). Each tab owns its own navigation stack.
struct MainTabView: View {
    let services: AppServices
    @EnvironmentObject private var transfers: TransferManager
    @State private var selection: Tab = .files

    enum Tab: Hashable { case files, search, transfers, notifications, settings }

    var body: some View {
        TabView(selection: $selection) {
            NavigationView {
                FileBrowserView(viewModel: FileBrowserViewModel(api: services.api, bridge: services.bridge, offline: services.offline, sync: services.sync, transfers: services.transfers))
            }
            .navigationViewStyle(.stack)
            .tabItem { Label("Files", systemImage: "folder.fill") }
            .tag(Tab.files)

            NavigationView {
                SearchView(viewModel: SearchViewModel(api: services.api))
            }
            .navigationViewStyle(.stack)
            .tabItem { Label("Search", systemImage: "magnifyingglass") }
            .tag(Tab.search)

            NavigationView {
                TransfersView()
            }
            .navigationViewStyle(.stack)
            .tabItem { Label("Transfers", systemImage: "arrow.up.arrow.down.circle.fill") }
            .badge(activeTransferCount)
            .tag(Tab.transfers)

            NavigationView {
                NotificationsView(viewModel: NotificationsViewModel(api: services.api, push: services.push))
            }
            .navigationViewStyle(.stack)
            .tabItem { Label("Alerts", systemImage: "bell.fill") }
            .tag(Tab.notifications)

            NavigationView {
                SettingsView(viewModel: SettingsViewModel(api: services.api, offline: services.offline, push: services.push))
            }
            .navigationViewStyle(.stack)
            .tabItem { Label("Settings", systemImage: "gearshape.fill") }
            .tag(Tab.settings)
        }
        .task { await services.bootstrapSignedInState() }
        .onReceive(NotificationCenter.default.publisher(for: .zkDriveDidTapNotification)) { _ in
            selection = .notifications
        }
    }

    private var activeTransferCount: Int {
        transfers.jobs.filter { if case .inProgress = $0.status { return true } else { return false } }.count
    }
}

extension AppServices {
    /// One-time post-sign-in wiring: resolve the active workspace (the
    /// token is workspace-scoped), activate sync for it, and run a first
    /// sync pass.
    func bootstrapSignedInState() async {
        guard sync.workspaceID == nil else { return }
        do {
            let workspaces = try await api.listWorkspaces()
            guard let active = workspaces.first else { return }
            sync.activate(workspaceID: active.id)
            await sync.syncNow()
            await push.refreshAuthorizationStatus()
        } catch {
            // Non-fatal: the browser still works without a sync pass.
        }
    }
}
