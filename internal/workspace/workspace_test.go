package workspace

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TestIsSupportedSearchLanguage pins the allow-list contract that
// the admin endpoint relies on for input validation. Adding a new
// dictionary to supportedSearchLanguages requires updating this
// test alongside the migration that documents it.
func TestIsSupportedSearchLanguage(t *testing.T) {
	for _, lang := range []string{
		"simple", "english", "french", "german", "spanish", "italian",
		"portuguese", "dutch", "russian", "swedish", "norwegian",
		"danish", "finnish", "hungarian", "turkish", "romanian",
	} {
		if !IsSupportedSearchLanguage(lang) {
			t.Errorf("expected %q to be supported", lang)
		}
	}
	for _, lang := range []string{"", "klingon", "ENGLISH", "english ", "Simple"} {
		// Case-sensitive — the admin handler lowercases input
		// before calling. Empty / unknown values always fail.
		if IsSupportedSearchLanguage(lang) {
			t.Errorf("expected %q NOT to be supported", lang)
		}
	}
}

// TestSupportedSearchLanguagesReturnsCopy asserts that mutating the
// returned slice cannot poison the package-level allow-list. The
// admin handler echoes the list in responses; if the slice were
// shared, a caller could swap entries and break subsequent
// validation.
func TestSupportedSearchLanguagesReturnsCopy(t *testing.T) {
	a := SupportedSearchLanguages()
	if len(a) == 0 {
		t.Fatalf("expected non-empty allow-list")
	}
	a[0] = "tampered"
	for _, lang := range SupportedSearchLanguages() {
		if lang == "tampered" {
			t.Errorf("mutating the returned slice leaked back into the package state")
		}
	}
}

// TestSupportedSearchLanguagesIsSorted pins the API contract that
// the supported-language list is alphabetically sorted, so JSON
// responses are byte-stable across calls (clients can cache /
// hash / diff them without false churn from Go's randomised map
// iteration order).
func TestSupportedSearchLanguagesIsSorted(t *testing.T) {
	got := SupportedSearchLanguages()
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Errorf("expected sorted output, got %q before %q (index %d)", got[i-1], got[i], i)
		}
	}
}

// fakeRepo lets us unit-test Service without a Postgres connection.
type fakeRepo struct {
	w                  *Workspace
	setLangCalled      bool
	setLangLang        string
	setLangErr         error
	getByIDErr         error
	// getLangCallCount lets tests assert the hot-path optimisation
	// holds: the search handler must call the dedicated
	// GetSearchLanguageByID, NOT the full-row GetByID. We bump
	// this in the dedicated helper and assert GetByID was NOT
	// invoked when only the language was needed.
	getLangCallCount   int
	getByIDCallCount   int
	getLangErr         error
	// Default-encryption-mode plumbing, mirroring the search-language
	// fields above so tests can assert the Set path was reached and
	// inject lookup errors.
	setModeCalled    bool
	setModeMode      string
	setModeErr       error
	getModeCallCount int
	getModeErr       error
}

func (f *fakeRepo) Create(context.Context, *Workspace) error { return nil }
func (f *fakeRepo) CreateTx(context.Context, pgx.Tx, *Workspace) error {
	return nil
}
func (f *fakeRepo) GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	f.getByIDCallCount++
	if f.getByIDErr != nil {
		return nil, f.getByIDErr
	}
	return f.w, nil
}
func (f *fakeRepo) GetSearchLanguageByID(_ context.Context, _ uuid.UUID) (string, error) {
	f.getLangCallCount++
	if f.getLangErr != nil {
		return "", f.getLangErr
	}
	if f.w == nil {
		return "", ErrNotFound
	}
	return f.w.SearchLanguage, nil
}
func (f *fakeRepo) Update(context.Context, *Workspace) error { return nil }
func (f *fakeRepo) ListForUser(context.Context, uuid.UUID) ([]*Workspace, error) {
	return nil, nil
}
func (f *fakeRepo) SetOwner(context.Context, uuid.UUID, uuid.UUID) error { return nil }
func (f *fakeRepo) SetOwnerTx(context.Context, pgx.Tx, uuid.UUID, uuid.UUID) error {
	return nil
}
func (f *fakeRepo) SetMFARequired(context.Context, uuid.UUID, bool) (bool, error) {
	return false, nil
}
func (f *fakeRepo) SetSearchLanguage(_ context.Context, _ uuid.UUID, lang string) (string, error) {
	f.setLangCalled = true
	f.setLangLang = lang
	if f.setLangErr != nil {
		return "", f.setLangErr
	}
	prev := ""
	if f.w != nil {
		prev = f.w.SearchLanguage
		f.w.SearchLanguage = lang
	}
	return prev, nil
}
func (f *fakeRepo) GetDefaultEncryptionModeByID(_ context.Context, _ uuid.UUID) (string, error) {
	f.getModeCallCount++
	if f.getModeErr != nil {
		return "", f.getModeErr
	}
	if f.w == nil {
		return "", ErrNotFound
	}
	return f.w.DefaultEncryptionMode, nil
}
func (f *fakeRepo) SetDefaultEncryptionMode(_ context.Context, _ uuid.UUID, mode string) (string, error) {
	f.setModeCalled = true
	f.setModeMode = mode
	if f.setModeErr != nil {
		return "", f.setModeErr
	}
	prev := ""
	if f.w != nil {
		prev = f.w.DefaultEncryptionMode
		f.w.DefaultEncryptionMode = mode
	}
	return prev, nil
}

// TestServiceSetSearchLanguage_Rejects covers the validation path
// — an unsupported value MUST short-circuit before reaching the
// repository so we can't leak garbage into the database column.
func TestServiceSetSearchLanguage_Rejects(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{SearchLanguage: "simple"}}
	svc := NewService(repo)
	_, err := svc.SetSearchLanguage(context.Background(), uuid.New(), "klingon")
	if !errors.Is(err, ErrUnsupportedSearchLanguage) {
		t.Fatalf("expected ErrUnsupportedSearchLanguage, got %v", err)
	}
	if repo.setLangCalled {
		t.Errorf("expected repo.SetSearchLanguage NOT to be called for invalid input")
	}
}

// TestServiceSetSearchLanguage_Accepts covers the happy path and
// ensures the returned previous value comes from the repository so
// the audit log gets the real prior state.
func TestServiceSetSearchLanguage_Accepts(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{SearchLanguage: "english"}}
	svc := NewService(repo)
	prev, err := svc.SetSearchLanguage(context.Background(), uuid.New(), "french")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prev != "english" {
		t.Errorf("expected prev=english, got %q", prev)
	}
	if !repo.setLangCalled || repo.setLangLang != "french" {
		t.Errorf("expected repo to be called with 'french', got called=%v lang=%q", repo.setLangCalled, repo.setLangLang)
	}
}

// TestServiceGetSearchLanguage_FallbackOnEmpty covers the defence-
// in-depth fallback: if a future migration ever drops NOT NULL and
// a workspace ends up with empty search_language, we still return
// the default rather than passing "" through to the search query
// (which would 500 on to_tsvector('', ...)).
func TestServiceGetSearchLanguage_FallbackOnEmpty(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{SearchLanguage: ""}}
	svc := NewService(repo)
	got, err := svc.GetSearchLanguage(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DefaultSearchLanguage {
		t.Errorf("expected fallback to %q, got %q", DefaultSearchLanguage, got)
	}
}

// TestServiceGetSearchLanguage_UsesHotPathQuery pins the hot-path
// optimisation: the search handler invokes
// GetSearchLanguage on every search request, so the service MUST
// dispatch to the dedicated single-column GetSearchLanguageByID
// helper (a one-column projection) and NOT to GetByID (a ten-
// column full-row read). Pulling the entire workspace row per
// search would waste bandwidth at high QPS / 10K+ workspace
// fleets.
func TestServiceGetSearchLanguage_UsesHotPathQuery(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{SearchLanguage: "english"}}
	svc := NewService(repo)
	got, err := svc.GetSearchLanguage(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "english" {
		t.Errorf("expected 'english', got %q", got)
	}
	if repo.getLangCallCount != 1 {
		t.Errorf("expected GetSearchLanguageByID called once, got %d", repo.getLangCallCount)
	}
	if repo.getByIDCallCount != 0 {
		t.Errorf("regression: GetSearchLanguage took the slow full-row path GetByID (count=%d)", repo.getByIDCallCount)
	}
}

// TestServiceSetDefaultEncryptionMode_Rejects: an unsupported mode
// must short-circuit before reaching the repository so no value
// outside the migration's CHECK constraint can be attempted.
func TestServiceSetDefaultEncryptionMode_Rejects(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{DefaultEncryptionMode: EncryptionManagedEncrypted}}
	svc := NewService(repo)
	_, err := svc.SetDefaultEncryptionMode(context.Background(), uuid.New(), "rot13")
	if !errors.Is(err, ErrUnsupportedEncryptionMode) {
		t.Fatalf("expected ErrUnsupportedEncryptionMode, got %v", err)
	}
	if repo.setModeCalled {
		t.Errorf("expected repo.SetDefaultEncryptionMode NOT to be called for invalid input")
	}
}

// TestServiceSetDefaultEncryptionMode_Accepts covers the happy path
// and ensures the previous value is sourced from the repository for
// the audit log.
func TestServiceSetDefaultEncryptionMode_Accepts(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{DefaultEncryptionMode: EncryptionManagedEncrypted}}
	svc := NewService(repo)
	prev, err := svc.SetDefaultEncryptionMode(context.Background(), uuid.New(), EncryptionStrictZK)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if prev != EncryptionManagedEncrypted {
		t.Errorf("expected prev=%q, got %q", EncryptionManagedEncrypted, prev)
	}
	if !repo.setModeCalled || repo.setModeMode != EncryptionStrictZK {
		t.Errorf("expected repo called with %q, got called=%v mode=%q", EncryptionStrictZK, repo.setModeCalled, repo.setModeMode)
	}
}

// TestServiceGetDefaultEncryptionMode_FallbackOnEmpty covers the
// defence-in-depth fallback to the package default when the column is
// somehow empty, matching GetSearchLanguage's behaviour.
func TestServiceGetDefaultEncryptionMode_FallbackOnEmpty(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{DefaultEncryptionMode: ""}}
	svc := NewService(repo)
	got, err := svc.GetDefaultEncryptionMode(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != DefaultEncryptionMode {
		t.Errorf("expected fallback to %q, got %q", DefaultEncryptionMode, got)
	}
}

// TestServiceGetDefaultEncryptionMode_UsesHotPathQuery pins the same
// hot-path optimisation as the search-language counterpart: the
// folder-create flow hits this per new root folder, so it must use
// the dedicated single-column helper, not the full-row GetByID.
func TestServiceGetDefaultEncryptionMode_UsesHotPathQuery(t *testing.T) {
	repo := &fakeRepo{w: &Workspace{DefaultEncryptionMode: EncryptionStrictZK}}
	svc := NewService(repo)
	got, err := svc.GetDefaultEncryptionMode(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != EncryptionStrictZK {
		t.Errorf("expected %q, got %q", EncryptionStrictZK, got)
	}
	if repo.getModeCallCount != 1 {
		t.Errorf("expected GetDefaultEncryptionModeByID called once, got %d", repo.getModeCallCount)
	}
	if repo.getByIDCallCount != 0 {
		t.Errorf("regression: GetDefaultEncryptionMode took the slow full-row path GetByID (count=%d)", repo.getByIDCallCount)
	}
}
