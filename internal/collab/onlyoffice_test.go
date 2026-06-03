package collab

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// fakeEditorData is an in-memory EditorDataSource for the service
// tests. Each field is set per-case so a test can dictate exactly
// what the drive layer would report.
type fakeEditorData struct {
	file       EditorFile
	fileErr    error
	encMode    string
	encErr     error
	canEdit    bool
	canEditErr error
	canView    bool
	canViewErr error
	url        string
	urlErr     error

	// gotTTL records the TTL the service requested so we can assert
	// the presign window.
	gotTTL time.Duration
}

func (f *fakeEditorData) FileForEditor(_ context.Context, _, _ uuid.UUID) (EditorFile, error) {
	return f.file, f.fileErr
}

func (f *fakeEditorData) EncryptionMode(_ context.Context, _, _ uuid.UUID) (string, error) {
	return f.encMode, f.encErr
}

func (f *fakeEditorData) CanEdit(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	return f.canEdit, f.canEditErr
}

func (f *fakeEditorData) CanView(_ context.Context, _, _, _ uuid.UUID) (bool, error) {
	return f.canView, f.canViewErr
}

func (f *fakeEditorData) PresignedDownloadURL(_ context.Context, _ uuid.UUID, _ string, ttl time.Duration) (string, error) {
	f.gotTTL = ttl
	return f.url, f.urlErr
}

func newTestData() *fakeEditorData {
	return &fakeEditorData{
		file: EditorFile{
			WorkspaceID: uuid.New(),
			FileID:      uuid.New(),
			Name:        "Quarterly Report.docx",
			ObjectKey:   "11111111-1111-1111-1111-111111111111/22222222-2222-2222-2222-222222222222/33333333-3333-3333-3333-333333333333",
		},
		encMode: folder.EncryptionManagedEncrypted,
		canEdit: true,
		canView: true,
		url:     "https://fabric.example/presigned-get",
	}
}

func TestGenerateEditorConfig_EditMode(t *testing.T) {
	data := newTestData()
	svc := NewOnlyOfficeService("https://office.example.com/", "topsecret", "https://drive.example.com", data)

	ws, file, user := uuid.New(), uuid.New(), uuid.New()
	cfg, err := svc.GenerateEditorConfig(context.Background(), ws, file, user, "Ada", ModeEdit)
	if err != nil {
		t.Fatalf("GenerateEditorConfig: %v", err)
	}
	if cfg.DocumentServerURL != "https://office.example.com" {
		t.Errorf("server url not trimmed: %q", cfg.DocumentServerURL)
	}
	if cfg.DocumentType != "word" || cfg.Document.FileType != "docx" {
		t.Errorf("doc type: got %q/%q", cfg.DocumentType, cfg.Document.FileType)
	}
	if cfg.EditorConfig.Mode != ModeEdit || !cfg.Document.Permissions.Edit {
		t.Errorf("expected edit mode, got %q edit=%v", cfg.EditorConfig.Mode, cfg.Document.Permissions.Edit)
	}
	if cfg.Document.URL != data.url {
		t.Errorf("document url: got %q", cfg.Document.URL)
	}
	if data.gotTTL != onlyOfficePresignTTL {
		t.Errorf("presign ttl: got %v want %v", data.gotTTL, onlyOfficePresignTTL)
	}
	wantCb := "https://drive.example.com/api/files/" + file.String() + "/editor-callback?workspace_id=" + ws.String()
	if cfg.EditorConfig.CallbackURL != wantCb {
		t.Errorf("callback url: got %q want %q", cfg.EditorConfig.CallbackURL, wantCb)
	}
	if cfg.EditorConfig.User.ID != user.String() || cfg.EditorConfig.User.Name != "Ada" {
		t.Errorf("user block: %+v", cfg.EditorConfig.User)
	}
	if cfg.Token == "" {
		t.Error("expected signed token")
	}
	// The key must be derived from the object key and contain no path
	// separators.
	if strings.Contains(cfg.Document.Key, "/") || cfg.Document.Key == "" {
		t.Errorf("bad document key: %q", cfg.Document.Key)
	}
}

func TestGenerateEditorConfig_ViewerDowngradedFromEdit(t *testing.T) {
	data := newTestData()
	data.canEdit = false // viewer only
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	cfg, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeEdit)
	if err != nil {
		t.Fatalf("GenerateEditorConfig: %v", err)
	}
	if cfg.EditorConfig.Mode != ModeView || cfg.Document.Permissions.Edit {
		t.Errorf("expected downgrade to view, got %q edit=%v", cfg.EditorConfig.Mode, cfg.Document.Permissions.Edit)
	}
}

func TestGenerateEditorConfig_StrictZKForbidden(t *testing.T) {
	data := newTestData()
	data.encMode = folder.EncryptionStrictZK
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	_, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeEdit)
	if !errors.Is(err, ErrStrictZKForbidden) {
		t.Fatalf("expected ErrStrictZKForbidden, got %v", err)
	}
}

func TestGenerateEditorConfig_NotConfigured(t *testing.T) {
	svc := NewOnlyOfficeService("", "s", "https://drive.example.com", newTestData())
	if svc.Enabled() {
		t.Fatal("service should be disabled with empty URL")
	}
	_, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeView)
	if !errors.Is(err, ErrOnlyOfficeNotConfigured) {
		t.Fatalf("expected ErrOnlyOfficeNotConfigured, got %v", err)
	}
}

func TestGenerateEditorConfig_AccessDenied(t *testing.T) {
	data := newTestData()
	data.canView = false
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	_, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeView)
	if !errors.Is(err, ErrEditorAccessDenied) {
		t.Fatalf("expected ErrEditorAccessDenied, got %v", err)
	}
}

func TestGenerateEditorConfig_NoCurrentVersion(t *testing.T) {
	data := newTestData()
	data.file.ObjectKey = ""
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	_, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeView)
	if !errors.Is(err, ErrNoCurrentVersion) {
		t.Fatalf("expected ErrNoCurrentVersion, got %v", err)
	}
}

func TestGenerateEditorConfig_UnsupportedType(t *testing.T) {
	data := newTestData()
	data.file.Name = "archive.zip"
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	_, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeView)
	if !errors.Is(err, ErrUnsupportedDocumentType) {
		t.Fatalf("expected ErrUnsupportedDocumentType, got %v", err)
	}
}

func TestGenerateEditorConfig_ManagedEncryptedEmptyModeAllowed(t *testing.T) {
	data := newTestData()
	data.encMode = "" // repository returns "" for default mode
	svc := NewOnlyOfficeService("https://office.example.com", "s", "https://drive.example.com", data)

	if _, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeEdit); err != nil {
		t.Fatalf("empty (default) encryption mode should be allowed: %v", err)
	}
}

func TestGenerateEditorConfig_UnsignedWhenNoSecret(t *testing.T) {
	svc := NewOnlyOfficeService("https://office.example.com", "", "https://drive.example.com", newTestData())
	cfg, err := svc.GenerateEditorConfig(context.Background(), uuid.New(), uuid.New(), uuid.New(), "Bob", ModeEdit)
	if err != nil {
		t.Fatalf("GenerateEditorConfig: %v", err)
	}
	if cfg.Token != "" {
		t.Errorf("expected empty token without secret, got %q", cfg.Token)
	}
}

func TestVerifyCallbackToken_RoundTrip(t *testing.T) {
	secret := "callback-secret"
	svc := NewOnlyOfficeService("https://office.example.com", secret, "https://drive.example.com", newTestData())

	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"status": float64(2), "url": "https://office.example.com/cache/out.docx"})
	signed, err := tok.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	claims, err := svc.VerifyCallbackToken(signed)
	if err != nil {
		t.Fatalf("VerifyCallbackToken: %v", err)
	}
	if claims["url"] != "https://office.example.com/cache/out.docx" {
		t.Errorf("claims not round-tripped: %+v", claims)
	}
}

func TestVerifyCallbackToken_RejectsWrongSecret(t *testing.T) {
	svc := NewOnlyOfficeService("https://office.example.com", "right", "https://drive.example.com", newTestData())
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, jwt.MapClaims{"status": float64(2)})
	signed, _ := tok.SignedString([]byte("wrong"))
	if _, err := svc.VerifyCallbackToken(signed); err == nil {
		t.Fatal("expected verification failure for wrong secret")
	}
}

func TestVerifyCallbackToken_DisabledWhenNoSecret(t *testing.T) {
	svc := NewOnlyOfficeService("https://office.example.com", "", "https://drive.example.com", newTestData())
	claims, err := svc.VerifyCallbackToken("anything")
	if err != nil || claims != nil {
		t.Fatalf("expected (nil, nil) when verification disabled, got (%v, %v)", claims, err)
	}
}

func TestIsOfficeDocument(t *testing.T) {
	cases := map[string]bool{
		"a.docx": true, "b.XLSX": true, "c.pptx": true, "d.odt": true,
		"e.txt": true, "f.pdf": false, "g.png": false, "noext": false, "": false,
	}
	for name, want := range cases {
		if got := IsOfficeDocument(name); got != want {
			t.Errorf("IsOfficeDocument(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestDocumentKey_SanitisesAndBounds(t *testing.T) {
	key := documentKey("ws-uuid/file-uuid/version-uuid")
	if strings.ContainsAny(key, "/ ") {
		t.Errorf("key contains illegal chars: %q", key)
	}
	long := strings.Repeat("a/", 200)
	if got := documentKey(long); len(got) > 128 {
		t.Errorf("key not bounded to 128: len=%d", len(got))
	}
}
