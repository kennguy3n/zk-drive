import Foundation
import SwiftUI

/// Debounced full-text search with a small recent-query history.
@MainActor
final class SearchViewModel: ObservableObject {
    @Published var query = "" { didSet { scheduleSearch() } }
    @Published private(set) var hits: [SearchHit] = []
    @Published private(set) var isSearching = false
    @Published private(set) var recentSearches: [String] = []
    @Published var error: AppError?

    private let api: DriveAPIClient
    private var debounceTask: Task<Void, Never>?
    private let defaults = UserDefaults.standard
    private static let recentKey = "search.recent"
    private static let maxRecent = 8
    private static let debounce: UInt64 = 300_000_000 // 300ms

    init(api: DriveAPIClient) {
        self.api = api
        recentSearches = defaults.stringArray(forKey: Self.recentKey) ?? []
    }

    private func scheduleSearch() {
        debounceTask?.cancel()
        let term = query.trimmingCharacters(in: .whitespacesAndNewlines)
        guard term.count >= 2 else {
            hits = []
            isSearching = false
            return
        }
        debounceTask = Task { [weak self] in
            try? await Task.sleep(nanoseconds: Self.debounce)
            guard !Task.isCancelled else { return }
            await self?.performSearch(term: term)
        }
    }

    /// Run a search immediately (e.g. when tapping a recent query).
    func searchNow(_ term: String) {
        query = term
        debounceTask?.cancel()
        Task { await performSearch(term: term) }
    }

    private func performSearch(term: String) async {
        isSearching = true
        defer { isSearching = false }
        do {
            hits = try await api.search(query: term)
            error = nil
            recordRecent(term)
        } catch is CancellationError {
            // ignore
        } catch {
            let appError = error.asAppError()
            if appError.category != .network || !Task.isCancelled {
                self.error = appError
            }
        }
    }

    private func recordRecent(_ term: String) {
        var recents = recentSearches.filter { $0.caseInsensitiveCompare(term) != .orderedSame }
        recents.insert(term, at: 0)
        if recents.count > Self.maxRecent { recents = Array(recents.prefix(Self.maxRecent)) }
        recentSearches = recents
        defaults.set(recents, forKey: Self.recentKey)
    }

    func clearRecents() {
        recentSearches = []
        defaults.removeObject(forKey: Self.recentKey)
    }
}
