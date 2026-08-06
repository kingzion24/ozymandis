package account

import (
	"context"
	"net/http"
)

// RequestRoles answers what the credential on a request may do.
//
// It exists because the API needs the same answer the dashboard gets from
// roleOf, from a credential the dashboard never sees. Both surfaces resolve an
// owner through identity.Provider, which deliberately reports only *who* the
// caller is — widening Owner with a role would put an authorisation field on a
// seam that a wrapping application implements, and a wrapping application
// filling in its own roles is a wrapping application granting itself authority.
//
// So the role is asked for separately, and asked of the credential rather than
// of the owner id: the id is the answer to a previous question, and re-deriving
// permissions from it would trust a value that has already been through one
// translation.
type RequestRoles struct {
	svc    *Service
	cookie string
}

// RequestRoles returns the role resolver for this install.
func (s *Service) RequestRoles(cookieName string) *RequestRoles {
	if cookieName == "" {
		cookieName = DefaultCookieName
	}
	return &RequestRoles{svc: s, cookie: cookieName}
}

// RoleForRequest returns the role the request's credential holds in ownerID.
//
// Tokens are tried before cookies, in the same order and for the same reason
// identity.First uses: a request carrying both should be judged on the one
// attached deliberately. The two must not disagree, which is why this resolves
// the credential again rather than reading anything the earlier resolve left
// behind — a cached role and a live credential are two facts that drift, and
// the window between them is where a demoted member still writes.
//
// The team is checked, not assumed. A credential proves a role in its own team;
// if the request is scoped to another owner the two are not statements about
// the same thing, and reading one as authority over the other is how a member
// of one team gets admin over another.
func (p *RequestRoles) RoleForRequest(
	ctx context.Context, r *http.Request, ownerID string,
) (Role, error) {
	if raw := BearerToken(r); raw != "" {
		tok, err := p.svc.ResolveAPIToken(ctx, raw)
		if err != nil {
			return "", err
		}
		if tok.OwnerID != ownerID {
			return "", ErrNotAMember
		}
		return tok.Role, nil
	}

	c, err := r.Cookie(p.cookie)
	if err != nil || c.Value == "" {
		return "", ErrSessionInvalid
	}
	sess, err := p.svc.ResolveSession(ctx, c.Value)
	if err != nil {
		return "", err
	}
	if sess.ActiveTeamID != ownerID {
		return "", ErrNotAMember
	}
	return sess.Role, nil
}
