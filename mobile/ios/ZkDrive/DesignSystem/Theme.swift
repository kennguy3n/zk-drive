import SwiftUI

/// The app-wide visual language for ZK Drive. Centralising colour,
/// spacing, radius and typography here keeps every screen consistent and
/// makes a future rebrand a one-file change rather than a sweep through
/// dozens of views.
enum Theme {
    // MARK: Colours
    //
    // The brand palette is defined in Assets.xcassets so it adapts to
    // light/dark automatically; these accessors give type-safe call
    // sites instead of stringly-typed `Color("BrandPrimary")` scattered
    // around the codebase.
    enum Palette {
        static let brand = Color("BrandPrimary")
        static let brandSecondary = Color("BrandSecondary")
        static let background = Color("BackgroundPrimary")

        static let surface = Color(.secondarySystemBackground)
        static let surfaceElevated = Color(.tertiarySystemBackground)
        static let separator = Color(.separator)

        static let textPrimary = Color(.label)
        static let textSecondary = Color(.secondaryLabel)
        static let textTertiary = Color(.tertiaryLabel)

        static let danger = Color(.systemRed)
        static let warning = Color(.systemOrange)
        static let success = Color(.systemGreen)

        /// Brand gradient used on the logomark, FAB and avatars. Defined
        /// explicitly (rather than `Color.gradient`, which is iOS 16+) so
        /// it works on the iOS 15 deployment target.
        static let brandGradient = LinearGradient(
            colors: [brand, brand.opacity(0.82)],
            startPoint: .topLeading,
            endPoint: .bottomTrailing
        )
    }

    // MARK: Spacing — an 8pt soft grid.
    enum Spacing {
        static let xxs: CGFloat = 2
        static let xs: CGFloat = 4
        static let sm: CGFloat = 8
        static let md: CGFloat = 12
        static let lg: CGFloat = 16
        static let xl: CGFloat = 24
        static let xxl: CGFloat = 32
    }

    // MARK: Corner radii.
    enum Radius {
        static let sm: CGFloat = 6
        static let md: CGFloat = 10
        static let lg: CGFloat = 16
        static let pill: CGFloat = 999
    }

    // MARK: Typography — semantic roles mapped onto the system font so
    // the app honours Dynamic Type out of the box.
    enum Typography {
        static let largeTitle = Font.system(.largeTitle, design: .rounded).weight(.bold)
        static let title = Font.system(.title2, design: .rounded).weight(.semibold)
        static let headline = Font.system(.headline, design: .rounded)
        static let body = Font.system(.body)
        static let callout = Font.system(.callout)
        static let footnote = Font.system(.footnote)
        static let caption = Font.system(.caption)
    }
}

/// The user-selectable colour scheme, persisted in `@AppStorage`.
enum AppearancePreference: String, CaseIterable, Identifiable {
    case system
    case light
    case dark

    var id: String { rawValue }

    var label: String {
        switch self {
        case .system: return "System"
        case .light: return "Light"
        case .dark: return "Dark"
        }
    }

    /// The SwiftUI `ColorScheme` to force, or `nil` to follow the system.
    var colorScheme: ColorScheme? {
        switch self {
        case .system: return nil
        case .light: return .light
        case .dark: return .dark
        }
    }
}
