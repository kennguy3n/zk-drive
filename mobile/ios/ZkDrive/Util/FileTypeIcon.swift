import SwiftUI
import UniformTypeIdentifiers

/// Maps a filename / MIME type to an SF Symbol + tint so lists and search
/// results have consistent, recognisable iconography.
enum FileTypeIcon {
    static func symbol(forMime mime: String?, name: String) -> String {
        let ext = (name as NSString).pathExtension.lowercased()
        if let mime, !mime.isEmpty {
            if mime.hasPrefix("image/") { return "photo" }
            if mime.hasPrefix("video/") { return "film" }
            if mime.hasPrefix("audio/") { return "waveform" }
            if mime == "application/pdf" { return "doc.richtext" }
            if mime.hasPrefix("text/") { return "doc.text" }
        }
        switch ext {
        case "pdf": return "doc.richtext"
        case "png", "jpg", "jpeg", "gif", "heic", "webp": return "photo"
        case "mp4", "mov", "m4v", "avi": return "film"
        case "mp3", "wav", "aac", "flac": return "waveform"
        case "txt", "md", "rtf": return "doc.text"
        case "zip", "tar", "gz", "7z": return "doc.zipper"
        case "doc", "docx": return "doc.text.fill"
        case "xls", "xlsx", "csv": return "tablecells"
        case "ppt", "pptx": return "rectangle.on.rectangle"
        default: return "doc"
        }
    }

    static func tint(forMime mime: String?, name: String) -> Color {
        let symbol = self.symbol(forMime: mime, name: name)
        switch symbol {
        case "photo", "film": return .pink
        case "waveform": return .purple
        case "doc.richtext": return .red
        case "doc.text", "doc.text.fill": return .blue
        case "tablecells": return .green
        case "doc.zipper": return .orange
        default: return Theme.Palette.brand
        }
    }
}
