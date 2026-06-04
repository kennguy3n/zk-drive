package collab

// onlyoffice.go bridges ZK Drive files to an external ONLYOFFICE
// Document Server for collaborative editing of office documents
// (Word / Excel / PowerPoint and their Open Document equivalents).
//
// The flow is:
//
//  1. The browser asks ZK Drive for an editor config
//     (GET /api/files/{id}/editor-config). OnlyOfficeService.
//     GenerateEditorConfig builds the JSON the ONLYOFFICE JS API
//     (`new DocsAPI.DocEditor(...)`) needs: a time-limited presigned
//     GET URL the Document Server uses to PULL the current bytes, a
//     callbackUrl it POSTs to when the user finishes editing, the
//     document type / file type, the per-version document key, and
//     the resolved edit/view permission.
//  2. The Document Server pulls the file, the user edits, and on save
//     the Document Server POSTs the callback (handled in
//     api/drive/onlyoffice_handler.go) which downloads the edited
//     bytes and writes them back as a new file version.
//
// Because the Document Server must READ and WRITE the plaintext file,
// this only works for managed_encrypted folders. strict_zk folders
// keep their bytes opaque to the server, so GenerateEditorConfig
// refuses them with ErrStrictZKForbidden.

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/internal/folder"
)

// onlyOfficePresignTTL is the validity window for the presigned GET
// URL embedded in the editor config. The Document Server fetches the
// document immediately on editor open, so a short window is enough
// while keeping the signed URL from lingering in logs / history.
const onlyOfficePresignTTL = 15 * time.Minute

// Editor modes. ONLYOFFICE's editorConfig.mode is "edit" or "view";
// the document.permissions.edit flag mirrors it.
const (
	ModeEdit = "edit"
	ModeView = "view"
)

var (
	// ErrOnlyOfficeNotConfigured is returned when no Document Server
	// URL is configured (ONLYOFFICE_URL empty). Callers surface this
	// as a 503 / feature-disabled so the frontend hides the editor.
	ErrOnlyOfficeNotConfigured = errors.New("collab: onlyoffice not configured")
	// ErrStrictZKForbidden is returned when the file lives in a
	// strict_zk folder. The Document Server would need plaintext
	// access, which strict-ZK deliberately denies.
	ErrStrictZKForbidden = errors.New("collab: onlyoffice editing forbidden for strict_zk folder")
	// ErrWorkspaceSuspended is returned when a save callback targets a
	// workspace the platform control plane has suspended. The callback
	// path runs outside the session-auth middleware (the Document
	// Server holds no ZK Drive JWT) and therefore outside
	// SuspensionGuard, so the save path re-checks suspension at the
	// write boundary to keep a suspended workspace frozen against
	// callback-driven version writes, consistent with the 503 every
	// REST write already returns.
	ErrWorkspaceSuspended = errors.New("collab: workspace suspended")
	// ErrEditorAccessDenied is returned when the user lacks even
	// viewer access to the file.
	ErrEditorAccessDenied = errors.New("collab: insufficient permission to open editor")
	// ErrNoCurrentVersion is returned when the file has no confirmed
	// version yet — there is nothing to open.
	ErrNoCurrentVersion = errors.New("collab: file has no current version to edit")
	// ErrUnsupportedDocumentType is returned when the file's name /
	// extension does not map to an ONLYOFFICE document type.
	ErrUnsupportedDocumentType = errors.New("collab: unsupported document type for onlyoffice")
	// ErrCallbackURLNotAllowed is returned when a save callback's
	// edited-document URL does not share the configured Document
	// Server origin. The callback path fetches this URL server-side,
	// so an attacker-controlled value (possible in the unsigned
	// insecure mode) would otherwise be an SSRF vector.
	ErrCallbackURLNotAllowed = errors.New("collab: callback document url not allowed")
)

// EditorFile is the minimal projection of a file the OnlyOffice
// service needs from the drive layer.
type EditorFile struct {
	WorkspaceID uuid.UUID
	FileID      uuid.UUID
	Name        string
	// ObjectKey is the storage key of the file's CURRENT version.
	// Empty when the file has no confirmed version (the service
	// rejects with ErrNoCurrentVersion).
	ObjectKey string
}

// EditorDataSource is the contract the OnlyOffice service needs from
// the drive layer to assemble a config. It is implemented by an
// adapter in api/drive over the file / folder / permission / storage
// services so internal/collab stays free of HTTP and concrete service
// dependencies (and so the orchestration is unit-testable with fakes).
type EditorDataSource interface {
	// FileForEditor returns the file + current-version object key,
	// scoped to the workspace.
	FileForEditor(ctx context.Context, workspaceID, fileID uuid.UUID) (EditorFile, error)
	// EncryptionMode returns the encryption mode of the folder owning
	// the file (folder.EncryptionManagedEncrypted / EncryptionStrictZK).
	// An empty string is treated as the managed-encrypted default.
	EncryptionMode(ctx context.Context, workspaceID, fileID uuid.UUID) (string, error)
	// CanEdit reports whether userID holds editor (or higher) access
	// on the file; CanView reports viewer (or higher).
	CanEdit(ctx context.Context, workspaceID, fileID, userID uuid.UUID) (bool, error)
	CanView(ctx context.Context, workspaceID, fileID, userID uuid.UUID) (bool, error)
	// PresignedDownloadURL returns a time-limited GET URL the
	// Document Server uses to pull the current bytes.
	PresignedDownloadURL(ctx context.Context, workspaceID uuid.UUID, objectKey string, ttl time.Duration) (string, error)
}

// OnlyOfficeService generates ONLYOFFICE Document Server editor
// configs and signs them with the shared JWT secret. A zero-value /
// nil service is not usable; construct via NewOnlyOfficeService.
type OnlyOfficeService struct {
	// serverURL is the Document Server base URL (ONLYOFFICE_URL),
	// echoed to the frontend so it can load the matching api.js.
	serverURL string
	// jwtSecret signs the config and verifies inbound callbacks.
	// Empty disables signing (local dev only).
	jwtSecret string
	// callbackBaseURL is the externally reachable base URL of ZK
	// Drive (cfg.PublicURL) used to compose the absolute callbackUrl
	// the Document Server POSTs to.
	callbackBaseURL string
	data            EditorDataSource
	// now is injected for deterministic tests; defaults to time.Now.
	now func() time.Time
}

// NewOnlyOfficeService builds a service. serverURL empty means the
// feature is disabled — GenerateEditorConfig then returns
// ErrOnlyOfficeNotConfigured and Enabled reports false.
func NewOnlyOfficeService(serverURL, jwtSecret, callbackBaseURL string, data EditorDataSource) *OnlyOfficeService {
	return &OnlyOfficeService{
		serverURL:       strings.TrimRight(strings.TrimSpace(serverURL), "/"),
		jwtSecret:       jwtSecret,
		callbackBaseURL: strings.TrimRight(strings.TrimSpace(callbackBaseURL), "/"),
		data:            data,
		now:             time.Now,
	}
}

// Enabled reports whether a Document Server URL is configured. When
// false, callers should treat office editing as unavailable.
func (s *OnlyOfficeService) Enabled() bool {
	return s != nil && s.serverURL != ""
}

// JWTSecret exposes the configured callback-verification secret so
// the HTTP callback handler can verify inbound Document Server
// tokens. Empty means verification is disabled (local dev).
func (s *OnlyOfficeService) JWTSecret() string {
	if s == nil {
		return ""
	}
	return s.jwtSecret
}

// ValidateDocumentURL guards the save-callback fetch against SSRF. The
// edited bytes are always served from the Document Server's own cache,
// so the callback's url must share the configured Document Server's
// scheme and host. This matters most in the insecure no-secret mode,
// where the callback body (and therefore the url) is unsigned and an
// attacker could otherwise steer the server into fetching an arbitrary
// internal/external address. Same-origin is enforced rather than a
// private-range block because the Document Server is routinely
// co-located on a private network, where a blanket private-IP rejection
// would break legitimate deployments.
func (s *OnlyOfficeService) ValidateDocumentURL(raw string) error {
	if !s.Enabled() {
		return ErrOnlyOfficeNotConfigured
	}
	srv, err := url.Parse(s.serverURL)
	if err != nil || srv.Hostname() == "" {
		// A misconfigured server URL means we cannot establish the
		// trusted origin, so fail closed rather than fetch blindly.
		return ErrCallbackURLNotAllowed
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ErrCallbackURLNotAllowed
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ErrCallbackURLNotAllowed
	}
	if !strings.EqualFold(u.Hostname(), srv.Hostname()) {
		return ErrCallbackURLNotAllowed
	}
	// Enforce the scheme too. Without this, http://office.example.com:443
	// would pass against an https Document Server (identical effective
	// port), letting a callback downgrade the fetch to cleartext or steer
	// it at a co-located plaintext service. The doc comment's same-origin
	// contract is host + scheme + port.
	if !strings.EqualFold(u.Scheme, srv.Scheme) {
		return ErrCallbackURLNotAllowed
	}
	// Match the port too, not just the host: a same-hostname URL on a
	// different port (e.g. office.example.com:9200 Elasticsearch,
	// :6379 Redis) would otherwise pass and let the callback reach a
	// co-located service. Compare effective ports so an explicit
	// default (":443"/":80") still matches an omitted one.
	if effectivePort(u) != effectivePort(srv) {
		return ErrCallbackURLNotAllowed
	}
	return nil
}

// effectivePort returns the URL's port, defaulting to the well-known
// port for its scheme when none is explicit so that
// "https://h" and "https://h:443" compare equal.
func effectivePort(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// EditorConfig is the payload returned to the browser. DocumentServerURL
// tells the frontend which Document Server api.js to load; the nested
// fields mirror the ONLYOFFICE DocEditor config object. Token is the
// HS256 JWT over {documentType, document, editorConfig} that the
// Document Server validates when JWT is enabled.
type EditorConfig struct {
	DocumentServerURL string         `json:"documentServerUrl"`
	DocumentType      string         `json:"documentType"`
	Document          EditorDocument `json:"document"`
	EditorConfig      EditorSettings `json:"editorConfig"`
	Token             string         `json:"token,omitempty"`
}

// EditorDocument is the ONLYOFFICE `document` block.
type EditorDocument struct {
	Title       string            `json:"title"`
	URL         string            `json:"url"`
	FileType    string            `json:"fileType"`
	Key         string            `json:"key"`
	Permissions EditorPermissions `json:"permissions"`
}

// EditorPermissions is the ONLYOFFICE `document.permissions` block.
type EditorPermissions struct {
	Edit     bool `json:"edit"`
	Download bool `json:"download"`
	Print    bool `json:"print"`
}

// EditorSettings is the ONLYOFFICE `editorConfig` block.
type EditorSettings struct {
	Mode        string     `json:"mode"`
	CallbackURL string     `json:"callbackUrl"`
	Lang        string     `json:"lang,omitempty"`
	User        EditorUser `json:"user"`
}

// EditorUser identifies the editing user to the Document Server so
// co-editing presence shows real names.
type EditorUser struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// GenerateEditorConfig builds (and signs) the ONLYOFFICE editor config
// for fileID on behalf of userID. mode is the REQUESTED mode ("edit"
// or "view"); the effective mode is downgraded to "view" when the user
// lacks editor access. userName labels the editing user in co-editing
// presence.
//
// Errors: ErrOnlyOfficeNotConfigured (no server URL),
// ErrEditorAccessDenied (no viewer access), ErrStrictZKForbidden
// (strict-ZK folder), ErrNoCurrentVersion (nothing to open), or
// ErrUnsupportedDocumentType (extension not an office type).
func (s *OnlyOfficeService) GenerateEditorConfig(
	ctx context.Context,
	workspaceID, fileID, userID uuid.UUID,
	userName, mode string,
) (*EditorConfig, error) {
	if !s.Enabled() {
		return nil, ErrOnlyOfficeNotConfigured
	}

	// Viewer access is the floor for opening the editor at all.
	canView, err := s.data.CanView(ctx, workspaceID, fileID, userID)
	if err != nil {
		return nil, err
	}
	if !canView {
		return nil, ErrEditorAccessDenied
	}

	// The Document Server reads (and, in edit mode, writes) plaintext,
	// so strict-ZK folders are off-limits. An empty mode string from
	// the repository is the managed-encrypted default and is allowed.
	encMode, err := s.data.EncryptionMode(ctx, workspaceID, fileID)
	if err != nil {
		return nil, err
	}
	if encMode == folder.EncryptionStrictZK {
		return nil, ErrStrictZKForbidden
	}

	f, err := s.data.FileForEditor(ctx, workspaceID, fileID)
	if err != nil {
		return nil, err
	}
	if f.ObjectKey == "" {
		return nil, ErrNoCurrentVersion
	}

	docType, fileType, ok := documentTypeForName(f.Name)
	if !ok {
		return nil, ErrUnsupportedDocumentType
	}

	// Resolve the effective mode: an "edit" request is only honoured
	// when the user actually holds editor access; otherwise it is
	// downgraded to read-only. A "view" request is always view.
	effectiveMode := ModeView
	if mode == ModeEdit {
		canEdit, cErr := s.data.CanEdit(ctx, workspaceID, fileID, userID)
		if cErr != nil {
			return nil, cErr
		}
		if canEdit {
			effectiveMode = ModeEdit
		}
	}

	url, err := s.data.PresignedDownloadURL(ctx, workspaceID, f.ObjectKey, onlyOfficePresignTTL)
	if err != nil {
		return nil, err
	}

	cfg := &EditorConfig{
		DocumentServerURL: s.serverURL,
		DocumentType:      docType,
		Document: EditorDocument{
			Title:    f.Name,
			URL:      url,
			FileType: fileType,
			// The key MUST change whenever the document content
			// changes, else the Document Server serves stale cached
			// state. The object key embeds the version UUID, so a
			// new version yields a new key for free.
			Key: documentKey(f.ObjectKey),
			Permissions: EditorPermissions{
				Edit:     effectiveMode == ModeEdit,
				Download: true,
				Print:    true,
			},
		},
		EditorConfig: EditorSettings{
			Mode:        effectiveMode,
			CallbackURL: s.callbackURL(workspaceID, fileID),
			User: EditorUser{
				ID:   userID.String(),
				Name: userName,
			},
		},
	}

	token, err := s.signConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg.Token = token
	return cfg, nil
}

// callbackURL composes the absolute URL the Document Server POSTs to
// when the edited document is ready to save. The workspace id is
// carried as a query param because the callback arrives without a
// session JWT (it is authenticated by the ONLYOFFICE-signed body
// token), so the handler needs the tenant scope to resolve the file.
func (s *OnlyOfficeService) callbackURL(workspaceID, fileID uuid.UUID) string {
	return s.callbackBaseURL + "/api/files/" + fileID.String() +
		"/editor-callback?workspace_id=" + workspaceID.String()
}

// signConfig produces the HS256 JWT the Document Server validates.
// Returns an empty token (no error) when no secret is configured so
// local dev works against a JWT-disabled Document Server.
func (s *OnlyOfficeService) signConfig(cfg *EditorConfig) (string, error) {
	if s.jwtSecret == "" {
		return "", nil
	}
	// Sign only the fields the Document Server validates against the
	// payload it receives — documentType, document, editorConfig — plus
	// an exp tied to the embedded presigned URL's lifetime. The token is
	// consumed once at editor open (well within the window); bounding it
	// keeps a leaked config from being replayed after its download URL
	// has already expired.
	claims := jwt.MapClaims{
		"documentType": cfg.DocumentType,
		"document":     cfg.Document,
		"editorConfig": cfg.EditorConfig,
		"exp":          s.now().Add(onlyOfficePresignTTL).Unix(),
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString([]byte(s.jwtSecret))
}

// VerifyCallbackToken parses and validates an inbound Document Server
// callback token, returning its claims. When no secret is configured
// it returns nil claims and a nil error (verification disabled).
func (s *OnlyOfficeService) VerifyCallbackToken(token string) (jwt.MapClaims, error) {
	if s.jwtSecret == "" {
		return nil, nil
	}
	claims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(token, claims, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, errors.New("collab: unexpected onlyoffice token signing method")
		}
		return []byte(s.jwtSecret), nil
	})
	if err != nil {
		return nil, err
	}
	return claims, nil
}

// CallbackPayload is the authenticated subset of an ONLYOFFICE save
// callback the drive layer acts on. When JWT verification is enabled
// every field is sourced from the verified token claims (never the
// unsigned request body), so a spoofed body cannot influence which URL
// the server fetches or whom the save is attributed to.
type CallbackPayload struct {
	Status int
	URL    string
	Key    string
	Users  []string
}

// VerifyCallback verifies an inbound Document Server callback token and
// returns the authenticated payload built ENTIRELY from the signed
// claims. A nil error with a nil payload means verification is disabled
// (no secret configured); callers gate on JWTSecret() before relying on
// this. A non-nil error means the token failed verification and the
// callback must be rejected.
func (s *OnlyOfficeService) VerifyCallback(token string) (*CallbackPayload, error) {
	claims, err := s.VerifyCallbackToken(token)
	if err != nil {
		return nil, err
	}
	if claims == nil {
		return nil, nil
	}
	return callbackPayloadFromClaims(claims), nil
}

// callbackPayloadFromClaims projects verified JWT claims into a
// CallbackPayload. Missing / mistyped claims yield zero values, which
// the caller treats as a non-actionable callback rather than trusting
// any unsigned fallback.
//
// ONLYOFFICE places the callback fields at the top level when the token
// is delivered in the request body (the default), but nests them under
// a "payload" object when the token arrives via the Authorization
// header (JWT_HEADER mode). Both layouts are handled so the verified
// claims stay authoritative regardless of the Document Server's token
// transport.
func callbackPayloadFromClaims(claims jwt.MapClaims) *CallbackPayload {
	src := map[string]any(claims)
	if nested, ok := claims["payload"].(map[string]any); ok {
		src = nested
	}
	p := &CallbackPayload{}
	if status, ok := src["status"].(float64); ok {
		p.Status = int(status)
	}
	if url, ok := src["url"].(string); ok {
		p.URL = url
	}
	if key, ok := src["key"].(string); ok {
		p.Key = key
	}
	p.Users = stringsFromClaim(src["users"])
	return p
}

// stringsFromClaim coerces a JWT claim that should be an array of
// strings (e.g. the ONLYOFFICE "users" list) into []string, ignoring
// any non-string entries.
func stringsFromClaim(v any) []string {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, e := range raw {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// documentKey derives the ONLYOFFICE document key from a version's
// object key. The key must be unique per content revision and limited
// to [0-9A-Za-z._=-] (max 128 chars). Object keys are
// "<workspace>/<file>/<version>" UUID triples, so replacing the path
// separators yields a stable, collision-free, ~110-char key that
// rotates whenever a new version (new key) is confirmed.
func documentKey(objectKey string) string {
	var b strings.Builder
	b.Grow(len(objectKey))
	for _, r := range objectKey {
		switch {
		case r >= '0' && r <= '9',
			r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r == '.', r == '_', r == '=', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	key := b.String()
	if len(key) > 128 {
		key = key[len(key)-128:]
	}
	return key
}

// officeDocType pairs an ONLYOFFICE documentType ("word" / "cell" /
// "slide") with the canonical fileType (extension without the dot).
type officeDocType struct {
	docType  string
	fileType string
}

// officeExtensions maps file extensions the Document Server can open
// to their document type. Covers the OOXML + legacy binary + Open
// Document formats ONLYOFFICE supports for editing.
var officeExtensions = map[string]officeDocType{
	// Word processing.
	"doc":  {"word", "doc"},
	"docx": {"word", "docx"},
	"odt":  {"word", "odt"},
	"rtf":  {"word", "rtf"},
	"txt":  {"word", "txt"},
	// Spreadsheets.
	"xls":  {"cell", "xls"},
	"xlsx": {"cell", "xlsx"},
	"ods":  {"cell", "ods"},
	"csv":  {"cell", "csv"},
	// Presentations.
	"ppt":  {"slide", "ppt"},
	"pptx": {"slide", "pptx"},
	"odp":  {"slide", "odp"},
}

// documentTypeForName resolves the ONLYOFFICE document type + file
// type from a filename's extension. The ok result is false for
// unsupported / extensionless names.
func documentTypeForName(name string) (docType, fileType string, ok bool) {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	if ext == "" {
		return "", "", false
	}
	dt, found := officeExtensions[ext]
	if !found {
		return "", "", false
	}
	return dt.docType, dt.fileType, true
}

// IsOfficeDocument reports whether name has an extension ONLYOFFICE
// can open. Exposed so callers (e.g. the features endpoint / handlers)
// can gate the editor affordance without duplicating the extension
// table.
func IsOfficeDocument(name string) bool {
	_, _, ok := documentTypeForName(name)
	return ok
}
