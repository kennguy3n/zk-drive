import Foundation

/// A single, UI-facing error type that both the Rust bridge
/// (`BridgeError`) and the native REST/storage layers normalise into.
/// Screens branch on the `category` to decide whether to retry, bounce
/// the user to sign-in, or simply show a message.
struct AppError: Error, Identifiable, Equatable {
    enum Category: Equatable {
        case auth            // re-authentication required
        case network         // transient transport failure — retryable
        case permission      // 403 — caller lacks access
        case notFound        // 404
        case server          // 5xx
        case crypto          // decrypt/encrypt failure
        case storage         // local catalogue / disk
        case invalidInput    // 4xx client error / bad argument — not retryable
        case cancelled       // user aborted the operation (e.g. dismissed sign-in)
        case unknown
    }

    let id = UUID()
    let category: Category
    let message: String
    /// HTTP status when the failure originated from an API response.
    let httpStatus: Int?

    static func == (lhs: AppError, rhs: AppError) -> Bool {
        lhs.category == rhs.category && lhs.message == rhs.message && lhs.httpStatus == rhs.httpStatus
    }

    var isRetryable: Bool {
        switch category {
        case .network, .server: return true
        default: return false
        }
    }

    var requiresReauth: Bool { category == .auth || httpStatus == 401 }

    /// A concise, user-presentable message — never leaks stack-like
    /// internals, just the category-appropriate guidance plus the
    /// underlying detail.
    var userMessage: String {
        switch category {
        case .auth: return "Your session expired. Please sign in again."
        case .network: return "Network problem. Check your connection and try again."
        case .permission: return "You don't have permission to do that."
        case .notFound: return "That item could not be found."
        case .server: return "The server had a problem. Please try again shortly."
        case .crypto: return "Could not decrypt this file. The key may be wrong."
        case .storage: return "Local storage error. Try restarting the app."
        case .invalidInput, .unknown, .cancelled: return message
        }
    }
}

extension AppError {
    /// Normalise a generated `BridgeError`. The bridge flattens its error
    /// enum, so the API status code is embedded in the message as
    /// "api: status NNN: …" — parse it back out so the UI can branch on
    /// 401/403/404 the same way it does for native REST errors.
    static func from(_ error: BridgeError) -> AppError {
        switch error {
        case .Crypto(let m):
            return AppError(category: .crypto, message: m, httpStatus: nil)
        case .Auth(let m):
            return AppError(category: .auth, message: m, httpStatus: 401)
        case .Network(let m):
            return AppError(category: .network, message: m, httpStatus: nil)
        case .Catalogue(let m):
            return AppError(category: .storage, message: m, httpStatus: nil)
        case .InvalidInput(let m):
            return AppError(category: .invalidInput, message: m, httpStatus: nil)
        case .Api(let m):
            let status = Self.parseAPIStatus(from: m)
            return AppError(category: Self.category(forStatus: status), message: m, httpStatus: status)
        }
    }

    /// Build from a raw HTTP status + server message (native REST layer).
    static func fromHTTP(status: Int, message: String) -> AppError {
        AppError(category: category(forStatus: status), message: message, httpStatus: status)
    }

    static func network(_ message: String) -> AppError {
        AppError(category: .network, message: message, httpStatus: nil)
    }

    static func unknown(_ message: String) -> AppError {
        AppError(category: .unknown, message: message, httpStatus: nil)
    }

    private static func category(forStatus status: Int?) -> Category {
        // No HTTP response at all means the request never reached the server
        // (DNS, timeout, connection lost): a genuine transport failure, which
        // is safe to retry.
        guard let status else { return .network }
        switch status {
        case 401: return .auth
        case 403: return .permission
        case 404: return .notFound
        // Request-timeout and rate-limit are transient — retry with backoff.
        case 408, 429: return .server
        case 500...: return .server
        // Every other 4xx (400, 409, 422, …) is a client error: the identical
        // request will keep failing, so it must NOT be marked retryable.
        case 400..<500: return .invalidInput
        default: return .unknown
        }
    }

    /// Extract NNN from "api: status NNN: message".
    private static func parseAPIStatus(from message: String) -> Int? {
        guard let range = message.range(of: "status ") else { return nil }
        let tail = message[range.upperBound...]
        let digits = tail.prefix { $0.isNumber }
        return Int(digits)
    }
}

extension Error {
    /// Convert any error thrown across the app into a normalised
    /// `AppError`, recognising the bridge type, `URLError`, and existing
    /// `AppError`s.
    func asAppError() -> AppError {
        if let app = self as? AppError { return app }
        if let bridge = self as? BridgeError { return .from(bridge) }
        if let urlError = self as? URLError {
            switch urlError.code {
            case .notConnectedToInternet, .timedOut, .networkConnectionLost, .cannotConnectToHost:
                return .network(urlError.localizedDescription)
            default:
                return .network(urlError.localizedDescription)
            }
        }
        return .unknown(localizedDescription)
    }
}
