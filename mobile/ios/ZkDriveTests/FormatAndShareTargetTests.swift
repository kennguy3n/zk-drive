import XCTest
@testable import ZkDrive

final class FormatTests: XCTestCase {
    func testNegativeByteCountClampsToZero() {
        XCTAssertEqual(Format.bytes(Int64(-100)), Format.bytes(Int64(0)))
    }

    func testUInt64OverflowClampsRatherThanTrapping() {
        // Int64(clamping:) must keep this from trapping on the conversion.
        let huge = Format.bytes(UInt64.max)
        XCTAssertFalse(huge.isEmpty)
    }

    func testByteFormattingProducesNonEmptyHumanString() {
        XCTAssertFalse(Format.bytes(Int64(1_500_000)).isEmpty)
    }
}

final class ShareTargetTests: XCTestCase {
    func testFolderNodeMapsToFolderResource() {
        let folder = Folder(
            id: "fld_9", workspaceID: "ws", parentFolderID: nil, name: "Docs",
            path: "/Docs", encryptionMode: .managedEncrypted,
            createdAt: Date(), updatedAt: Date()
        )
        let target = ShareTarget(node: .folder(folder))
        XCTAssertEqual(target.resourceType, "folder")
        XCTAssertEqual(target.resourceID, "fld_9")
        XCTAssertEqual(target.name, "Docs")
        XCTAssertEqual(target.id, "folder:fld_9")
    }

    func testFileNodeMapsToFileResource() {
        let file = FileItem(
            id: "file_3", workspaceID: "ws", folderID: "fld", name: "a.pdf",
            currentVersionID: "v1", sizeBytes: 10, mimeType: "application/pdf",
            createdAt: Date(), updatedAt: Date()
        )
        let target = ShareTarget(node: .file(file))
        XCTAssertEqual(target.resourceType, "file")
        XCTAssertEqual(target.resourceID, "file_3")
        XCTAssertEqual(target.id, "file:file_3")
    }

    func testDriveNodeIdIsPrefixedToAvoidCollisions() {
        let folder = Folder(
            id: "shared_id", workspaceID: "ws", parentFolderID: nil, name: "F",
            path: "/F", encryptionMode: .managedEncrypted, createdAt: Date(), updatedAt: Date()
        )
        let file = FileItem(
            id: "shared_id", workspaceID: "ws", folderID: "fld", name: "f",
            currentVersionID: nil, sizeBytes: 1, mimeType: "text/plain",
            createdAt: Date(), updatedAt: Date()
        )
        // Same underlying id, different node kind → distinct list identity.
        XCTAssertNotEqual(DriveNode.folder(folder).id, DriveNode.file(file).id)
    }
}
