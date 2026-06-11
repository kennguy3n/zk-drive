import XCTest
@testable import ZkDrive

/// Decoding tests for the domain models + shared JSON coders. These pin
/// the Swift contract to the zk-drive Go API's snake_case JSON and its
/// RFC3339 timestamps (with and without fractional seconds).
final class ModelCodingTests: XCTestCase {
    func testEncryptionModeDecodesKnownValues() throws {
        XCTAssertEqual(try decodeMode("\"managed_encrypted\""), .managedEncrypted)
        XCTAssertEqual(try decodeMode("\"strict_zk\""), .strictZK)
    }

    func testEncryptionModeUnknownValueFallsBackToManaged() throws {
        // Forward-compatibility: an unrecognised server value must not
        // fail the whole payload — it decodes to the safe default.
        XCTAssertEqual(try decodeMode("\"some_future_mode\""), .managedEncrypted)
    }

    func testEncryptionModeLabels() {
        XCTAssertEqual(EncryptionMode.managedEncrypted.shortLabel, "Confidential")
        XCTAssertEqual(EncryptionMode.strictZK.shortLabel, "Zero-Knowledge")
    }

    func testFolderDecodesFromAPIShape() throws {
        let json = """
        {
          "id": "fld_1",
          "workspace_id": "ws_1",
          "parent_folder_id": null,
          "name": "Contracts",
          "path": "/Contracts",
          "encryption_mode": "strict_zk",
          "created_at": "2024-01-02T03:04:05Z",
          "updated_at": "2024-01-02T03:04:05.123Z"
        }
        """.data(using: .utf8)!

        let folder = try JSONCoding.decoder.decode(Folder.self, from: json)
        XCTAssertEqual(folder.id, "fld_1")
        XCTAssertEqual(folder.workspaceID, "ws_1")
        XCTAssertNil(folder.parentFolderID)
        XCTAssertEqual(folder.name, "Contracts")
        XCTAssertEqual(folder.encryptionMode, .strictZK)
    }

    func testJSONCodingHandlesFractionalAndPlainTimestamps() throws {
        struct Holder: Codable, Equatable { let at: Date }
        let plain = try JSONCoding.decoder.decode(Holder.self, from: #"{"at":"2024-06-01T12:00:00Z"}"#.data(using: .utf8)!)
        let frac = try JSONCoding.decoder.decode(Holder.self, from: #"{"at":"2024-06-01T12:00:00.500Z"}"#.data(using: .utf8)!)
        XCTAssertEqual(frac.at.timeIntervalSince(plain.at), 0.5, accuracy: 0.001)
    }

    func testJSONCodingRejectsUnparseableDate() {
        struct Holder: Codable { let at: Date }
        XCTAssertThrowsError(try JSONCoding.decoder.decode(Holder.self, from: #"{"at":"yesterday"}"#.data(using: .utf8)!))
    }

    func testWorkspaceUsedFractionIsClamped() {
        let full = Workspace(id: "w", name: "n", storageQuotaBytes: 100, storageUsedBytes: 250, tier: "pro")
        XCTAssertEqual(full.usedFraction, 1.0)
        let zeroQuota = Workspace(id: "w", name: "n", storageQuotaBytes: 0, storageUsedBytes: 10, tier: "free")
        XCTAssertEqual(zeroQuota.usedFraction, 0)
    }

    private func decodeMode(_ raw: String) throws -> EncryptionMode {
        try JSONDecoder().decode(EncryptionMode.self, from: raw.data(using: .utf8)!)
    }
}
