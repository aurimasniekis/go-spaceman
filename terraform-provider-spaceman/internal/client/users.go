package client

import (
	"context"
	"net/http"
	"net/url"
)

// ---- users, grants and linked identities ----

// Grant is a role in a domain held by a user. Source is "manual" (set by administrators) or "sso" (from OIDC group
// mappings, replaced at each SSO sign-in).
type Grant struct {
	Role   string `json:"role"`
	Domain string `json:"domain"`
	Source string `json:"source,omitempty"`
}

// GrantInput is one grant of PUT /users/{id}/grants.
type GrantInput struct {
	Role   string `json:"role"`
	Domain string `json:"domain"`
}

// User is a SpaceMan user (GET /users/{id}).
type User struct {
	ID             string  `json:"id"`
	Email          string  `json:"email,omitempty"`
	DisplayName    string  `json:"display_name"`
	Enabled        bool    `json:"enabled"`
	ProvisionedVia string  `json:"provisioned_via"`
	Grants         []Grant `json:"grants"`
	SSOGrants      []Grant `json:"sso_grants"`
	Version        int64   `json:"version"`
}

// ManualGrants are the grants administrators set (the set PUT /users/{id}/grants replaces).
func (u *User) ManualGrants() []GrantInput {
	out := []GrantInput{}
	for _, g := range u.Grants {
		if g.Source == "" || g.Source == "manual" {
			out = append(out, GrantInput{Role: g.Role, Domain: g.Domain})
		}
	}
	return out
}

// CreateUserInput is POST /users. Passwords are never sent by the provider (users set them with a reset token or
// sign in through OIDC).
type CreateUserInput struct {
	Email       string       `json:"email,omitempty"`
	DisplayName string       `json:"display_name"`
	Grants      []GrantInput `json:"grants"`
}

// ListUsers is GET /users.
func (c *Client) ListUsers(ctx context.Context) ([]*User, error) {
	var out items[*User]
	return out.Items, c.get(ctx, "/users", &out)
}

// GetUser is GET /users/{id}.
func (c *Client) GetUser(ctx context.Context, id string) (*User, error) {
	var out User
	return &out, c.get(ctx, "/users/"+esc(id), &out)
}

// CreateUser is POST /users (with an Idempotency-Key).
func (c *Client) CreateUser(ctx context.Context, in CreateUserInput) (*User, error) {
	if in.Grants == nil {
		in.Grants = []GrantInput{}
	}
	var out User
	return &out, c.create(ctx, "/users", in, &out)
}

// SetUserEnabled is POST /users/{id}/enable or /disable (If-Match required).
func (c *Client) SetUserEnabled(ctx context.Context, id string, version int64, enabled bool) (*User, error) {
	op := "/disable"
	if enabled {
		op = "/enable"
	}
	var out User
	return &out, c.post(ctx, "/users/"+esc(id)+op, version, nil, &out)
}

// SetUserGrants is PUT /users/{id}/grants: it replaces the user's manual grants (If-Match required).
func (c *Client) SetUserGrants(ctx context.Context, id string, version int64, grants []GrantInput) (*User, error) {
	if grants == nil {
		grants = []GrantInput{}
	}
	var out User
	return &out, c.put(ctx, "/users/"+esc(id)+"/grants", version, map[string]any{"grants": grants}, &out)
}

// Identity is a sign-in identity linked to a user.
type Identity struct {
	Issuer     string `json:"issuer"`
	Subject    string `json:"subject"`
	ProviderID string `json:"provider_id"`
	UserID     string `json:"user_id"`
	LinkedVia  string `json:"linked_via"`
}

// ListUserIdentities is GET /users/{id}/identities.
func (c *Client) ListUserIdentities(ctx context.Context, userID string) ([]Identity, error) {
	var out items[Identity]
	return out.Items, c.get(ctx, "/users/"+esc(userID)+"/identities", &out)
}

// GetUserIdentity finds a linked identity of a user by provider and subject.
func (c *Client) GetUserIdentity(ctx context.Context, userID, providerID, subject string) (*Identity, error) {
	l, err := c.ListUserIdentities(ctx, userID)
	if err != nil {
		return nil, err
	}
	for i := range l {
		if l[i].ProviderID == providerID && l[i].Subject == subject {
			return &l[i], nil
		}
	}
	return nil, notFound("GET /users/{user_id}/identities", "identity")
}

// LinkUserIdentity is POST /users/{id}/identities (linking the same identity to the same user again is a no-op).
func (c *Client) LinkUserIdentity(ctx context.Context, userID, providerID, subject string) error {
	return c.do2(ctx, http.MethodPost, "/users/"+esc(userID)+"/identities", 0, map[string]string{"provider_id": providerID, "subject": subject}, nil)
}

// UnlinkUserIdentity is DELETE /users/{id}/identities?issuer=…&subject=….
func (c *Client) UnlinkUserIdentity(ctx context.Context, userID, issuer, subject string) error {
	_, err := c.do(ctx, request{method: http.MethodDelete, path: "/users/" + esc(userID) + "/identities",
		query: url.Values{"issuer": {issuer}, "subject": {subject}}}, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}
