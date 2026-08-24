package orchestrator

import (
	"strings"
	"testing"
)

func validSpec() AppSpec {
	return AppSpec{
		Ref:      Ref{Owner: "owner-1", Namespace: "ozymandis-demo", Name: "web"},
		Image:    "nginx:alpine",
		Replicas: 1,
		Port:     8080,
	}
}

func TestAppSpecAcceptsHosts(t *testing.T) {
	s := validSpec()
	s.Issuer = IssuerRef{Name: "letsencrypt"}
	s.Hosts = []HostSpec{
		{Name: "web.apps.example.com", Cert: CertIssued},
		{Name: "www.customer.test", Cert: CertIssued},
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The check that matters: a hostname needing a certificate of its own, on an
// install that cannot obtain one, is refused rather than deployed. Accepting it
// would route the name and serve it under whatever certificate the controller
// holds for a different domain.
func TestAppSpecRejectsIssuedHostWithNoIssuer(t *testing.T) {
	s := validSpec()
	s.Hosts = []HostSpec{{Name: "www.customer.test", Cert: CertIssued}}

	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted a host needing issuance with no issuer configured")
	}
	if !strings.Contains(err.Error(), "www.customer.test") {
		t.Fatalf("error does not name the host: %v", err)
	}
}

// IssuedHosts selects the names a certificate is obtained for, and leaves the
// plain-HTTP ones out. The partition used to be three-way, across a default-cert
// source that no longer exists; what remains is the only distinction there is.
func TestAppSpecPartitionsHostsByCertificate(t *testing.T) {
	s := validSpec()
	s.Issuer = IssuerRef{Name: "letsencrypt"}
	s.Hosts = []HostSpec{
		{Name: "web.apps.example.com", Cert: CertIssued},
		{Name: "www.customer.test", Cert: CertIssued},
		{Name: "shop.customer.test", Cert: CertIssued},
		{Name: "plain.customer.test", Cert: CertNone},
	}

	got := s.IssuedHosts()
	if len(got) != 3 {
		t.Errorf("IssuedHosts = %v, want the three names asking for a certificate", got)
	}
	for _, h := range got {
		if h == "plain.customer.test" {
			t.Error("a CertNone host was included — it would be issued for and " +
				"served over TLS when the caller asked for plain HTTP")
		}
	}
}

// Two rules for one hostname is a caller that built its host list from two
// sources without merging them. Kubernetes accepts it and routes whichever
// rule it reads last, so the disagreement resolves silently.
func TestAppSpecRejectsDuplicateHosts(t *testing.T) {
	s := validSpec()
	// Both CertNone, and deliberately: a CertIssued host on a spec with no
	// issuer fails the issuer check first, so this test would pass on an error
	// that has nothing to do with duplication.
	s.Hosts = []HostSpec{
		{Name: "web.apps.example.com", Cert: CertNone},
		{Name: "web.apps.example.com", Cert: CertNone},
	}
	err := s.Validate()
	if err == nil {
		t.Fatal("Validate accepted the same hostname twice")
	}
	if !strings.Contains(err.Error(), "routed twice") {
		t.Fatalf("error = %v, want the duplicate-hostname one", err)
	}
}

func TestAppSpecRejectsMalformedHosts(t *testing.T) {
	cases := map[string]string{
		"empty":         "",
		"space":         "web .example.com",
		"uppercase":     "WEB.example.com",
		"scheme":        "https://web.example.com",
		"path":          "web.example.com/app",
		"port":          "web.example.com:8080",
		"leading dot":   ".web.example.com",
		"double dot":    "web..example.com",
		"underscore":    "web_1.example.com",
		"trailing dash": "web-.example.com",
	}

	for name, host := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.Hosts = []HostSpec{Host(host)}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted malformed host %q", host)
			}
		})
	}
}

// A spec that takes no traffic cannot be routed to, so declaring hosts for it
// is a wiring mistake worth catching here rather than producing an Ingress
// pointing at a Service that was never created.
func TestAppSpecRejectsHostsWithoutAPort(t *testing.T) {
	s := validSpec()
	s.Port = 0
	s.Hosts = []HostSpec{Host("web.apps.example.com")}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted hosts on a spec with no port")
	}
}

func TestAppSpecAcceptsVolumes(t *testing.T) {
	s := validSpec()
	s.Volumes = []VolumeSpec{{Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30}}
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A ReadWriteOnce volume mounts on one node at a time, so the second pod can
// never schedule. Refusing here means the person is told at the moment they
// ask; the alternative is a pod stuck Pending forever with the reason buried
// in kubectl describe.
func TestAppSpecRefusesVolumesWithMoreThanOneReplica(t *testing.T) {
	s := validSpec()
	s.Replicas = 2
	s.Volumes = []VolumeSpec{{Name: "data", MountPath: "/var/lib/data", SizeBytes: 1 << 30}}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a volume on a workload with two replicas")
	}
}

func TestAppSpecRejectsMalformedVolumes(t *testing.T) {
	cases := map[string]VolumeSpec{
		"no name":         {MountPath: "/data", SizeBytes: 1 << 30},
		"bad name":        {Name: "Data!", MountPath: "/data", SizeBytes: 1 << 30},
		"relative mount":  {Name: "data", MountPath: "var/lib", SizeBytes: 1 << 30},
		"root mount":      {Name: "data", MountPath: "/", SizeBytes: 1 << 30},
		"no size":         {Name: "data", MountPath: "/data"},
		"negative size":   {Name: "data", MountPath: "/data", SizeBytes: -1},
		"trailing slash":  {Name: "data", MountPath: "/data/", SizeBytes: 1 << 30},
		"dot dot in path": {Name: "data", MountPath: "/data/../etc", SizeBytes: 1 << 30},
	}
	for name, v := range cases {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.Volumes = []VolumeSpec{v}
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %+v", v)
			}
		})
	}
}

// Two volumes at one path is a workload where one silently wins.
func TestAppSpecRejectsDuplicateMountPaths(t *testing.T) {
	s := validSpec()
	s.Volumes = []VolumeSpec{
		{Name: "a", MountPath: "/var/lib/data", SizeBytes: 1 << 30},
		{Name: "b", MountPath: "/var/lib/data", SizeBytes: 1 << 30},
	}
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted two volumes mounted at the same path")
	}
}

func TestAppSpecAcceptsAHealthPath(t *testing.T) {
	s := validSpec()
	s.HealthPath = "/healthz"
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// A probe is an HTTP GET against the app's own port. With no port there is
// nothing to probe, and Kubernetes would reject the pod rather than explain.
func TestAppSpecRejectsAHealthPathWithNoPort(t *testing.T) {
	s := validSpec()
	s.Port = 0
	s.HealthPath = "/healthz"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a health path on a workload with no port")
	}
}

func TestAppSpecRejectsMalformedHealthPaths(t *testing.T) {
	for name, path := range map[string]string{
		"relative":  "healthz",
		"scheme":    "http://x/healthz",
		"space":     "/health z",
		"not clean": "/a/../healthz",
		"query":     "/healthz?x=1",
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			s.HealthPath = path
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted %q", path)
			}
		})
	}
}

// Liveness restarts the container. Asking for it without saying what to probe
// is a request that cannot be honoured, and honouring it by guessing would
// restart a working app.
func TestAppSpecRejectsLivenessWithNoHealthPath(t *testing.T) {
	s := validSpec()
	s.Liveness = true
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted liveness with nothing to probe")
	}
}

// ---------------------------------------------------------------- resources

func TestParseCPUQuantity(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"100m", 100, false},
		{"1500m", 1500, false},
		{"0m", 0, false},
		{"1", 1000, false},
		{"2", 2000, false},
		{"0.5", 500, false},
		{"", 0, true},
		{"-100m", 0, true},
		{"1x", 0, true},
		{"m", 0, true},
	} {
		got, err := ParseCPUQuantity(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseCPUQuantity(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseCPUQuantity(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseCPUQuantity(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestParseMemoryQuantity(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"512Mi", 512 * (1 << 20), false},
		{"1Gi", 1 << 30, false},
		{"128Ki", 128 * (1 << 10), false},
		{"1G", 1_000_000_000, false},
		{"1024", 1024, false},
		{"", 0, true},
		{"-1Gi", 0, true},
		{"1Xi", 0, true},
		{"Mi", 0, true},
	} {
		got, err := ParseMemoryQuantity(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMemoryQuantity(%q) = %d, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMemoryQuantity(%q) unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMemoryQuantity(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// A spec with no resources of its own is always valid: it falls back to the
// namespace default rather than requesting anything, which is every app
// before this feature existed and must keep working unchanged.
func TestAppSpecWithNoResourcesIsValid(t *testing.T) {
	s := validSpec()
	if err := s.Validate(); err != nil {
		t.Fatalf("Validate rejected a spec with no resources: %v", err)
	}
	if err := s.FitsWithinLimits(DefaultLimits); err != nil {
		t.Fatalf("FitsWithinLimits rejected a spec with no resources: %v", err)
	}
}

func TestAppSpecRejectsUnparsableResources(t *testing.T) {
	for name, mutate := range map[string]func(*AppSpec){
		"cpu request":    func(s *AppSpec) { s.CPURequest = "not-a-quantity" },
		"cpu limit":      func(s *AppSpec) { s.CPULimit = "2x" },
		"memory request": func(s *AppSpec) { s.MemoryRequest = "lots" },
		"memory limit":   func(s *AppSpec) { s.MemoryLimit = "512Xi" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			if err := s.Validate(); err == nil {
				t.Fatalf("Validate accepted an unparsable %s", name)
			}
		})
	}
}

// A request bigger than its own limit cannot be satisfied by Kubernetes and
// should be refused here, where the error names the two values somebody
// typed, rather than at the cluster as an admission rejection.
func TestAppSpecRejectsRequestAboveItsOwnLimit(t *testing.T) {
	s := validSpec()
	s.CPURequest, s.CPULimit = "500m", "250m"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a cpu request greater than its limit")
	}

	s = validSpec()
	s.MemoryRequest, s.MemoryLimit = "1Gi", "512Mi"
	if err := s.Validate(); err == nil {
		t.Fatal("Validate accepted a memory request greater than its limit")
	}
}

// FitsWithinLimits is the ceiling check Validate cannot do itself, since
// Validate has no namespace to check against.
func TestFitsWithinLimitsRejectsAboveTheCeiling(t *testing.T) {
	for name, mutate := range map[string]func(*AppSpec){
		"cpu request":    func(s *AppSpec) { s.CPURequest = "4" },
		"cpu limit":      func(s *AppSpec) { s.CPULimit = "4" },
		"memory request": func(s *AppSpec) { s.MemoryRequest = "8Gi" },
		"memory limit":   func(s *AppSpec) { s.MemoryLimit = "8Gi" },
	} {
		t.Run(name, func(t *testing.T) {
			s := validSpec()
			mutate(&s)
			if err := s.FitsWithinLimits(DefaultLimits); err == nil {
				t.Fatalf("FitsWithinLimits accepted a %s above the ceiling", name)
			}
		})
	}
}

func TestFitsWithinLimitsAcceptsAtTheCeiling(t *testing.T) {
	s := validSpec()
	s.CPURequest, s.CPULimit = "1", DefaultLimits.MaxCPU
	s.MemoryRequest, s.MemoryLimit = "1Gi", DefaultLimits.MaxMemory
	if err := s.FitsWithinLimits(DefaultLimits); err != nil {
		t.Fatalf("FitsWithinLimits rejected values exactly at the ceiling: %v", err)
	}
}
