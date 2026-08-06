package backup

import (
	"strings"
	"testing"
)

func validDestination() Destination {
	return Destination{
		Endpoint:        "https://account.r2.cloudflarestorage.com",
		Bucket:          "ozymandis-backups",
		Prefix:          "install-1",
		Region:          "auto",
		AccessKeyID:     "AKIAEXAMPLE",
		SecretAccessKey: "s3cr3t",
		RepoPassword:    "a-long-enough-password",
	}
}

func TestDestinationAcceptsAWellFormedOne(t *testing.T) {
	if err := validDestination().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

// The credential and every byte of every backup cross this connection, so plain
// HTTP to somewhere on the internet is refused.
func TestDestinationRefusesPlainHTTPToAPublicHost(t *testing.T) {
	d := validDestination()
	d.Endpoint = "http://s3.example.com"

	err := d.Validate()
	if err == nil {
		t.Fatal("Validate accepted a public endpoint over plain http")
	}
	if !strings.Contains(err.Error(), "https") {
		t.Fatalf("the error does not say what to do: %v", err)
	}
}

// A self-hosted MinIO on a private network or inside the cluster has no
// certificate to present, and is a legitimate destination. Refusing it would
// push people towards a public bucket, which is a worse answer to a question
// about keeping data safe.
func TestDestinationAllowsPlainHTTPWhereItCannotLeaveTheNetwork(t *testing.T) {
	for _, endpoint := range []string{
		"http://localhost:9000",
		"http://127.0.0.1:9000",
		"http://10.0.0.5:9000",
		"http://192.168.1.20:9000",
		"http://172.16.4.2:9000",
		"http://minio:9000",
		"http://minio.storage.svc.cluster.local:9000",
		"http://nas.internal:9000",
	} {
		t.Run(endpoint, func(t *testing.T) {
			d := validDestination()
			d.Endpoint = endpoint
			if err := d.Validate(); err != nil {
				t.Fatalf("Validate refused a private endpoint: %v", err)
			}
		})
	}
}

// Matched on the label boundary, never as a substring: notcluster.local is a
// public name that happens to end with one that is not.
func TestDestinationDoesNotMistakeALookalikeForAPrivateName(t *testing.T) {
	d := validDestination()
	d.Endpoint = "http://evil-cluster.local.attacker.example.com"
	if err := d.Validate(); err == nil {
		t.Fatal("a public host was treated as cluster-internal")
	}
}

// The one credential with no recovery path is the one with a length floor.
func TestDestinationRefusesAShortRepositoryPassword(t *testing.T) {
	d := validDestination()
	d.RepoPassword = "short"
	if err := d.Validate(); err == nil {
		t.Fatal("Validate accepted a repository password too short to protect anything")
	}
}

func TestDestinationRefusesMalformedInput(t *testing.T) {
	cases := map[string]func(*Destination){
		"no endpoint":    func(d *Destination) { d.Endpoint = "" },
		"not a url":      func(d *Destination) { d.Endpoint = "not a url" },
		"wrong scheme":   func(d *Destination) { d.Endpoint = "ftp://s3.example.com" },
		"bad bucket":     func(d *Destination) { d.Bucket = "Not A Bucket" },
		"traversal":      func(d *Destination) { d.Prefix = "../../etc" },
		"no access key":  func(d *Destination) { d.AccessKeyID = "" },
		"no secret":      func(d *Destination) { d.SecretAccessKey = "" },
		"no repo secret": func(d *Destination) { d.RepoPassword = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := validDestination()
			mutate(&d)
			if err := d.Validate(); err == nil {
				t.Fatal("Validate accepted it")
			}
		})
	}
}

// One repository per app: restic locks a repository while writing, and `restic
// forget` applies retention to the whole of one — so a shared repository would
// let one app's retention prune another's snapshots.
func TestRepositoryIsPerApp(t *testing.T) {
	d := validDestination()

	a := d.Repository("db")
	b := d.Repository("cache")
	if a == b {
		t.Fatal("two apps share a repository")
	}
	if !strings.HasPrefix(a, "s3:https://") {
		t.Errorf("repository = %q, want restic's s3 form", a)
	}
	if !strings.HasSuffix(a, "/ozymandis-backups/install-1/db") {
		t.Errorf("repository = %q, want it under the bucket and prefix", a)
	}
}

func TestRepositoryOmitsAnEmptyPrefix(t *testing.T) {
	d := validDestination()
	d.Prefix = ""
	if got := d.Repository("db"); !strings.HasSuffix(got, "/ozymandis-backups/db") {
		t.Fatalf("repository = %q, want no empty path segment", got)
	}
}

// The most consequential check in the package. `restic forget` with every count
// at zero matches nothing as worth keeping and deletes every snapshot —
// including the one the job just took. A policy that reads as "keep nothing"
// behaves as "destroy the backups".
func TestPolicyRefusesToKeepNothing(t *testing.T) {
	p := DefaultPolicy()
	p.KeepDaily, p.KeepWeekly, p.KeepMonthly = 0, 0, 0

	err := p.Validate()
	if err == nil {
		t.Fatal("Validate accepted a policy that would delete every backup")
	}
	if !strings.Contains(err.Error(), "delete every backup") {
		t.Fatalf("the error does not say what would happen: %v", err)
	}
}

func TestPolicyAcceptsTheDefault(t *testing.T) {
	if err := DefaultPolicy().Validate(); err != nil {
		t.Fatalf("the default policy is invalid: %v", err)
	}
}

func TestPolicyRefusesAMalformedSchedule(t *testing.T) {
	for name, schedule := range map[string]string{
		"empty":          "",
		"too few fields": "17 3 * *",
		"too many":       "17 3 * * * *",
		"a sentence":     "every night at three",
		"shell":          "17 3 * * *; rm -rf /",
	} {
		t.Run(name, func(t *testing.T) {
			p := DefaultPolicy()
			p.Schedule = schedule
			if err := p.Validate(); err == nil {
				t.Fatalf("Validate accepted %q", schedule)
			}
		})
	}
}

// A Postgres app is dumped, not copied file by file: its volume holds that same
// database's files, and copying those from a running server yields a backup
// that is consistent only by luck.
func TestAPostgresAppIsBackedUpByDumpAndNotByItsVolume(t *testing.T) {
	a := App{
		Postgres:     true,
		PostgresUser: "ozymandis",
		PostgresDB:   "ozymandis",
		Port:         5432,
		Volumes:      []Volume{{Name: "data", MountPath: "/var/lib/postgresql/data"}},
	}
	a.Ref.Name = "db"

	targets := a.Targets()
	if len(targets) != 1 {
		t.Fatalf("targets = %+v, want exactly the dump", targets)
	}
	if targets[0].Kind != KindPostgres {
		t.Errorf("kind = %q, want postgres", targets[0].Kind)
	}
	for _, tg := range targets {
		if tg.Kind == KindVolume {
			t.Fatal("the database's own data directory is being copied as files as well")
		}
	}
}

func TestAnAppWithVolumesGetsOneTargetEach(t *testing.T) {
	a := App{Volumes: []Volume{
		{Name: "data", MountPath: "/data"},
		{Name: "uploads", MountPath: "/uploads"},
	}}
	a.Ref.Name = "web"

	if got := a.Targets(); len(got) != 2 {
		t.Fatalf("targets = %+v, want one per volume", got)
	}
}

func TestAStatelessAppHasNothingToBackUp(t *testing.T) {
	a := App{}
	a.Ref.Name = "web"

	if got := a.Targets(); len(got) != 0 {
		t.Fatalf("targets = %+v, want none", got)
	}
	if _, err := a.Schedules(validDestination(), DefaultPolicy()); err != ErrNothingToBackUp {
		t.Fatalf("err = %v, want ErrNothingToBackUp", err)
	}
}

// A backup must never be able to write to what it is copying.
func TestBackupSchedulesMountVolumesReadOnly(t *testing.T) {
	a := App{Volumes: []Volume{{Name: "data", MountPath: "/data"}}}
	a.Ref.Name = "web"
	a.Ref.Owner = "owner-1"
	a.Ref.Namespace = "ozymandis-web"

	specs, err := a.Schedules(validDestination(), DefaultPolicy())
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	if len(specs) != 1 {
		t.Fatalf("got %d schedules, want 1", len(specs))
	}
	if len(specs[0].Mounts) != 1 || !specs[0].Mounts[0].ReadOnly {
		t.Fatalf("mounts = %+v, want the volume read-only", specs[0].Mounts)
	}
	if specs[0].App != "web" {
		t.Errorf("mount app = %q, want the app that owns the volume", specs[0].App)
	}
}

// Each target gets its own schedule, so a failed volume backup and a failed
// database dump are distinguishable rather than one exit status for both.
func TestEachTargetGetsItsOwnSchedule(t *testing.T) {
	a := App{Volumes: []Volume{
		{Name: "data", MountPath: "/data"},
		{Name: "uploads", MountPath: "/uploads"},
	}}
	a.Ref.Name = "web"
	a.Ref.Owner = "owner-1"
	a.Ref.Namespace = "ozymandis-web"

	specs, err := a.Schedules(validDestination(), DefaultPolicy())
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}

	names := map[string]bool{}
	for _, s := range specs {
		if names[s.Name] {
			t.Fatalf("two schedules share the name %q, so one would overwrite the other", s.Name)
		}
		names[s.Name] = true
		if err := s.Validate(); err != nil {
			t.Errorf("schedule %q is invalid: %v", s.Name, err)
		}
	}
	if len(names) != 2 {
		t.Fatalf("got %d schedules, want one per volume", len(names))
	}
}

// A disabled policy suspends rather than deletes, so switching backups off for
// an afternoon does not lose the schedule.
func TestADisabledPolicySuspendsTheSchedule(t *testing.T) {
	a := App{Volumes: []Volume{{Name: "data", MountPath: "/data"}}}
	a.Ref.Name = "web"
	a.Ref.Owner = "owner-1"
	a.Ref.Namespace = "ozymandis-web"

	p := DefaultPolicy()
	p.Enabled = false

	specs, err := a.Schedules(validDestination(), p)
	if err != nil {
		t.Fatalf("Schedules: %v", err)
	}
	if !specs[0].Suspended {
		t.Fatal("a disabled policy produced a schedule that still fires")
	}
}

// Credentials belong in Secrets, never in Env — Env becomes literals in a pod
// template, which is the copy people paste into issues.
func TestCredentialsAreNeverInThePlainEnvironment(t *testing.T) {
	d := validDestination()

	for k, v := range Env(d, "db") {
		if v == d.SecretAccessKey || v == d.RepoPassword {
			t.Errorf("%s carries a credential in the plain environment", k)
		}
	}

	secrets := Secrets(d)
	if secrets["RESTIC_PASSWORD"] != d.RepoPassword {
		t.Error("the repository password did not reach the secrets")
	}
	if secrets["AWS_SECRET_ACCESS_KEY"] != d.SecretAccessKey {
		t.Error("the storage secret did not reach the secrets")
	}
}

// pipefail is what stops a truncated pg_dump being stored as a complete
// backup, and it only works if it is actually in the script.
func TestThePostgresBackupScriptFailsOnATruncatedDump(t *testing.T) {
	script := BackupScript(DefaultPolicy(), Target{
		Kind: KindPostgres, Name: "ozymandis", Service: "db", Port: 5432,
	})
	if !strings.Contains(script, "set -o pipefail") {
		t.Fatal("the dump is piped into restic without pipefail: a pg_dump that " +
			"dies halfway would be stored as a complete backup")
	}
	if !strings.Contains(script, "pg_dump") || !strings.Contains(script, "restic backup --stdin") {
		t.Error("the script does not stream a dump into restic")
	}
}

// ON_ERROR_STOP is what makes a failed restore report as failed. Without it
// psql prints errors, carries on, and exits zero.
func TestThePostgresRestoreScriptStopsOnTheFirstError(t *testing.T) {
	script := RestoreScript(Target{
		Kind: KindPostgres, Name: "ozymandis", Service: "db", Port: 5432,
	}, "latest")
	if !strings.Contains(script, "ON_ERROR_STOP=1") {
		t.Fatal("a restore that half failed would be reported as having worked")
	}
}

// --delete is what makes a volume restore a restore rather than a merge, and
// --include is what keeps that deletion inside the volume.
func TestTheVolumeRestoreScriptDeletesWithinTheVolumeOnly(t *testing.T) {
	script := RestoreScript(Target{Kind: KindVolume, Name: "data"}, "abc123")

	if !strings.Contains(script, "--delete") {
		t.Error("a restore would merge over whatever is there rather than replace it")
	}
	if !strings.Contains(script, "--include "+dataMount) {
		t.Fatal("the deletion is not scoped to the mounted volume")
	}
}

// The tags are how a restore finds the right snapshot in a repository holding
// several targets.
func TestSnapshotsCarryTheirTargetAndKind(t *testing.T) {
	tg := Target{Kind: KindVolume, Name: "uploads"}
	s := Snapshot{Tags: tg.Tags()}

	if s.Target() != "uploads" {
		t.Errorf("Target() = %q, want uploads", s.Target())
	}
	if s.Kind() != KindVolume {
		t.Errorf("Kind() = %q, want volume", s.Kind())
	}
}

func TestParseSnapshotsReadsResticOutput(t *testing.T) {
	// The preamble writes to the same stream, so the parser has to find the
	// JSON rather than assume it starts at byte zero.
	output := `Initialising the repository at s3:https://x/y
[{"short_id":"11b08d95","time":"2026-08-04T13:45:03.1Z",` +
		`"tags":["target:ozymandis","kind:postgres"],` +
		`"summary":{"total_bytes_processed":13797}}]`

	snaps, err := parseSnapshots(output)
	if err != nil {
		t.Fatalf("parseSnapshots: %v", err)
	}
	if len(snaps) != 1 {
		t.Fatalf("got %d snapshots, want 1", len(snaps))
	}
	s := snaps[0]
	if s.ID != "11b08d95" {
		t.Errorf("id = %q", s.ID)
	}
	if s.Target() != "ozymandis" || s.Kind() != KindPostgres {
		t.Errorf("tags did not survive: %+v", s.Tags)
	}
	if s.SizeBytes != 13797 {
		t.Errorf("size = %d, want 13797", s.SizeBytes)
	}
	if s.Time.IsZero() {
		t.Error("time did not survive")
	}
}

func TestParseSnapshotsRefusesOutputWithNoListing(t *testing.T) {
	if _, err := parseSnapshots("Fatal: unable to open config file"); err == nil {
		t.Fatal("a failed listing was parsed as zero snapshots, which reads as " +
			"an empty repository rather than as an error")
	}
}
