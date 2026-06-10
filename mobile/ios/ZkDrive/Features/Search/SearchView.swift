import SwiftUI

struct SearchView: View {
    @StateObject var viewModel: SearchViewModel

    var body: some View {
        List {
            if viewModel.query.trimmingCharacters(in: .whitespaces).count < 2 {
                recentSection
            } else if viewModel.isSearching && viewModel.hits.isEmpty {
                HStack { Spacer(); ProgressView(); Spacer() }
            } else if viewModel.hits.isEmpty {
                emptyResults
            } else {
                resultsSection
            }
        }
        .listStyle(.plain)
        .navigationTitle("Search")
        .searchable(text: $viewModel.query, placement: .navigationBarDrawer(displayMode: .always), prompt: "Search files and folders")
        .alert("Search error", isPresented: Binding(get: { viewModel.error != nil }, set: { if !$0 { viewModel.error = nil } })) {
            Button("OK", role: .cancel) {}
        } message: { Text(viewModel.error?.userMessage ?? "") }
    }

    @ViewBuilder
    private var recentSection: some View {
        if viewModel.recentSearches.isEmpty {
            EmptyStateView(systemImage: "magnifyingglass", title: "Search your drive", message: "Find files and folders by name or content.")
                .listRowSeparator(.hidden)
        } else {
            Section {
                ForEach(viewModel.recentSearches, id: \.self) { term in
                    Button { viewModel.searchNow(term) } label: {
                        Label(term, systemImage: "clock.arrow.circlepath").foregroundColor(Theme.Palette.textPrimary)
                    }
                }
            } header: {
                HStack {
                    Text("Recent")
                    Spacer()
                    Button("Clear") { viewModel.clearRecents() }.font(Theme.Typography.caption)
                }
            }
        }
    }

    private var emptyResults: some View {
        EmptyStateView(systemImage: "doc.text.magnifyingglass", title: "No results", message: "No files match “\(viewModel.query)”.")
            .listRowSeparator(.hidden)
    }

    private var resultsSection: some View {
        ForEach(viewModel.hits) { hit in
            HStack(spacing: Theme.Spacing.md) {
                Image(systemName: hit.type == "folder" ? "folder.fill" : FileTypeIcon.symbol(forMime: nil, name: hit.name))
                    .font(.title3)
                    .foregroundColor(hit.type == "folder" ? Theme.Palette.brand : FileTypeIcon.tint(forMime: nil, name: hit.name))
                    .frame(width: 32)
                VStack(alignment: .leading, spacing: 2) {
                    Text(hit.name).font(Theme.Typography.body).lineLimit(1)
                    Text(hit.path).font(Theme.Typography.caption).foregroundColor(Theme.Palette.textTertiary).lineLimit(1)
                }
                Spacer()
            }
            .padding(.vertical, 2)
        }
    }
}
