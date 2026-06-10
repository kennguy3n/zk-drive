import XCTest
@testable import ZkDrive

/// Exercises the error-normalisation layer that every screen branches on
/// for retry / re-auth / message decisions.
final class AppErrorTests: XCTestCase {
    func testHTTPStatusMapsToCategory() {
        XCTAssertEqual(AppError.fromHTTP(status: 401, message: "x").category, .auth)
        XCTAssertEqual(AppError.fromHTTP(status: 403, message: "x").category, .permission)
        XCTAssertEqual(AppError.fromHTTP(status: 404, message: "x").category, .notFound)
        XCTAssertEqual(AppError.fromHTTP(status: 500, message: "x").category, .server)
        XCTAssertEqual(AppError.fromHTTP(status: 503, message: "x").category, .server)
        // Timeout / rate-limit are transient, so they map to the retryable
        // server bucket rather than a permanent client error.
        XCTAssertEqual(AppError.fromHTTP(status: 408, message: "x").category, .server)
        XCTAssertEqual(AppError.fromHTTP(status: 429, message: "x").category, .server)
        // Other 4xx are client errors that will keep failing — not retryable.
        XCTAssertEqual(AppError.fromHTTP(status: 400, message: "x").category, .invalidInput)
        XCTAssertEqual(AppError.fromHTTP(status: 409, message: "x").category, .invalidInput)
        XCTAssertEqual(AppError.fromHTTP(status: 422, message: "x").category, .invalidInput)
        XCTAssertEqual(AppError.fromHTTP(status: 418, message: "x").category, .invalidInput)
    }

    func testClientErrorsAreNotRetryable() {
        // A 4xx (other than timeout/rate-limit) must never be retried — doing
        // so would just hammer the server with the same rejected request.
        XCTAssertFalse(AppError.fromHTTP(status: 400, message: "x").isRetryable)
        XCTAssertFalse(AppError.fromHTTP(status: 409, message: "x").isRetryable)
        XCTAssertFalse(AppError.fromHTTP(status: 422, message: "x").isRetryable)
        // Transient statuses remain retryable.
        XCTAssertTrue(AppError.fromHTTP(status: 408, message: "x").isRetryable)
        XCTAssertTrue(AppError.fromHTTP(status: 429, message: "x").isRetryable)
    }

    func testBridgeApiErrorParsesEmbeddedStatus() {
        let err = AppError.from(.Api(message: "api: status 404: folder not found"))
        XCTAssertEqual(err.category, .notFound)
        XCTAssertEqual(err.httpStatus, 404)
    }

    func testBridgeApiErrorWithoutStatusIsNetwork() {
        let err = AppError.from(.Api(message: "api: connection reset"))
        XCTAssertNil(err.httpStatus)
        XCTAssertEqual(err.category, .network)
    }

    func testBridgeErrorCategoryMapping() {
        XCTAssertEqual(AppError.from(.Crypto(message: "x")).category, .crypto)
        XCTAssertEqual(AppError.from(.Auth(message: "x")).category, .auth)
        XCTAssertEqual(AppError.from(.Network(message: "x")).category, .network)
        XCTAssertEqual(AppError.from(.Catalogue(message: "x")).category, .storage)
        XCTAssertEqual(AppError.from(.InvalidInput(message: "x")).category, .invalidInput)
    }

    func testRetryAndReauthSemantics() {
        XCTAssertTrue(AppError.fromHTTP(status: 500, message: "x").isRetryable)
        XCTAssertTrue(AppError.network("x").isRetryable)
        XCTAssertFalse(AppError.fromHTTP(status: 403, message: "x").isRetryable)

        XCTAssertTrue(AppError.from(.Auth(message: "x")).requiresReauth)
        XCTAssertTrue(AppError.fromHTTP(status: 401, message: "x").requiresReauth)
        XCTAssertFalse(AppError.fromHTTP(status: 404, message: "x").requiresReauth)
    }

    func testCancelledIsNeitherRetryableNorReauth() {
        // A user-aborted action (e.g. dismissing the sign-in sheet) is its
        // own category so callers can suppress it without fragile string
        // matching, and it must never trigger a retry or re-auth bounce.
        let cancelled = AppError(category: .cancelled, message: "Sign-in was cancelled", httpStatus: nil)
        XCTAssertFalse(cancelled.isRetryable)
        XCTAssertFalse(cancelled.requiresReauth)
        XCTAssertEqual(cancelled.userMessage, "Sign-in was cancelled")
    }

    func testInvalidInputUserMessagePassesThroughDetail() {
        let err = AppError(category: .invalidInput, message: "Folder name required", httpStatus: nil)
        XCTAssertEqual(err.userMessage, "Folder name required")
    }

    func testURLErrorNormalisesToNetwork() {
        let urlError = URLError(.notConnectedToInternet)
        XCTAssertEqual(urlError.asAppError().category, .network)
    }

    func testExistingAppErrorPassesThroughUnchanged() {
        let original = AppError(category: .permission, message: "nope", httpStatus: 403)
        XCTAssertEqual(original.asAppError(), original)
    }
}
