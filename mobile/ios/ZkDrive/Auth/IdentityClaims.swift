import Foundation

/// A read-only view of the OIDC claims carried in the iam-core access
/// token. Decoded locally for *display only* (account name/email in
/// Settings) — never used for authorization decisions, which always
/// happen server-side.
struct IdentityClaims: Equatable {
    let subject: String?
    let email: String?
    let name: String?
    let orgID: String?
    let tenantID: String?
    let roles: [String]

    var displayName: String {
        name ?? email ?? subject ?? "Signed in"
    }

    var initials: String {
        let source = name ?? email ?? "?"
        let parts = source.split(whereSeparator: { $0 == " " || $0 == "@" || $0 == "." })
        let letters = parts.prefix(2).compactMap { $0.first }
        return String(letters).uppercased()
    }

    /// Decode the payload segment of a JWT without verifying the
    /// signature (display use only). Returns nil if the token is not a
    /// well-formed JWT.
    static func decode(jwt: String) -> IdentityClaims? {
        let segments = jwt.split(separator: ".")
        guard segments.count >= 2 else { return nil }
        guard let payload = base64URLDecode(String(segments[1])),
              let json = try? JSONSerialization.jsonObject(with: payload) as? [String: Any] else {
            return nil
        }
        let roles: [String]
        if let array = json["roles"] as? [String] {
            roles = array
        } else if let single = json["role"] as? String {
            roles = [single]
        } else {
            roles = []
        }
        return IdentityClaims(
            subject: json["sub"] as? String,
            email: json["email"] as? String,
            name: json["name"] as? String,
            orgID: json["org_id"] as? String,
            tenantID: json["tenant_id"] as? String,
            roles: roles
        )
    }

    private static func base64URLDecode(_ value: String) -> Data? {
        var base64 = value.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
        let padding = base64.count % 4
        if padding > 0 { base64 += String(repeating: "=", count: 4 - padding) }
        return Data(base64Encoded: base64)
    }
}
