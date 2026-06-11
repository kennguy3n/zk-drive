import SwiftUI
import PDFKit

struct FilePreviewView: View {
    @StateObject var viewModel: FilePreviewViewModel
    @State private var showingShare = false

    var body: some View {
        Group {
            switch viewModel.content {
            case .loading:
                ProgressView("Loading preview…").frame(maxWidth: .infinity, maxHeight: .infinity)
            case .image(let url):
                imagePreview(url)
            case .pdf(let url):
                PDFKitView(url: url).ignoresSafeArea(edges: .bottom)
            case .text(let string):
                ScrollView { Text(string).font(.system(.body, design: .monospaced)).padding().frame(maxWidth: .infinity, alignment: .leading) }
            case .offline(let url):
                unsupportedOrDownloaded(localURL: url)
            case .unsupported:
                unsupportedOrDownloaded(localURL: nil)
            }
        }
        .navigationTitle(viewModel.file.name)
        .navigationBarTitleDisplayMode(.inline)
        .toolbar {
            ToolbarItem(placement: .navigationBarTrailing) {
                Button {
                    Task {
                        await viewModel.prepareShare()
                        if viewModel.shareURL != nil { showingShare = true }
                    }
                } label: {
                    if viewModel.isPreparingShare { ProgressView() } else { Image(systemName: "square.and.arrow.up") }
                }
                .disabled(viewModel.isPreparingShare)
            }
        }
        .task { await viewModel.load() }
        .sheet(isPresented: $showingShare) {
            if let url = viewModel.shareURL { ActivityView(items: [url]) }
        }
        .alert("Preview error", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: { Text(viewModel.error?.userMessage ?? "") }
    }

    private func imagePreview(_ url: URL) -> some View {
        AsyncImage(url: url) { phase in
            switch phase {
            case .empty: ProgressView()
            case .success(let image): image.resizable().scaledToFit()
            case .failure: Image(systemName: "photo").font(.largeTitle).foregroundColor(Theme.Palette.textTertiary)
            @unknown default: EmptyView()
            }
        }
        .frame(maxWidth: .infinity, maxHeight: .infinity)
        .background(Color.black.opacity(0.02))
    }

    private func unsupportedOrDownloaded(localURL: URL?) -> some View {
        VStack(spacing: Theme.Spacing.lg) {
            Image(systemName: FileTypeIcon.symbol(forMime: viewModel.file.mimeType, name: viewModel.file.name))
                .font(.system(size: 56))
                .foregroundColor(FileTypeIcon.tint(forMime: viewModel.file.mimeType, name: viewModel.file.name))
            Text(viewModel.file.name).font(Theme.Typography.headline).multilineTextAlignment(.center)
            Text("\(Format.bytes(viewModel.file.sizeBytes)) · No inline preview")
                .font(Theme.Typography.footnote).foregroundColor(Theme.Palette.textSecondary)
            VStack(spacing: Theme.Spacing.sm) {
                Button {
                    Task {
                        await viewModel.prepareShare()
                        if viewModel.shareURL != nil { showingShare = true }
                    }
                } label: { Label("Download & Share", systemImage: "square.and.arrow.up") }
                .buttonStyle(PrimaryButtonStyle(isLoading: viewModel.isPreparingShare))

                if !viewModel.hasOfflineCopy {
                    Button { Task { await viewModel.saveOffline() } } label: {
                        Label("Save Offline", systemImage: "arrow.down.circle")
                    }
                    .buttonStyle(SecondaryButtonStyle())
                } else {
                    Label("Available offline", systemImage: "checkmark.circle.fill")
                        .font(Theme.Typography.footnote)
                        .foregroundColor(Theme.Palette.success)
                }
            }
            .padding(.horizontal, Theme.Spacing.xl)
        }
        .padding(Theme.Spacing.xl)
        .frame(maxWidth: .infinity, maxHeight: .infinity)
    }
}

/// Renders a PDF inline using PDFKit.
struct PDFKitView: UIViewRepresentable {
    let url: URL

    func makeUIView(context: Context) -> PDFView {
        let view = PDFView()
        view.autoScales = true
        view.document = PDFDocument(url: url)
        return view
    }

    func updateUIView(_ uiView: PDFView, context: Context) {
        if uiView.document?.documentURL != url {
            uiView.document = PDFDocument(url: url)
        }
    }
}
