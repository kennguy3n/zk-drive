import SwiftUI

// Reusable view building blocks shared across screens. Keeping them in
// one place enforces visual consistency and keeps feature views focused
// on behaviour rather than styling.

/// The primary call-to-action button (filled, brand-coloured).
struct PrimaryButtonStyle: ButtonStyle {
    var isLoading: Bool = false

    func makeBody(configuration: Configuration) -> some View {
        HStack(spacing: Theme.Spacing.sm) {
            if isLoading {
                ProgressView()
                    .progressViewStyle(.circular)
                    .tint(.white)
            }
            configuration.label
                .font(Theme.Typography.headline)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, Theme.Spacing.md)
        .padding(.horizontal, Theme.Spacing.lg)
        .background(Theme.Palette.brand.opacity(configuration.isPressed ? 0.8 : 1))
        .foregroundColor(.white)
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.md, style: .continuous))
        .opacity(isLoading ? 0.85 : 1)
        .animation(.easeOut(duration: 0.12), value: configuration.isPressed)
    }
}

/// A subdued secondary button (tinted, bordered).
struct SecondaryButtonStyle: ButtonStyle {
    func makeBody(configuration: Configuration) -> some View {
        configuration.label
            .font(Theme.Typography.headline)
            .frame(maxWidth: .infinity)
            .padding(.vertical, Theme.Spacing.md)
            .padding(.horizontal, Theme.Spacing.lg)
            .background(Theme.Palette.brand.opacity(configuration.isPressed ? 0.18 : 0.1))
            .foregroundColor(Theme.Palette.brand)
            .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.md, style: .continuous))
    }
}

/// A card container with consistent padding, radius and elevation.
struct Card<Content: View>: View {
    @ViewBuilder var content: Content

    var body: some View {
        content
            .padding(Theme.Spacing.lg)
            .background(Theme.Palette.surface)
            .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.lg, style: .continuous))
    }
}

/// The per-folder encryption indicator. ZK Drive distinguishes
/// server-managed encryption from true zero-knowledge folders; surfacing
/// this on every file/folder is a core trust signal for SME tenants.
struct EncryptionBadge: View {
    let mode: EncryptionMode

    var body: some View {
        Label(mode.shortLabel, systemImage: mode.systemImage)
            .font(Theme.Typography.caption.weight(.semibold))
            .padding(.horizontal, Theme.Spacing.sm)
            .padding(.vertical, Theme.Spacing.xxs)
            .background(mode.tint.opacity(0.16))
            .foregroundColor(mode.tint)
            .clipShape(Capsule())
            .accessibilityLabel(mode.accessibilityLabel)
    }
}

/// Illustrated empty-state used by browser/search/notifications when
/// there is nothing to show.
struct EmptyStateView: View {
    let systemImage: String
    let title: String
    let message: String
    var actionTitle: String?
    var action: (() -> Void)?

    var body: some View {
        VStack(spacing: Theme.Spacing.lg) {
            Image(systemName: systemImage)
                .font(.system(size: 52, weight: .regular))
                .foregroundStyle(Theme.Palette.brandGradient)
                .padding(Theme.Spacing.lg)
                .background(Theme.Palette.brand.opacity(0.08), in: Circle())
            VStack(spacing: Theme.Spacing.xs) {
                Text(title).font(Theme.Typography.title)
                Text(message)
                    .font(Theme.Typography.callout)
                    .foregroundColor(Theme.Palette.textSecondary)
                    .multilineTextAlignment(.center)
            }
            if let actionTitle, let action {
                Button(actionTitle, action: action)
                    .buttonStyle(SecondaryButtonStyle())
                    .fixedSize()
            }
        }
        .padding(Theme.Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// A thin inline banner for surfacing recoverable errors without a modal.
struct InlineBanner: View {
    enum Kind { case error, info, success }
    let kind: Kind
    let message: String

    private var tint: Color {
        switch kind {
        case .error: return Theme.Palette.danger
        case .info: return Theme.Palette.brand
        case .success: return Theme.Palette.success
        }
    }

    private var icon: String {
        switch kind {
        case .error: return "exclamationmark.triangle.fill"
        case .info: return "info.circle.fill"
        case .success: return "checkmark.circle.fill"
        }
    }

    var body: some View {
        HStack(alignment: .top, spacing: Theme.Spacing.sm) {
            Image(systemName: icon)
            Text(message).font(Theme.Typography.footnote)
            Spacer(minLength: 0)
        }
        .padding(Theme.Spacing.md)
        .foregroundColor(tint)
        .background(tint.opacity(0.12))
        .clipShape(RoundedRectangle(cornerRadius: Theme.Radius.md, style: .continuous))
    }
}

/// A determinate/indeterminate progress row used by upload & download
/// queues.
struct TransferProgressRow: View {
    let title: String
    let subtitle: String
    let fraction: Double?

    var body: some View {
        VStack(alignment: .leading, spacing: Theme.Spacing.xs) {
            HStack {
                Text(title).font(Theme.Typography.callout).lineLimit(1)
                Spacer()
                if let fraction {
                    Text("\(Int(fraction * 100))%")
                        .font(Theme.Typography.caption.monospacedDigit())
                        .foregroundColor(Theme.Palette.textSecondary)
                }
            }
            if let fraction {
                ProgressView(value: max(0, min(1, fraction)))
                    .tint(Theme.Palette.brand)
            } else {
                ProgressView().progressViewStyle(.linear).tint(Theme.Palette.brand)
            }
            Text(subtitle)
                .font(Theme.Typography.caption)
                .foregroundColor(Theme.Palette.textTertiary)
                .lineLimit(1)
        }
    }
}

/// A key/value row (label leading, value trailing). Mirrors iOS 16's
/// `LabeledContent` so we stay on the iOS 15 deployment target.
struct KeyValueRow: View {
    let label: String
    let value: String

    init(_ label: String, value: String) {
        self.label = label
        self.value = value
    }

    var body: some View {
        HStack {
            Text(label)
            Spacer(minLength: Theme.Spacing.md)
            Text(value)
                .foregroundColor(Theme.Palette.textSecondary)
                .multilineTextAlignment(.trailing)
        }
    }
}
