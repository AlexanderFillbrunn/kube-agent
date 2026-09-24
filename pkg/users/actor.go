package users

type Actor struct {
	UserID   string `json:"user_id"`
	TenantID string `json:"tenant_id"`

	// CalledBy captures impersonation/delegation: if this Actor is acting on
	// behalf of another, CalledBy points to that parent Actor.
	CalledBy *Actor `json:"called_by,omitempty"`
}