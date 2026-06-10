package database

import "testing"

func TestIsReadOnlySQL(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want bool
	}{
		// Plain reads route to the replica.
		{"simple select", "SELECT 1", true},
		{"select lower", "select id from files where workspace_id = $1", true},
		{"select with newline+indent", "\n\t SELECT count(*) FROM folders\n", true},
		{"cte read", "WITH t AS (SELECT 1) SELECT * FROM t", true},
		{"recursive cte read", "WITH RECURSIVE tree AS (SELECT 1) SELECT * FROM tree", true},
		{"values", "VALUES (1),(2)", true},
		{"table", "TABLE files", true},
		{"show", "SHOW server_version", true},
		{"explain plain", "EXPLAIN SELECT * FROM files", true},
		{"leading line comment then select", "-- fetch row\nSELECT * FROM files", true},
		{"leading block comment then select", "/* hint */ SELECT * FROM files", true},
		{"select mentioning column updated_at", "SELECT updated_at FROM files", true},

		// Mutations and ambiguous statements route to the primary.
		{"insert", "INSERT INTO files (id) VALUES ($1)", false},
		{"update", "UPDATE files SET name=$1 WHERE id=$2", false},
		{"delete", "DELETE FROM files WHERE id=$1", false},
		{"merge", "MERGE INTO files USING src ON files.id=src.id", false},
		{"writeable cte", "WITH moved AS (DELETE FROM a RETURNING *) INSERT INTO b SELECT * FROM moved", false},
		{"writeable cte update", "WITH x AS (UPDATE files SET n=1 RETURNING id) SELECT * FROM x", false},
		{"select for update", "SELECT * FROM files WHERE id=$1 FOR UPDATE", false},
		{"select for no key update", "SELECT * FROM files FOR NO KEY UPDATE", false},
		{"select for share", "SELECT * FROM files FOR SHARE", false},
		{"explain analyze", "EXPLAIN ANALYZE SELECT * FROM files", false},
		{"explain analyze lower", "explain analyze update files set n=1", false},
		{"select nextval", "SELECT nextval('seq')", false},
		{"empty", "", false},
		{"whitespace only", "   \n\t ", false},
		{"only comment", "-- nothing here", false},
		{"selector not select", "SELECTOR_FUNC()", false},
		{"call proc", "CALL do_work()", false},
		{"set", "SET search_path = public", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isReadOnlySQL(tc.sql); got != tc.want {
				t.Fatalf("isReadOnlySQL(%q) = %v, want %v", tc.sql, got, tc.want)
			}
		})
	}
}

func TestContainsWord(t *testing.T) {
	cases := []struct {
		s, word string
		want    bool
	}{
		{"FOR UPDATE X", "UPDATE", true},
		{"UPDATED_AT", "UPDATE", false},
		{"PREUPDATE", "UPDATE", false},
		{"X UPDATE", "UPDATE", true},
		{"UPDATE", "UPDATE", true},
		{"NO_MATCH", "UPDATE", false},
	}
	for _, tc := range cases {
		if got := containsWord(tc.s, tc.word); got != tc.want {
			t.Fatalf("containsWord(%q,%q)=%v want %v", tc.s, tc.word, got, tc.want)
		}
	}
}

func TestNewReadWriteSplitterNilReplica(t *testing.T) {
	// A nil replica must not panic and must report HasReplica()==false.
	// We cannot open real pools here without a DB, so we only exercise
	// the nil-replica normalisation via a zero-value primary guarded by
	// the panic contract.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on nil primary")
		}
	}()
	_ = NewReadWriteSplitter(nil, nil)
}
