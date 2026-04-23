package user

import (
	"time"

	"github.com/google/uuid"
)

// Role values recognized for a user within a workspace.
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

// User represents a workspace-scoped account.
type User struct {
	ID           uuid.UUID `json:"id"`
	WorkspaceID  uuid.UUID `json:"workspace_id"`
	Email        string    `json:"email"`
	Name         string    `json:"name"`
	PasswordHash string    `json:"-"`
	Role         string    `json:"role"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}
