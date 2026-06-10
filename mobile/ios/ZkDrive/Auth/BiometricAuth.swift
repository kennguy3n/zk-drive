import Foundation
import LocalAuthentication

/// Face ID / Touch ID gate for returning users. The tokens already live
/// in the Keychain; biometric unlock is an app-level confirmation before
/// they are loaded into memory, matching the "lock the vault" UX users
/// expect from a privacy product.
struct BiometricAuth {
    enum Availability: Equatable {
        case faceID
        case touchID
        case opticID
        case none(reason: String)
    }

    /// What biometric the device offers right now.
    static func availability() -> Availability {
        let context = LAContext()
        var error: NSError?
        guard context.canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: &error) else {
            return .none(reason: error?.localizedDescription ?? "Biometrics unavailable")
        }
        switch context.biometryType {
        case .faceID: return .faceID
        case .touchID: return .touchID
        case .opticID: return .opticID
        case .none: return .none(reason: "No biometric enrolled")
        @unknown default: return .none(reason: "Unknown biometric type")
        }
    }

    static var isAvailable: Bool {
        if case .none = availability() { return false }
        return true
    }

    /// Prompt the user to authenticate. Falls back to the device
    /// passcode if biometrics fail, so a user who can't use Face ID
    /// (e.g. mask, gloves) is not locked out of their own data.
    static func authenticate(reason: String) async throws {
        let context = LAContext()
        context.localizedFallbackTitle = "Use Passcode"
        do {
            let ok = try await context.evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason)
            if !ok {
                throw AppError(category: .auth, message: "Biometric authentication failed", httpStatus: nil)
            }
        } catch let laError as LAError {
            throw AppError(category: .auth, message: laError.localizedDescription, httpStatus: nil)
        }
    }
}
