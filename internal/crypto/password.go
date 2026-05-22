package crypto

import "golang.org/x/crypto/bcrypt"

// PasswordHashCost is the bcrypt work factor used when hashing user
// passwords and share-link passwords. 12 is the OWASP-recommended
// minimum as of 2025 and roughly 4× slower than bcrypt.DefaultCost
// (10) on modern hardware (~250ms vs ~60ms per hash on a typical
// 2024 server CPU), which materially raises the cost of an offline
// dictionary attack on a leaked credentials dump without making
// interactive login feel sluggish.
//
// When bumping further (cost 13/14), watch out for two things:
//
//  1. Bcrypt is exponential — cost 14 takes ~16× longer than cost 10.
//     At some point password verification starts to dominate login
//     latency budgets.
//  2. Existing password hashes are stored with the cost they were
//     created at, so this only affects newly-hashed passwords. The
//     auth layer should rehash on next successful login when it
//     detects an outdated cost (see VerifyPassword in
//     internal/user/service.go for the rehash-on-login pattern).
const PasswordHashCost = 12

// HashPassword wraps bcrypt.GenerateFromPassword with the package's
// chosen cost so future cost-bump migrations only have to touch this
// file rather than every call site.
func HashPassword(password string) ([]byte, error) {
	return bcrypt.GenerateFromPassword([]byte(password), PasswordHashCost)
}
