// Package scan provides async ClamAV virus scanning for uploaded file
// versions. The service talks to clamd over its TCP socket using the
// INSTREAM protocol, which accepts arbitrary byte streams and returns
// a one-line verdict per object.
//
// Design notes:
//   - clamd is an external service (typically a docker-compose sidecar
//     or a system daemon). Address is configured via CLAMAV_ADDRESS.
//   - When no address is configured the service runs in a "permissive"
//     mode that marks every version clean without calling out. Local
//     development and CI use this to keep the pipeline green without
//     requiring clamav in every environment.
//   - A quarantine verdict updates file_versions.scan_status to
//     'quarantined', stores the signature name in scan_detail, and
//     fires a notification.scan_quarantined event to workspace
//     admins via the notification service.
//   - ClamAV is GPL-2.0. We invoke it as an external daemon (network
//     socket), not as linked code, so it falls under the "mere
//     aggregation" exception. No AGPL code enters the binary.
package scan

import "time"

// Status enum values mirror migration 008's CHECK constraint on
// file_versions.scan_status. Exposing them as typed constants keeps
// the string set discoverable from Go.
const (
	StatusPending     = "pending"
	StatusScanning    = "scanning"
	StatusClean       = "clean"
	StatusQuarantined = "quarantined"
)

// Verdict captures the result of a single clamd scan.
type Verdict struct {
	Status    string    // one of the Status* constants
	Detail    string    // signature name (quarantined) or error message
	ScannedAt time.Time // wall-clock timestamp of the scan result
}

// DefaultAddress is the clamd TCP endpoint used when CLAMAV_ADDRESS is
// not explicitly configured.
const DefaultAddress = "localhost:3310"

// MaxScanBytes caps the number of bytes the INSTREAM protocol will
// hand to clamd in a single session. clamd's StreamMaxLength defaults
// to 25 MB; we pick 100 MB to cover larger uploads while still
// bounding memory use on the worker.
const MaxScanBytes = 100 * 1024 * 1024
