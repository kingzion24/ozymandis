package identity

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// named returns a provider that resolves to owner when the request carries the
// header it is named for, and declines otherwise.
func named(header, owner string) Provider {
	return ProviderFunc(func(_ context.Context, r *http.Request) (Owner, error) {
		if r.Header.Get(header) == "" {
			return Owner{}, ErrUnauthenticated
		}
		return Owner{ID: owner}, nil
	})
}

func TestFirstUsesTheFirstProviderThatRecognisesTheRequest(t *testing.T) {
	chain := First(named("X-Token", "by-token"), named("X-Cookie", "by-cookie"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Cookie", "yes")

	owner, err := chain.Resolve(context.Background(), r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if owner.ID != "by-cookie" {
		t.Errorf("owner = %q, want by-cookie", owner.ID)
	}
}

// Order is significance. A request carrying both should be judged on the
// credential the caller attached deliberately, not the one the browser did.
func TestFirstPrefersTheEarlierProvider(t *testing.T) {
	chain := First(named("X-Token", "by-token"), named("X-Cookie", "by-cookie"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Token", "yes")
	r.Header.Set("X-Cookie", "yes")

	owner, err := chain.Resolve(context.Background(), r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if owner.ID != "by-token" {
		t.Errorf("owner = %q, want by-token — a bearer token is explicit and "+
			"per-request; a cookie is ambient", owner.ID)
	}
}

func TestFirstDeclinesWhenEveryProviderDoes(t *testing.T) {
	chain := First(named("X-Token", "by-token"), named("X-Cookie", "by-cookie"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := chain.Resolve(context.Background(), r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// A provider that errors means "not mine", not "stop". A session provider says
// exactly that about every request with no cookie, so an early error must not
// prevent a later provider from answering.
func TestFirstKeepsGoingPastAnError(t *testing.T) {
	boom := ProviderFunc(func(context.Context, *http.Request) (Owner, error) {
		return Owner{}, errors.New("database is on fire")
	})
	chain := First(boom, named("X-Cookie", "by-cookie"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Cookie", "yes")

	owner, err := chain.Resolve(context.Background(), r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if owner.ID != "by-cookie" {
		t.Errorf("owner = %q, want by-cookie", owner.ID)
	}
}

// A provider returning a zero Owner with no error is a bug in that provider.
// Treating it as success would authenticate a request as nobody, and every
// downstream query is scoped by the id it did not set.
func TestFirstRejectsAnInvalidOwner(t *testing.T) {
	empty := ProviderFunc(func(context.Context, *http.Request) (Owner, error) {
		return Owner{}, nil
	})
	chain := First(empty, named("X-Cookie", "by-cookie"))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Cookie", "yes")

	owner, err := chain.Resolve(context.Background(), r)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if owner.ID != "by-cookie" {
		t.Errorf("owner = %q, want by-cookie — an empty owner is not an answer", owner.ID)
	}
}

func TestFirstSkipsNilProviders(t *testing.T) {
	// An install with no account service passes a nil token provider. The chain
	// has to tolerate that rather than panic at the first request.
	chain := First(nil, named("X-Cookie", "by-cookie"), nil)

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Cookie", "yes")

	if _, err := chain.Resolve(context.Background(), r); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
}

func TestFirstWithNoProviders(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	if _, err := First().Resolve(context.Background(), r); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("err = %v, want ErrUnauthenticated", err)
	}
}

// The chain must work behind Middleware, which is the only way it is ever
// mounted.
func TestFirstBehindMiddleware(t *testing.T) {
	chain := First(named("X-Token", "team-a"))

	var seen Owner
	h := Middleware(chain)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		seen = MustFromContext(r.Context())
	}))

	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Token", "yes")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)

	if w.Code != http.StatusOK || seen.ID != "team-a" {
		t.Errorf("status = %d, owner = %q; want 200 and team-a", w.Code, seen.ID)
	}

	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", w.Code)
	}
}
