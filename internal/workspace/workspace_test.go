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

// fakeRepo lets us unit-test Service without a Postgres connection.
type fakeRepo struct {
	w                  *Workspace
	setLangCalled      bool
	setLangLang        string
	setLangErr         error
	getByIDErr         error
}

func (f *fakeRepo) Create(context.Context, *Workspace) error { return nil }
func (f *fakeRepo) CreateTx(context.Context, pgx.Tx, *Workspace) error {
	return nil
}
func (f *fakeRepo) GetByID(ctx context.Context, id uuid.UUID) (*Workspace, error) {
	if f.getByIDErr != nil {
		return nil, f.getByIDErr
	}
	return f.w, nil
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
