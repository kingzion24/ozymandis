package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The dashboard's cookie is SameSite=Lax, which stops other sites but not
// sibling subdomains — and every app this install deploys is one. This is the
// check that stops a tenant's page posting forms as whoever is signed in.
func TestSameOriginOnly(t *testing.T) {
	reached := false
	h := sameOriginOnly(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		reached = true
	}))

	for name, tc := range map[string]struct {
		method  string
		headers map[string]string
		allowed bool
	}{
		"a form on the dashboard itself": {
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-origin"}, true,
		},
		"a tenant app on a sibling subdomain": {
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "same-site"}, false,
		},
		"another site entirely": {
			http.MethodPost, map[string]string{"Sec-Fetch-Site": "cross-site"}, false,
		},
		"an older browser, same origin, behind a TLS proxy": {
			http.MethodPost, map[string]string{"Origin": "https://ozymandis.example.test"}, true,
		},
		"an older browser, sibling subdomain": {
			http.MethodPost, map[string]string{"Origin": "https://evil.apps.example.test"}, false,
		},
		"the oz CLI, which sends neither header": {
			http.MethodPost, nil, true,
		},
		"a cross-site link is still followed": {
			http.MethodGet, map[string]string{"Sec-Fetch-Site": "cross-site"}, true,
		},
	} {
		t.Run(name, func(t *testing.T) {
			reached = false
			req := httptest.NewRequest(tc.method, "http://ozymandis.example.test/team/users",
				strings.NewReader(""))
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if reached != tc.allowed {
				t.Fatalf("reached the handler = %v, want %v (status %d)",
					reached, tc.allowed, rec.Code)
			}
		})
	}
}
