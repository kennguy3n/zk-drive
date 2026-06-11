import Foundation

enum Format {
    private static let byteFormatter: ByteCountFormatter = {
        let f = ByteCountFormatter()
        f.countStyle = .file
        f.allowsNonnumericFormatting = true
        return f
    }()

    static func bytes(_ count: Int64) -> String {
        byteFormatter.string(fromByteCount: max(0, count))
    }

    static func bytes(_ count: UInt64) -> String {
        bytes(Int64(clamping: count))
    }

    private static let relative: RelativeDateTimeFormatter = {
        let f = RelativeDateTimeFormatter()
        f.unitsStyle = .abbreviated
        return f
    }()

    static func relative(_ date: Date) -> String {
        relative.localizedString(for: date, relativeTo: Date())
    }

    static func shortDate(_ date: Date) -> String {
        date.formatted(date: .abbreviated, time: .shortened)
    }
}
