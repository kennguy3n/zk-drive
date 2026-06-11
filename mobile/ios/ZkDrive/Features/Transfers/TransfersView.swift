import SwiftUI

/// Live view of in-flight and recently-finished uploads/downloads. Backed
/// by `TransferManager`, whose background URLSession keeps transfers
/// running across app suspension and relaunch.
struct TransfersView: View {
    @EnvironmentObject private var transfers: TransferManager

    var body: some View {
        Group {
            if transfers.jobs.isEmpty {
                EmptyStateView(
                    systemImage: "arrow.up.arrow.down.circle",
                    title: "No transfers",
                    message: "Uploads and downloads will appear here while they run."
                )
            } else {
                List {
                    ForEach(transfers.jobs) { job in
                        TransferRow(job: job)
                            .swipeActions(edge: .trailing) {
                                if case .inProgress = job.status {
                                    Button(role: .destructive) { transfers.cancel(job) } label: {
                                        Label("Cancel", systemImage: "xmark")
                                    }
                                }
                            }
                    }
                }
                .listStyle(.plain)
            }
        }
        .navigationTitle("Transfers")
        .toolbar {
            if transfers.jobs.contains(where: { !$0.isActive }) {
                ToolbarItem(placement: .navigationBarTrailing) {
                    Button("Clear") { transfers.clearFinished() }.font(Theme.Typography.caption)
                }
            }
        }
    }
}

private struct TransferRow: View {
    let job: TransferJob

    var body: some View {
        HStack(spacing: Theme.Spacing.md) {
            Image(systemName: job.kind == .upload ? "arrow.up.circle.fill" : "arrow.down.circle.fill")
                .font(.title2)
                .foregroundColor(iconColor)
            VStack(alignment: .leading, spacing: Theme.Spacing.xxs) {
                Text(job.title).font(Theme.Typography.callout).lineLimit(1)
                statusLine
            }
            Spacer(minLength: 0)
        }
        .padding(.vertical, 2)
    }

    @ViewBuilder
    private var statusLine: some View {
        switch job.status {
        case .inProgress:
            ProgressView(value: max(0, min(1, job.fraction))).tint(Theme.Palette.brand)
            Text("\(Int(job.fraction * 100))% • \(job.kind == .upload ? "Uploading" : "Downloading")")
                .font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary)
        case .completed:
            Label("Completed", systemImage: "checkmark.circle.fill")
                .font(Theme.Typography.caption).foregroundColor(Theme.Palette.success)
        case .failed(let message):
            Label(message, systemImage: "exclamationmark.triangle.fill")
                .font(Theme.Typography.caption).foregroundColor(Theme.Palette.danger).lineLimit(2)
        }
    }

    private var iconColor: Color {
        switch job.status {
        case .completed: return Theme.Palette.success
        case .failed: return Theme.Palette.danger
        case .inProgress: return Theme.Palette.brand
        }
    }
}
