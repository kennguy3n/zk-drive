package integration

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// chainVerifyResult mirrors audit.ChainVerification on the wire.
type chainVerifyResult struct {
	WorkspaceID     uuid.UUID `json:"workspace_id"`
	Valid           bool      `json:"valid"`
	RowsChecked     int64     `json:"rows_checked"`
	HeadSeq         int64     `json:"head_seq"`
	FirstInvalidSeq int64     `json:"first_invalid_seq"`
	Detail          string    `json:"detail"`
}

func verifyAuditChain(t *testing.T, env *testEnv, token string) chainVerifyResult {
	t.Helper()
	status, body := env.httpRequest(http.MethodGet, "/api/admin/audit-log/verify", token, nil)
	if status != http.StatusOK {
		t.Fatalf("verify audit chain: status=%d body=%s", status, string(body))
	}
	var res chainVerifyResult
	env.decodeJSON(body, &res)
	return res
}

// TestAuditChainVerifiesAndDetectsTampering is the end-to-end proof of
// 6.6: real audited actions build a per-workspace HMAC hash chain that
// verifies green, and a direct out-of-band mutation of an audit_log
// row (simulating a malicious DB admin) is detected by the verifier.
func TestAuditChainVerifiesAndDetectsTampering(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")

	// Generate several audited events so the chain has multiple links.
	// signup + each login emit audit.ActionLogin; wait until they land.
	_ = env.login("admin@acme.test", "pw")
	_ = env.login("admin@acme.test", "pw")
	waitForAuditAction(t, env, tok.Token, "")

	// A freshly built chain must verify.
	res := verifyAuditChain(t, env, tok.Token)
	if !res.Valid {
		t.Fatalf("fresh chain not valid: %+v", res)
	}
	if res.RowsChecked == 0 {
		t.Fatalf("expected at least one audited row, got rows_checked=0")
	}
	if res.HeadSeq != res.RowsChecked {
		t.Errorf("head_seq=%d != rows_checked=%d for an unarchived chain", res.HeadSeq, res.RowsChecked)
	}

	wsID := uuid.MustParse(tok.WorkspaceID)

	// Tamper: flip the action on the first chained row without
	// recomputing its HMAC — exactly what a DB admin editing history
	// would do. The pool bypasses RLS (no workspace GUC set), matching
	// the audit writer's own access path.
	tag, err := env.pool.Exec(t.Context(),
		`UPDATE audit_log SET action = action || '.tampered'
		 WHERE workspace_id = $1 AND seq = 1`, wsID)
	if err != nil {
		t.Fatalf("tamper update: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("tamper update affected %d rows, want 1", tag.RowsAffected())
	}

	res = verifyAuditChain(t, env, tok.Token)
	if res.Valid {
		t.Fatal("verifier reported a tampered chain as valid")
	}
	if res.FirstInvalidSeq != 1 {
		t.Errorf("first_invalid_seq=%d, want 1 (the mutated row)", res.FirstInvalidSeq)
	}
	if res.Detail == "" {
		t.Error("expected a human-readable detail for the failed verification")
	}
}

// TestAuditChainDetectsDeletedRow proves a deleted middle row (a gap in
// the chain) is caught: the successor's prev_hash no longer matches the
// new predecessor's entry_hash.
func TestAuditChainDetectsDeletedRow(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	_ = env.login("admin@acme.test", "pw")
	_ = env.login("admin@acme.test", "pw")
	_ = env.login("admin@acme.test", "pw")

	// Wait until the head sequence reaches at least 3 so there is a
	// genuine middle row to remove.
	deadline := time.Now().Add(3 * time.Second)
	var head chainVerifyResult
	for time.Now().Before(deadline) {
		head = verifyAuditChain(t, env, tok.Token)
		if head.HeadSeq >= 3 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if head.HeadSeq < 3 {
		t.Fatalf("expected head_seq>=3, got %d", head.HeadSeq)
	}
	if !head.Valid {
		t.Fatalf("precondition: chain should be valid before deletion: %+v", head)
	}

	wsID := uuid.MustParse(tok.WorkspaceID)
	// Delete a middle row (seq 2): the chain head still records the
	// real head, so verification must flag the broken link at seq 3.
	tag, err := env.pool.Exec(t.Context(),
		`DELETE FROM audit_log WHERE workspace_id = $1 AND seq = 2`, wsID)
	if err != nil {
		t.Fatalf("delete row: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete affected %d rows, want 1", tag.RowsAffected())
	}

	res := verifyAuditChain(t, env, tok.Token)
	if res.Valid {
		t.Fatal("verifier reported a chain with a deleted row as valid")
	}
	if res.FirstInvalidSeq != 3 {
		t.Errorf("first_invalid_seq=%d, want 3 (successor of the deleted row)", res.FirstInvalidSeq)
	}
}
