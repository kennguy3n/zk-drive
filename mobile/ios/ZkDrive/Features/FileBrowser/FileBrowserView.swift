import SwiftUI

/// The main file browser: list/grid toggle, pull-to-refresh, breadcrumb,
/// swipe actions, and a FAB for upload / new folder.
struct FileBrowserView: View {
    @StateObject var viewModel: FileBrowserViewModel
    @AppStorage(SettingsStore.defaultListLayoutKey) private var layoutRaw = BrowserLayout.list.rawValue

    @State private var showingCreateFolder = false
    @State private var showingDocumentPicker = false
    @State private var showingCamera = false
    @State private var newFolderName = ""
    @State private var newFolderMode: EncryptionMode = .managedEncrypted
    @State private var shareTarget: ShareTarget?

    private var layout: BrowserLayout {
        get { BrowserLayout(rawValue: layoutRaw) ?? .list }
    }

    var body: some View {
        ZStack(alignment: .bottomTrailing) {
            content
            floatingActionMenu
        }
        .navigationTitle(viewModel.location.title)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .navigationBarTrailing) {
                Button {
                    layoutRaw = layout.toggled.rawValue
                } label: {
                    Image(systemName: layout.toggled.systemImage)
                }
                .accessibilityLabel("Toggle layout")
            }
        }
        .sheet(item: $shareTarget) { target in
            NavigationView {
                SharingView(viewModel: viewModel.sharing(for: target))
            }
            .navigationViewStyle(.stack)
        }
        .task { if viewModel.nodes.isEmpty { await viewModel.load() } }
        .refreshable { await viewModel.refresh() }
        .alert("Something went wrong", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: {
            Text(viewModel.error?.userMessage ?? "")
        }
        .sheet(isPresented: $showingDocumentPicker) {
            DocumentPicker { urls in
                Task { await viewModel.upload(urls: urls) }
            }
        }
        .sheet(isPresented: $showingCamera) {
            CameraPicker { url in
                Task { await viewModel.upload(urls: [url]) }
            }
        }
        .alert("New Folder", isPresented: $showingCreateFolder) {
            TextField("Folder name", text: $newFolderName)
            Button("Create") {
                let name = newFolderName.trimmingCharacters(in: .whitespacesAndNewlines)
                guard !name.isEmpty else { return }
                Task { await viewModel.createFolder(name: name, mode: newFolderMode) }
                newFolderName = ""
            }
            Button("Cancel", role: .cancel) { newFolderName = "" }
        }
    }

    // MARK: Content

    @ViewBuilder
    private var content: some View {
        if viewModel.isLoading && viewModel.nodes.isEmpty {
            ProgressView().frame(maxWidth: .infinity, maxHeight: .infinity)
        } else if viewModel.nodes.isEmpty {
            EmptyStateView(
                systemImage: "folder",
                title: "Nothing here yet",
                message: viewModel.canUpload ? "Upload a file or create a folder to get started." : "Create a folder to get started.",
                actionTitle: viewModel.canUpload ? "Upload a file" : "New folder",
                action: { viewModel.canUpload ? (showingDocumentPicker = true) : (showingCreateFolder = true) }
            )
        } else {
            switch layout {
            case .list: listLayout
            case .grid: gridLayout
            }
        }
    }

    private var listLayout: some View {
        List {
            if case .folder(let folder) = viewModel.location {
                Section {
                    BreadcrumbView(path: folder.path)
                        .listRowInsets(EdgeInsets(top: 4, leading: 16, bottom: 4, trailing: 16))
                }
            }
            ForEach(viewModel.nodes) { node in
                nodeRow(node)
                    .swipeActions(edge: .trailing, allowsFullSwipe: false) {
                        Button(role: .destructive) {
                            Task { await viewModel.delete(node) }
                        } label: { Label("Delete", systemImage: "trash") }
                        Button {
                            shareTarget = ShareTarget(node: node)
                        } label: {
                            Label("Share", systemImage: "square.and.arrow.up")
                        }
                        .tint(Theme.Palette.brand)
                    }
            }
        }
        .listStyle(.plain)
    }

    private var gridLayout: some View {
        ScrollView {
            if case .folder(let folder) = viewModel.location {
                BreadcrumbView(path: folder.path)
                    .padding(.horizontal, Theme.Spacing.lg)
                    .padding(.top, Theme.Spacing.sm)
            }
            LazyVGrid(columns: [GridItem(.adaptive(minimum: 110), spacing: Theme.Spacing.md)], spacing: Theme.Spacing.md) {
                ForEach(viewModel.nodes) { node in
                    nodeGridCell(node)
                }
            }
            .padding(Theme.Spacing.lg)
        }
    }

    // MARK: Rows

    @ViewBuilder
    private func nodeRow(_ node: DriveNode) -> some View {
        switch node {
        case .folder(let folder):
            NavigationLink(destination: FileBrowserView(viewModel: viewModel.child(for: folder))) {
                FolderRow(folder: folder)
            }
        case .file(let file):
            NavigationLink(destination: FilePreviewView(viewModel: viewModel.preview(for: file))) {
                FileRow(file: file)
            }
        }
    }

    @ViewBuilder
    private func nodeGridCell(_ node: DriveNode) -> some View {
        switch node {
        case .folder(let folder):
            NavigationLink(destination: FileBrowserView(viewModel: viewModel.child(for: folder))) {
                GridCell(node: node, encryptionMode: folder.encryptionMode)
            }
            .buttonStyle(.plain)
        case .file(let file):
            NavigationLink(destination: FilePreviewView(viewModel: viewModel.preview(for: file))) {
                GridCell(node: node, encryptionMode: nil)
            }
            .buttonStyle(.plain)
        }
    }

    // MARK: FAB

    private var floatingActionMenu: some View {
        Menu {
            if viewModel.canUpload {
                Button { showingDocumentPicker = true } label: { Label("Upload File", systemImage: "doc.badge.plus") }
                Button { showingCamera = true } label: { Label("Camera", systemImage: "camera") }
            }
            Button { showingCreateFolder = true } label: { Label("New Folder", systemImage: "folder.badge.plus") }
        } label: {
            Image(systemName: "plus")
                .font(.system(size: 22, weight: .bold))
                .foregroundColor(.white)
                .frame(width: 56, height: 56)
                .background(Theme.Palette.brandGradient, in: Circle())
                .shadow(color: .black.opacity(0.2), radius: 8, y: 4)
        }
        .padding(Theme.Spacing.xl)
    }
}

/// A breadcrumb derived from the folder's `/`-delimited path.
struct BreadcrumbView: View {
    let path: String

    private var components: [String] {
        path.split(separator: "/").map(String.init)
    }

    var body: some View {
        ScrollView(.horizontal, showsIndicators: false) {
            HStack(spacing: Theme.Spacing.xs) {
                Image(systemName: "house.fill").font(Theme.Typography.caption)
                ForEach(Array(components.enumerated()), id: \.offset) { _, component in
                    Image(systemName: "chevron.right").font(.system(size: 9)).foregroundColor(Theme.Palette.textTertiary)
                    Text(component).font(Theme.Typography.caption)
                }
            }
            .foregroundColor(Theme.Palette.textSecondary)
        }
    }
}

/// A lightweight value used to present the sharing screen for a node.
struct ShareTarget: Hashable, Identifiable {
    var id: String { "\(resourceType):\(resourceID)" }
    let resourceType: String
    let resourceID: String
    let name: String

    init(node: DriveNode) {
        switch node {
        case .folder(let f): resourceType = "folder"; resourceID = f.id; name = f.name
        case .file(let f): resourceType = "file"; resourceID = f.id; name = f.name
        }
    }
}


