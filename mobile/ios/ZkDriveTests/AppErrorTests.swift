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
        // Anything else falls back to a retryable network error.
        XCTAssertEqual(AppError.fromHTTP(status: 418, message: "x").category, .network)
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
