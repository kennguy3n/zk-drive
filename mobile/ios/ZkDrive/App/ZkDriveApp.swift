import SwiftUI

@main
struct ZkDriveApp: App {
    @UIApplicationDelegateAdaptor(AppDelegate.self) private var appDelegate
    @StateObject private var bootstrap = AppBootstrap()
    @AppStorage(SettingsStore.appearanceKey) private var appearanceRaw = AppearancePreference.system.rawValue

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(bootstrap)
                .tint(Theme.Palette.brand)
                .preferredColorScheme(AppearancePreference(rawValue: appearanceRaw)?.colorScheme)
                .task { await bootstrap.start() }
        }
    }
}

/// Switches between the loading splash, a hard-failure screen, and the
/// authenticated/unauthenticated app based on bootstrap + auth state.
struct RootView: View {
    @EnvironmentObject private var bootstrap: AppBootstrap

    var body: some View {
        switch bootstrap.phase {
        case .loading:
            LaunchView()
        case .failed(let error):
            StartupErrorView(error: error)
        case .ready(let services):
            AuthGate(services: services)
                .environmentObject(services.auth)
                .environmentObject(services.transfers)
                .environmentObject(services.sync)
                .environmentObject(services.push)
        }
    }
}

/// Routes on authentication state once services are ready.
private struct AuthGate: View {
    let services: AppServices
    @EnvironmentObject private var auth: AuthService

    var body: some View {
        Group {
            switch auth.state {
            case .loading:
                LaunchView()
            case .signedOut:
                LoginView()
            case .locked:
                BiometricLockView()
            case .signedIn:
                MainTabView(services: services)
            }
        }
        .animation(.easeInOut(duration: 0.25), value: auth.state)
    }
}

/// Brand splash shown during bootstrap.
struct LaunchView: View {
    var body: some View {
        ZStack {
            Theme.Palette.background.ignoresSafeArea()
            VStack(spacing: Theme.Spacing.lg) {
                BrandMark(size: 72)
                ProgressView().tint(Theme.Palette.brand)
            }
        }
    }
}

/// Unrecoverable startup error (e.g. malformed config). Offers a retry.
struct StartupErrorView: View {
    let error: AppError
    @EnvironmentObject private var bootstrap: AppBootstrap

    var body: some View {
        ZStack {
            Theme.Palette.background.ignoresSafeArea()
            VStack(spacing: Theme.Spacing.lg) {
                Image(systemName: "exclamationmark.icloud")
                    .font(.system(size: 52))
                    .foregroundColor(Theme.Palette.danger)
                Text("Couldn't start ZK Drive").font(Theme.Typography.title)
                Text(error.userMessage)
                    .font(Theme.Typography.callout)
                    .foregroundColor(Theme.Palette.textSecondary)
                    .multilineTextAlignment(.center)
                Button("Try Again") { Task { await bootstrap.start() } }
                    .buttonStyle(PrimaryButtonStyle())
                    .fixedSize()
            }
            .padding(Theme.Spacing.xl)
        }
    }
}

/// The ZK Drive logomark — a shield + key glyph composed from SF Symbols
/// so it scales crisply without bundled art.
struct BrandMark: View {
    var size: CGFloat = 48

    var body: some View {
        ZStack {
            RoundedRectangle(cornerRadius: size * 0.28, style: .continuous)
                .fill(Theme.Palette.brandGradient)
                .frame(width: size, height: size)
            Image(systemName: "lock.shield.fill")
                .font(.system(size: size * 0.5, weight: .bold))
                .foregroundStyle(.white)
        }
        .accessibilityLabel("ZK Drive")
    }
}
