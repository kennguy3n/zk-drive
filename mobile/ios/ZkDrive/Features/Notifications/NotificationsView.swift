import SwiftUI
import UserNotifications

struct NotificationsView: View {
    @StateObject var viewModel: NotificationsViewModel

    var body: some View {
        List {
            if viewModel.pushStatus == .notDetermined || viewModel.pushStatus == .denied {
                pushPrompt
            }
            if viewModel.notifications.isEmpty && !viewModel.isLoading {
                EmptyStateView(systemImage: "bell.slash", title: "No notifications", message: "Activity in your workspace will show up here.")
                    .listRowSeparator(.hidden)
            } else {
                ForEach(viewModel.notifications) { item in
                    NotificationRow(item: item)
                        .contentShape(Rectangle())
                        .onTapGesture { Task { await viewModel.markRead(item) } }
                }
            }
        }
        .listStyle(.plain)
        .navigationTitle("Alerts")
        .toolbar {
            if viewModel.unreadCount > 0 {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button("Mark all read") { Task { await viewModel.markAllRead() } }.font(Theme.Typography.caption)
                }
            }
        }
        .task { await viewModel.load() }
        .refreshable { await viewModel.load() }
        .alert("Error", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: { Text(viewModel.error?.userMessage ?? "") }
    }

    private var pushPrompt: some View {
        VStack(alignment: .leading, spacing: Theme.Spacing.sm) {
            Label("Enable push notifications", systemImage: "bell.badge")
                .font(Theme.Typography.headline)
            Text("Get notified about shares, comments, and sync activity.")
                .font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary)
            if viewModel.pushStatus == .denied {
                Text("Notifications are disabled in Settings. Enable them in the iOS Settings app.")
                    .font(Theme.Typography.caption).foregroundColor(Theme.Palette.warning)
            } else {
                Button("Enable") { Task { await viewModel.enablePush() } }
                    .buttonStyle(SecondaryButtonStyle())
            }
        }
        .padding(.vertical, Theme.Spacing.xs)
    }
}

struct NotificationRow: View {
    let item: AppNotification

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Spacing.md) {
            Circle()
                .fill(item.isRead ? Color.clear : Theme.Palette.brand)
                .frame(width: 8, height: 8)
                .padding(.top, 6)
            VStack(alignment: .leading, spacing: 2) {
                Text(item.title).font(Theme.Typography.body.weight(item.isRead ? .regular : .semibold)).lineLimit(2)
                Text(item.body).font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary).lineLimit(3)
                Text(Format.relative(item.createdAt)).font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary)
            }
        }
        .padding(.vertical, 2)
    }
}
