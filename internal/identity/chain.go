package identity

import (
	"context"
	"net/http"
)

// First resolves a request with the first provider that recognises it.
//
// The engine now has two ways to prove who you are that arrive by different
// routes: a browser sends a session cookie, a CLI sends a bearer token. Both
// answer the same question and both produce the same Owner, so the choice
// between them belongs here rather than in every handler downstream — which is
// the whole point of the seam.
//
// Order is significance, not preference. Providers are tried in the order
// given and the first valid Owner wins, so a request carrying both a cookie and
// a header resolves as the header. That is the right way round: a bearer token
// is explicit and per-request, while a cookie is ambient and attached by the
// browser whether or not the caller meant it. Someone testing an API token from
// a signed-in browser tab should be testing the token.
//
// A provider returning an error is not a failure of the chain — it means "not
// mine", which is exactly what a session provider says about a request with no
// cookie. Only when every provider declines does the chain decline, and the
// reason is deliberately not reported: which credential was absent or malformed
// is information about what would have worked, and the middleware's job is to
// make every rejection look the same.
func First(providers ...Provider) Provider {
	return ProviderFunc(func(ctx context.Context, r *http.Request) (Owner, error) {
		for _, p := range providers {
			if p == nil {
				continue
			}
			if owner, err := p.Resolve(ctx, r); err == nil && owner.Valid() {
				return owner, nil
			}
		}
		return Owner{}, ErrUnauthenticated
	})
}
