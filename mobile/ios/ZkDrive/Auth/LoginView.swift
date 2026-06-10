import SwiftUI

/// The unauthenticated entry screen. Sign-in delegates entirely to
/// iam-core via the system browser (OAuth2 + PKCE); the app never shows a
/// password field.
struct LoginView: View {
    @EnvironmentObject private var auth: AuthService
    @State private var isSigningIn = false

    var body: some View {
        ZStack {
            Theme.Palette.background.ignoresSafeArea()
            VStack(spacing: Theme.Spacing.xl) {
                Spacer()
                VStack(spacing: Theme.Spacing.lg) {
                    BrandMark(size: 88)
                    VStack(spacing: Theme.Spacing.xs) {
                        Text("ZK Drive").font(Theme.Typography.largeTitle)
                        Text("Private, zero-knowledge storage for your team")
                            .font(Theme.Typography.callout)
                            .foregroundColor(Theme.Palette.textSecondary)
                            .multilineTextAlignment(.center)
                    }
                }
                Spacer()
                VStack(spacing: Theme.Spacing.md) {
                    if let error = auth.lastError {
                        InlineBanner(kind: .error, message: error.userMessage)
                    }
                    Button {
                        Task {
                            isSigningIn = true
                            await auth.signIn()
                            isSigningIn = false
                        }
                    } label: {
                        Text("Sign in")
                    }
                    .buttonStyle(PrimaryButtonStyle(isLoading: isSigningIn))
                    .disabled(isSigningIn)

                    Label("Secured by end-to-end encryption", systemImage: "lock.shield")
                        .font(Theme.Typography.footnote)
                        .foregroundColor(Theme.Palette.textTertiary)
                }
                .padding(.horizontal, Theme.Spacing.xl)
                .padding(.bottom, Theme.Spacing.xxl)
            }
        }
    }
}

/// Shown to a returning user whose tokens are in the Keychain but locked
/// behind biometrics.
struct BiometricLockView: View {
    @EnvironmentObject private var auth: AuthService
    @State private var isUnlocking = false

    private var biometricLabel: String {
        switch BiometricAuth.availability() {
        case .faceID: return "Unlock with Face ID"
        case .touchID: return "Unlock with Touch ID"
        case .opticID: return "Unlock with Optic ID"
        case .none: return "Unlock"
        }
    }

    private var biometricIcon: String {
        switch BiometricAuth.availability() {
        case .faceID, .opticID: return "faceid"
        case .touchID: return "touchid"
        case .none: return "lock.fill"
        }
    }

    var body: some View {
        ZStack {
            Theme.Palette.background.ignoresSafeArea()
            VStack(spacing: Theme.Spacing.xl) {
                Spacer()
                BrandMark(size: 80)
                Text("ZK Drive is locked").font(Theme.Typography.title)
                if let error = auth.lastError {
                    InlineBanner(kind: .error, message: error.userMessage)
                        .padding(.horizontal, Theme.Spacing.xl)
                }
                Spacer()
                VStack(spacing: Theme.Spacing.md) {
                    Button {
                        Task {
                            isUnlocking = true
                            await auth.unlockWithBiometrics()
                            isUnlocking = false
                        }
                    } label: {
                        Label(biometricLabel, systemImage: biometricIcon)
                    }
                    .buttonStyle(PrimaryButtonStyle(isLoading: isUnlocking))
                    .disabled(isUnlocking)

                    Button("Sign in with a different account") {
                        Task { await auth.signOut() }
                    }
                    .buttonStyle(SecondaryButtonStyle())
                }
                .padding(.horizontal, Theme.Spacing.xl)
                .padding(.bottom, Theme.Spacing.xxl)
            }
        }
        .task {
            // Offer the biometric prompt immediately on appear.
            isUnlocking = true
            await auth.unlockWithBiometrics()
            isUnlocking = false
        }
    }
}
