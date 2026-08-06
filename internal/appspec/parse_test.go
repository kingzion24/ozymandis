package appspec

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const full = `
name = "api"

[build]
repo   = "https://github.com/you/app.git"
branch = "main"
subdir = "services/api"

[deploy]
release_command = "./bin/migrate"

[service]
port     = 8080
internal = false

[health]
path     = "/healthz"
liveness = true

[scale]
replicas = 2

[env]
LOG_LEVEL = "info"
REGION    = "eu"

[[volumes]]
name = "data"
path = "/data"
size = "10Gi"

[[domains]]
host = "api.example.com"
`

func TestParseFull(t *testing.T) {
	s, err := Parse([]byte(full))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	if s.Name != "api" {
		t.Errorf("name = %q", s.Name)
	}
	if s.Build == nil || s.Build.Repo == nil || *s.Build.Repo != "https://github.com/you/app.git" {
		t.Errorf("build = %+v", s.Build)
	}
	if s.Deploy == nil || s.Deploy.ReleaseCommand == nil || *s.Deploy.ReleaseCommand != "./bin/migrate" {
		t.Errorf("deploy = %+v", s.Deploy)
	}
	if s.Service == nil || s.Service.Port == nil || *s.Service.Port != 8080 {
		t.Errorf("service = %+v", s.Service)
	}
	if s.Health == nil || s.Health.Path == nil || *s.Health.Path != "/healthz" {
		t.Errorf("health = %+v", s.Health)
	}
	if len(s.Env) != 2 || s.Env["LOG_LEVEL"] != "info" {
		t.Errorf("env = %v", s.Env)
	}
	if len(s.Volumes) != 1 || s.Volumes[0].Name != "data" {
		t.Errorf("volumes = %+v", s.Volumes)
	}
	if len(s.Domains) != 1 || s.Domains[0].Host != "api.example.com" {
		t.Errorf("domains = %+v", s.Domains)
	}
}

// The file is committed to a repository. A password in it is readable by
// everyone with access to the code, forever, including after it is deleted.
func TestSecretsInTheFileAreRefused(t *testing.T) {
	for _, doc := range []string{
		"name = \"web\"\n[secrets]\nDATABASE_URL = \"postgres://u:p@h/db\"\n",
		"name = \"web\"\n[secret]\nAPI_KEY = \"sk-live-1234\"\n",
	} {
		_, err := Parse([]byte(doc))
		if !errors.Is(err, ErrSecretsInFile) {
			t.Errorf("doc %q: err = %v, want ErrSecretsInFile", doc, err)
		}
	}
}

// The [secrets] message must be useful, not just correct: it is the only thing
// standing between somebody and a credential in their git history.
func TestTheSecretsMessageSaysWhatToDoInstead(t *testing.T) {
	_, err := Parse([]byte("name = \"web\"\n[secrets]\nK = \"v\"\n"))
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{"oz secrets set", "history"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the message does not mention %q: %s", want, err)
		}
	}
}

// A misspelled key is a setting somebody believes they made and did not.
// Ignoring it means the deploy goes out with the old value and says nothing.
func TestUnknownKeysAreRefused(t *testing.T) {
	cases := map[string]string{
		"top level typo":  "name = \"web\"\nreplicaz = 3\n",
		"section typo":    "name = \"web\"\n[scaling]\nreplicas = 3\n",
		"key typo":        "name = \"web\"\n[scale]\nreplica = 3\n",
		"plausible extra": "name = \"web\"\n[service]\nprotocol = \"grpc\"\n",
	}
	for label, doc := range cases {
		_, err := Parse([]byte(doc))
		if err == nil {
			t.Errorf("%s: accepted silently", label)
			continue
		}
		if errors.Is(err, ErrSecretsInFile) {
			t.Errorf("%s: reported as a secrets error", label)
		}
	}
}

// A typo AND a secrets table: the message that matters must win.
func TestSecretsWinOverAnUnknownKey(t *testing.T) {
	doc := "name = \"web\"\nreplicaz = 3\n[secrets]\nK = \"v\"\n"
	if _, err := Parse([]byte(doc)); !errors.Is(err, ErrSecretsInFile) {
		t.Errorf("err = %v, want ErrSecretsInFile", err)
	}
}

func TestValidate(t *testing.T) {
	cases := map[string]string{
		"no name":             "[scale]\nreplicas = 1\n",
		"blank name":          "name = \"   \"\n",
		"repo and image":      "name = \"w\"\n[build]\nrepo = \"https://e.com/a.git\"\nimage = \"nginx\"\n",
		"branch without repo": "name = \"w\"\n[build]\nbranch = \"main\"\n",
		"port zero":           "name = \"w\"\n[service]\nport = 0\n",
		"port too big":        "name = \"w\"\n[service]\nport = 70000\n",
		"relative health":     "name = \"w\"\n[health]\npath = \"healthz\"\n",
		"negative replicas":   "name = \"w\"\n[scale]\nreplicas = -1\n",
		"volume no path":      "name = \"w\"\n[[volumes]]\nname = \"d\"\n",
		"volume root path":    "name = \"w\"\n[[volumes]]\nname = \"d\"\npath = \"/\"\n",
		"volume relative":     "name = \"w\"\n[[volumes]]\nname = \"d\"\npath = \"data\"\n",
		"bad size":            "name = \"w\"\n[[volumes]]\nname = \"d\"\npath = \"/d\"\nsize = \"big\"\n",
		"blank host":          "name = \"w\"\n[[domains]]\nhost = \"  \"\n",
	}
	for label, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

func TestDuplicatesAreRefused(t *testing.T) {
	cases := map[string]string{
		"two volumes named the same": "name = \"w\"\n" +
			"[[volumes]]\nname = \"d\"\npath = \"/a\"\n" +
			"[[volumes]]\nname = \"d\"\npath = \"/b\"\n",
		"two volumes at one path": "name = \"w\"\n" +
			"[[volumes]]\nname = \"a\"\npath = \"/d\"\n" +
			"[[volumes]]\nname = \"b\"\npath = \"/d\"\n",
		"one host twice": "name = \"w\"\n" +
			"[[domains]]\nhost = \"a.example\"\n" +
			"[[domains]]\nhost = \"a.example\"\n",
	}
	for label, doc := range cases {
		if _, err := Parse([]byte(doc)); err == nil {
			t.Errorf("%s: accepted", label)
		}
	}
}

// port = 0 is invalid, but it must be rejected by Validate rather than read as
// "no port given" — the pointer is what makes that distinction possible and
// this pins that it is actually used.
func TestPortZeroIsRejectedNotIgnored(t *testing.T) {
	_, err := Parse([]byte("name = \"w\"\n[service]\nport = 0\n"))
	if err == nil {
		t.Fatal("port = 0 was accepted")
	}
	if !strings.Contains(err.Error(), "0") {
		t.Errorf("the error does not mention the value: %v", err)
	}

	// Whereas an absent port is fine.
	if _, err := Parse([]byte("name = \"w\"\n[service]\n")); err != nil {
		t.Errorf("an absent port was rejected: %v", err)
	}
}

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0":     0,
		"1":     1,
		"512":   512,
		"10Gi":  10 << 30,
		"512Mi": 512 << 20,
		"1T":    1000 * 1000 * 1000 * 1000,
		"2Ki":   2048,
	}
	for in, want := range cases {
		got, err := ParseSize(in)
		if err != nil {
			t.Errorf("ParseSize(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}

	for _, in := range []string{"", "big", "10GB", "-1", "1.5Gi", "Gi", "99999999999999999999"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) was accepted", in)
		}
	}
}

func TestLoadFromDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, FileName)
	if err := os.WriteFile(path, []byte(full), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	s, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Name != "api" {
		t.Errorf("name = %q", s.Name)
	}

	if _, err := Load(filepath.Join(dir, "nope.toml")); err == nil {
		t.Error("a missing file loaded")
	}
}

// Re-encoding a partial file must not turn its silences into explicit zeros —
// which would convert "say nothing about scaling" into "scale to nothing" on
// the next deploy.
func TestEncodeKeepsAbsentFieldsAbsent(t *testing.T) {
	s, err := Parse([]byte("name = \"web\"\n[service]\nport = 8080\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	out, err := Encode(s)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Contains(string(out), "replicas") {
		t.Errorf("an absent field was written out: %s", out)
	}

	back, err := Parse(out)
	if err != nil {
		t.Fatalf("re-parse: %v", err)
	}
	if back.Scale != nil {
		t.Errorf("scale came back as %+v after a round trip", back.Scale)
	}
	if back.Service == nil || back.Service.Port == nil || *back.Service.Port != 8080 {
		t.Errorf("port did not survive: %+v", back.Service)
	}
}

func TestEnvKeysAreSorted(t *testing.T) {
	s, err := Parse([]byte("name = \"w\"\n[env]\nZ = \"1\"\nA = \"2\"\nM = \"3\"\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got := s.EnvKeys()
	want := []string{"A", "M", "Z"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("EnvKeys() = %v, want %v", got, want)
		}
	}
}
