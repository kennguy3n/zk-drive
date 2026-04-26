package integration

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

func TestTagSearchSurfacesTaggedFile(t *testing.T) {
	env := setupEnv(t)
	tok := env.signupAndLogin("Acme", "admin@acme.test", "Alice", "pw")
	fold := createFolder(t, env, tok.Token, nil, "Docs")
	f := createFile(t, env, tok.Token, fold.ID.String(), "untaggedname.txt", "text/plain")

	const tag = "compliance"
	status, body := env.httpRequest(http.MethodPost, "/api/files/"+f.ID.String()+"/tags", tok.Token, map[string]string{
		"tag": tag,
	})
	if status != http.StatusCreated {
		t.Fatalf("add tag: status=%d body=%s", status, string(body))
	}

	status, body = env.httpRequest(http.MethodGet, "/api/search?q="+tag, tok.Token, nil)
	if status != http.StatusOK {
		t.Fatalf("search: status=%d body=%s", status, string(body))
	}
	var resp struct {
		Hits []struct {
			ID   uuid.UUID `json:"id"`
			Type string    `json:"type"`
			Tags []string  `json:"tags"`
		} `json:"hits"`
	}
	env.decodeJSON(body, &resp)

	for _, h := range resp.Hits {
		if h.Type == "file" && h.ID == f.ID {
			for _, ttag := range h.Tags {
				if ttag == tag {
					return
				}
			}
			t.Fatalf("file hit missing %q in tags=%v", tag, h.Tags)
		}
	}
	t.Fatalf("expected tagged file %s to surface in search hits=%+v", f.ID, resp.Hits)
}
