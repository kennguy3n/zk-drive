package sharing

import (
	"context"
	"errors"
	"log"

	"github.com/google/uuid"
)

// Template describes a pre-canned client-room layout. Name is the
// short identifier used in the API ("agency", "legal", …); SubFolders
// are created in order under the room's root folder when the
// template is materialized.
//
// Templates intentionally cover only the folder structure: roles,
// share-link policy, retention, and tagging are out of scope (and
// belong to follow-on Phase 4 work). That keeps the surface area
// stable while we observe how customers actually use the verticals.
type Template struct {
	Name       string
	SubFolders []string
}

// builtinTemplates is the registry of every template the API
// recognises. Order is significant — the slice positions match the
// directory ordering customers see in the UI.
var builtinTemplates = map[string]Template{
	"agency": {
		Name:       "agency",
		SubFolders: []string{"Briefs", "Assets", "Deliverables", "Feedback"},
	},
	"accounting": {
		Name:       "accounting",
		SubFolders: []string{"Tax Documents", "Invoices", "Receipts", "Reports"},
	},
	"legal": {
		Name:       "legal",
		SubFolders: []string{"Contracts", "Discovery", "Correspondence", "Court Filings"},
	},
	"construction": {
		Name:       "construction",
		SubFolders: []string{"Plans", "Permits", "Inspections", "Change Orders"},
	},
	"clinic": {
		Name:       "clinic",
		SubFolders: []string{"Patient Files", "Lab Results", "Referrals", "Billing"},
	},
}

// ErrUnknownTemplate is returned by GetTemplate / CreateFromTemplate
// when name is not a registered template.
var ErrUnknownTemplate = errors.New("sharing: unknown template")

// GetTemplate returns the template registered under name. The error
// is ErrUnknownTemplate when the name is not recognised so callers
// can map directly to a 404 / 400 response.
func GetTemplate(name string) (*Template, error) {
	t, ok := builtinTemplates[name]
	if !ok {
		return nil, ErrUnknownTemplate
	}
	tc := t
	return &tc, nil
}

// ListTemplates returns every registered template. Returned in a
// stable name-sorted order is not promised — callers that care
// (HTTP responses) sort on their own.
func ListTemplates() []Template {
	out := make([]Template, 0, len(builtinTemplates))
	for _, t := range builtinTemplates {
		out = append(out, t)
	}
	return out
}

// CreateFromTemplate provisions a regular client room (via Create)
// then populates the room's folder with the template's sub-folders.
// Sub-folder creation failures are not fatal — the room is already
// usable without them, and surfacing a partial success is friendlier
// than rolling back a possibly-already-shared room. Failed
// sub-folders are logged and skipped; the error return is reserved
// for the underlying Create call.
//
// The first return is the room. The second is the share link
// (matching Create's signature). The third is the list of created
// sub-folder ids in template-defined order — short of len(tpl.SubFolders)
// when one or more individual creates failed.
func (s *ClientRoomService) CreateFromTemplate(ctx context.Context, workspaceID, createdBy uuid.UUID, in ClientRoomInput, templateName string) (*ClientRoom, *ShareLink, []uuid.UUID, error) {
	tpl, err := GetTemplate(templateName)
	if err != nil {
		return nil, nil, nil, err
	}
	room, link, err := s.Create(ctx, workspaceID, createdBy, in)
	if err != nil {
		return nil, nil, nil, err
	}
	subIDs := make([]uuid.UUID, 0, len(tpl.SubFolders))
	for _, name := range tpl.SubFolders {
		fref, ferr := s.folders.Create(ctx, workspaceID, &room.FolderID, name, createdBy)
		if ferr != nil {
			// Log-and-continue: the room itself is already created
			// and shared; failing the whole call would force the
			// caller to clean up an already-public room. Operators
			// can re-create missing sub-folders through the regular
			// folder API.
			log.Printf("sharing.CreateFromTemplate: create sub-folder %q room=%s: %v", name, room.ID, ferr)
			continue
		}
		subIDs = append(subIDs, fref.ID)
	}
	return room, link, subIDs, nil
}
