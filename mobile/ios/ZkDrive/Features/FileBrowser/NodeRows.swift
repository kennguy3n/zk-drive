import SwiftUI

/// A folder row in the list layout.
struct FolderRow: View {
    let folder: Folder

    var body: some View {
        HStack(spacing: Theme.Spacing.md) {
            Image(systemName: "folder.fill")
                .font(.title2)
                .foregroundStyle(Theme.Palette.brandGradient)
                .frame(width: 36)
            VStack(alignment: .leading, spacing: 2) {
                Text(folder.name).font(Theme.Typography.body).lineLimit(1)
                Text("Updated \(Format.relative(folder.updatedAt))")
                    .font(Theme.Typography.caption)
                    .foregroundColor(Theme.Palette.textTertiary)
            }
            Spacer()
            EncryptionBadge(mode: folder.encryptionMode)
        }
        .padding(.vertical, 4)
    }
}

/// A file row in the list layout.
struct FileRow: View {
    let file: FileItem

    var body: some View {
        HStack(spacing: Theme.Spacing.md) {
            Image(systemName: FileTypeIcon.symbol(forMime: file.mimeType, name: file.name))
                .font(.title2)
                .foregroundColor(FileTypeIcon.tint(forMime: file.mimeType, name: file.name))
                .frame(width: 36)
            VStack(alignment: .leading, spacing: 2) {
                Text(file.name).font(Theme.Typography.body).lineLimit(1)
                Text("\(Format.bytes(file.sizeBytes)) · \(Format.relative(file.updatedAt))")
                    .font(Theme.Typography.caption)
                    .foregroundColor(Theme.Palette.textTertiary)
            }
            Spacer()
        }
        .padding(.vertical, 4)
    }
}

/// A grid cell used by the grid layout for both folders and files.
struct GridCell: View {
    let node: DriveNode
    let encryptionMode: EncryptionMode?

    var body: some View {
        VStack(spacing: Theme.Spacing.sm) {
            ZStack(alignment: .topTrailing) {
                RoundedRectangle(cornerRadius: Theme.Radius.md, style: .continuous)
                    .fill(Theme.Palette.surface)
                    .frame(height: 92)
                    .overlay(icon)
                if let encryptionMode {
                    EncryptionBadge(mode: encryptionMode)
                        .scaleEffect(0.85)
                        .padding(Theme.Spacing.xs)
                }
            }
            Text(node.name)
                .font(Theme.Typography.caption)
                .lineLimit(2)
                .multilineTextAlignment(.center)
                .foregroundColor(Theme.Palette.textPrimary)
                .frame(maxWidth: .infinity)
        }
    }

    @ViewBuilder
    private var icon: some View {
        switch node {
        case .folder:
            Image(systemName: "folder.fill")
                .font(.system(size: 34))
                .foregroundStyle(Theme.Palette.brandGradient)
        case .file(let file):
            Image(systemName: FileTypeIcon.symbol(forMime: file.mimeType, name: file.name))
                .font(.system(size: 32))
                .foregroundColor(FileTypeIcon.tint(forMime: file.mimeType, name: file.name))
        }
    }
}
