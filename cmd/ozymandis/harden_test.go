package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRealIP(t *testing.T) {
	trusted, err := parseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		remote string
		xff    []string
		realIP string
		want   string
	}{
		{
			name:   "a direct caller cannot choose its own address",
			remote: "203.0.113.9:4000",
			xff:    []string{"198.51.100.1"},
			want:   "203.0.113.9:4000",
		},
		{
			name:   "a trusted proxy is believed",
			remote: "10.42.0.7:4000",
			xff:    []string{"198.51.100.1"},
			want:   "198.51.100.1",
		},
		{
			name:   "what the sender typed ahead of the proxy's entry is ignored",
			remote: "10.42.0.7:4000",
			xff:    []string{"1.2.3.4, 198.51.100.1"},
			want:   "198.51.100.1",
		},
		{
			name:   "a header of the sender's own ahead of the proxy's is ignored",
			remote: "10.42.0.7:4000",
			xff:    []string{"1.2.3.4", "198.51.100.1"},
			want:   "198.51.100.1",
		},
		{
			name:   "trusted hops are skipped to reach the client",
			remote: "127.0.0.1:4000",
			xff:    []string{"198.51.100.1, 10.42.0.7"},
			want:   "198.51.100.1",
		},
		{
			name:   "a hop that is not an address is not believed",
			remote: "10.42.0.7:4000",
			xff:    []string{"198.51.100.1, nonsense"},
			want:   "10.42.0.7:4000",
		},
		{
			name:   "a hop with a port is read without it",
			remote: "10.42.0.7:4000",
			xff:    []string{"198.51.100.1:4711"},
			want:   "198.51.100.1",
		},
		{
			name:   "a bracketed IPv6 hop is read",
			remote: "10.42.0.7:4000",
			xff:    []string{"[2001:db8::1]"},
			want:   "2001:db8::1",
		},
		{
			name:   "an unreadable hop falls back to X-Real-IP",
			remote: "10.42.0.7:4000",
			xff:    []string{"nonsense"},
			realIP: "198.51.100.3",
			want:   "198.51.100.3",
		},
		{
			name:   "X-Real-IP from a trusted proxy",
			remote: "10.42.0.7:4000",
			realIP: "198.51.100.2",
			want:   "198.51.100.2",
		},
		{
			name:   "X-Real-IP from a direct caller",
			remote: "203.0.113.9:4000",
			realIP: "198.51.100.2",
			want:   "203.0.113.9:4000",
		},
		{
			name:   "no headers leaves the peer",
			remote: "10.42.0.7:4000",
			want:   "10.42.0.7:4000",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got string
			h := realIP(trusted, "")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
			}))

			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = tc.remote
			for _, v := range tc.xff {
				r.Header.Add("X-Forwarded-For", v)
			}
			if tc.realIP != "" {
				r.Header.Set("X-Real-IP", tc.realIP)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)

			if got != tc.want {
				t.Errorf("RemoteAddr = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestParseTrustedProxies(t *testing.T) {
	if _, err := parseTrustedProxies([]string{"10.0.0.0/8", "192.0.2.1"}); err != nil {
		t.Errorf("a CIDR and a bare address should parse: %v", err)
	}
	if _, err := parseTrustedProxies([]string{"traefik"}); err == nil {
		t.Error("a value that is not an address must be refused, not skipped")
	}
}

func TestSecurityHeaders(t *testing.T) {
	h := securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	for header, want := range map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "same-origin",
	} {
		if got := rec.Header().Get(header); got != want {
			t.Errorf("%s = %q, want %q", header, got, want)
		}
	}
	if rec.Header().Get("Content-Security-Policy") == "" {
		t.Error("no Content-Security-Policy set")
	}
}

func TestRealIPEdgeSecret(t *testing.T) {
	trusted, err := parseTrustedProxies(nil)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, header, want string
	}{
		{"the edge, with the secret, is believed", "s3cret", "198.51.100.1"},
		{"a tenant pod without it is not", "", "10.42.0.7:4000"},
		{"a wrong secret is not", "guess", "10.42.0.7:4000"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got, leaked string
			h := realIP(trusted, "s3cret")(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				got = r.RemoteAddr
				leaked = r.Header.Get(edgeHeader)
			}))
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.RemoteAddr = "10.42.0.7:4000"
			r.Header.Set("X-Forwarded-For", "198.51.100.1")
			if tc.header != "" {
				r.Header.Set(edgeHeader, tc.header)
			}
			h.ServeHTTP(httptest.NewRecorder(), r)

			if got != tc.want {
				t.Errorf("RemoteAddr = %q, want %q", got, tc.want)
			}
			if leaked != "" {
				t.Errorf("the edge header reached the handler: %q", leaked)
			}
		})
	}
}
