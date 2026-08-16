package backup_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/kingzion24/ozymandis/internal/backup"
)

// The scripts in this package are the only part of it that cannot be checked by
// reading them. Everything else — which targets an app has, what a repository
// URL is, whether a retention policy is safe — is Go, and tested as Go. The
// scripts are shell, run inside an image, against a storage API and a database,
// and the failure they are most likely to have is the one that only appears
// when all four are present at once.
//
// So this runs them. Not a simulation of them, and not a reimplementation: the
// exact strings BackupScript and RestoreScript return, executed in the exact
// image tasks use, against a real S3 API and a real Postgres. Then it checks
// the data came back.
//
// It needs those things standing, so it skips without them. A skip reads as a
// pass, so the environment it needs is documented in the README's Development
// section and the Makefile target that provides it is `make test-backup`.
//
// What this cannot cover is Kubernetes itself: whether the pod spec mounts the
// right claim, whether the Secret arrives. That is covered by the fake
// clientset tests in the k8s package. Between them the whole path is checked,
// and neither test needs a cluster.

const (
	envEndpoint = "OZYMANDIS_TEST_S3_ENDPOINT"
	envImage    = "OZYMANDIS_TEST_BACKUP_IMAGE"
	envNetwork  = "OZYMANDIS_TEST_DOCKER_NETWORK"
	envPGHost   = "OZYMANDIS_TEST_PG_HOST"
)

func testDestination(t *testing.T) backup.Destination {
	t.Helper()

	endpoint := os.Getenv(envEndpoint)
	if endpoint == "" || os.Getenv(envImage) == "" {
		t.Skipf("set %s and %s to run the backup round-trip (see `make test-backup`)",
			envEndpoint, envImage)
	}

	d := backup.Destination{
		Endpoint:        endpoint,
		Bucket:          envOr("OZYMANDIS_TEST_S3_BUCKET", "ozymandis-backups"),
		Prefix:          fmt.Sprintf("roundtrip-%d", time.Now().UnixNano()),
		Region:          "auto",
		AccessKeyID:     envOr("OZYMANDIS_TEST_S3_KEY", "ozminio"),
		SecretAccessKey: envOr("OZYMANDIS_TEST_S3_SECRET", "ozminio123"),

		// Long enough to satisfy the floor Validate enforces, which is the
		// point: a test that used a short one would be testing a destination
		// the product refuses to save.
		RepoPassword: "roundtrip-repository-password",
	}

	// The same validation a person's input goes through. If the test's own
	// destination would be rejected by the form, the test is not exercising a
	// configuration anybody can actually have.
	if err := d.Validate(); err != nil {
		t.Fatalf("the test destination is one the product would refuse: %v", err)
	}
	return d
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// run executes a generated script in the backup image, the way a task would.
func run(t *testing.T, d backup.Destination, app, script string, extraEnv map[string]string) (string, error) {
	t.Helper()

	args := []string{
		"run", "--rm",
		"--network", envOr(envNetwork, "oz-backup-test"),
		"--user", "65532:65532",
	}

	env := backup.Env(d, app)
	for k, v := range backup.Secrets(d) {
		env[k] = v
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}

	args = append(args, os.Getenv(envImage), "-c", script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// psql runs a query against the source database and returns its output.
func psql(t *testing.T, query string) string {
	t.Helper()

	host := envOr(envPGHost, "oz-pg-src")
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	out, err := exec.CommandContext(ctx, "docker", "exec",
		"-e", "PGPASSWORD=pgsecret", host,
		"psql", "-U", "ozymandis", "-d", "ozymandis", "-tAc", query).CombinedOutput()
	if err != nil {
		t.Fatalf("psql %q: %v\n%s", query, err, out)
	}
	return strings.TrimSpace(string(out))
}

func postgresTarget() backup.Target {
	return backup.Target{
		Kind:    backup.KindPostgres,
		Name:    "ozymandis",
		Service: envOr(envPGHost, "oz-pg-src"),
		Port:    5432,
	}
}

func postgresEnv() map[string]string {
	return map[string]string{
		"PGUSER":     "ozymandis",
		"PGDATABASE": "ozymandis",
		"PGPASSWORD": "pgsecret",
	}
}

// The test this whole package exists to make possible: data is backed up, the
// data is then destroyed, and the backup brings it back.
//
// Destroying it in the middle is not theatre. A restore that runs against
// data which is already correct proves nothing at all — it is the single most
// common way a backup procedure gets verified and the reason so many of them
// turn out not to work.
func TestBackupAndRestoreRoundTrip(t *testing.T) {
	d := testDestination(t)
	app := envOr(envPGHost, "oz-pg-src")
	target := postgresTarget()

	// A known starting state.
	psql(t, `DROP TABLE IF EXISTS invoices`)
	psql(t, `CREATE TABLE invoices (id serial PRIMARY KEY, customer text NOT NULL, cents bigint NOT NULL)`)
	psql(t, `INSERT INTO invoices (customer, cents)
	         SELECT 'customer-' || g, g * 1000 FROM generate_series(1, 500) g`)

	before := psql(t, `SELECT count(*), coalesce(sum(cents), 0) FROM invoices`)
	if before != "500|125250000" {
		t.Fatalf("seed produced %q, want 500|125250000", before)
	}

	// Back it up, with the script the product generates.
	out, err := run(t, d, app, backup.BackupScript(backup.DefaultPolicy(), target), postgresEnv())
	if err != nil {
		t.Fatalf("backup failed: %v\n%s", err, out)
	}
	t.Logf("backup output:\n%s", out)

	// Now destroy it. Dropping the table rather than deleting the rows, so a
	// restore that silently did nothing cannot pass — the query afterwards
	// would error rather than return zero.
	psql(t, `DROP TABLE invoices`)
	if got := psql(t, `SELECT count(*) FROM pg_tables WHERE tablename = 'invoices'`); got != "0" {
		t.Fatalf("the table survived the drop: %q", got)
	}

	// Restore.
	out, err = run(t, d, app, backup.RestoreScript(target, "latest"), postgresEnv())
	if err != nil {
		t.Fatalf("restore failed: %v\n%s", err, out)
	}
	t.Logf("restore output:\n%s", out)

	after := psql(t, `SELECT count(*), coalesce(sum(cents), 0) FROM invoices`)
	if after != before {
		t.Fatalf("data after restore = %q, want %q", after, before)
	}

	// And the sequence came back with it. A dump that restored rows but not the
	// sequence leaves a table that reads correctly and fails on the next
	// insert, which is a restore that looks like a success for as long as
	// nobody writes.
	psql(t, `INSERT INTO invoices (customer, cents) VALUES ('after-restore', 1)`)
	if got := psql(t, `SELECT count(*) FROM invoices`); got != "501" {
		t.Fatalf("insert after restore left %q rows, want 501", got)
	}
}

// The other kind of target. A volume is copied as files rather than dumped, and
// restored with --delete — so this checks the property that flag is there for:
// a file written after the snapshot is gone after the restore.
//
// Without --delete a restore merges the snapshot over whatever is present, and
// the leftovers are exactly the corruption somebody is restoring to escape.
func TestVolumeRestoreRemovesWhatTheSnapshotDoesNotHave(t *testing.T) {
	d := testDestination(t)
	app := "vol-app"
	target := backup.Target{Kind: backup.KindVolume, Name: "data"}

	vol := "oz-backup-vol-" + fmt.Sprint(time.Now().UnixNano())
	mustDocker(t, "volume", "create", vol)
	t.Cleanup(func() { _ = exec.Command("docker", "volume", "rm", "-f", vol).Run() })

	// A freshly provisioned volume belongs to root, and the task runs as 65532.
	// In a cluster the pod's fsGroup does this; Docker has no equivalent, so
	// the test does it explicitly rather than running the task as root and
	// proving nothing about the case that ships.
	mustDocker(t, "run", "--rm", "-v", vol+":/data", "--user", "0:0",
		"alpine:3.22", "chown", "-R", "65532:65532", "/data")

	// Seed the volume with a file that belongs in the snapshot.
	inVolume(t, vol, "mkdir -p /data/sub && echo 'keep me' > /data/sub/wanted.txt")

	out, err := runWithVolume(t, d, app, vol,
		backup.BackupScript(backup.DefaultPolicy(), target), nil)
	if err != nil {
		t.Fatalf("volume backup failed: %v\n%s", err, out)
	}

	// Now corrupt it the way a bad deploy would: change the wanted file and add
	// one the snapshot has never seen.
	inVolume(t, vol, "echo 'corrupted' > /data/sub/wanted.txt && echo 'junk' > /data/unwanted.txt")

	out, err = runWithVolume(t, d, app, vol, backup.RestoreScript(target, "latest"), nil)
	if err != nil {
		t.Fatalf("volume restore failed: %v\n%s", err, out)
	}
	t.Logf("restore output:\n%s", out)

	if got := inVolume(t, vol, "cat /data/sub/wanted.txt"); !strings.Contains(got, "keep me") {
		t.Errorf("restored file = %q, want the snapshot's contents", got)
	}
	if got := inVolume(t, vol, "ls /data/unwanted.txt 2>&1 || true"); !strings.Contains(got, "No such file") {
		t.Errorf("a file the snapshot does not contain survived the restore: %q", got)
	}
}

// runWithVolume runs a generated script with a Docker volume mounted where a
// task would mount a claim.
func runWithVolume(
	t *testing.T, d backup.Destination, app, vol, script string, extraEnv map[string]string,
) (string, error) {
	t.Helper()

	args := []string{
		"run", "--rm",
		"--network", envOr(envNetwork, "oz-backup-test"),
		"--user", "65532:65532",
		"-v", vol + ":" + backup.MountPath(),
	}
	env := backup.Env(d, app)
	for k, v := range backup.Secrets(d) {
		env[k] = v
	}
	for k, v := range extraEnv {
		env[k] = v
	}
	for k, v := range env {
		args = append(args, "-e", k+"="+v)
	}
	args = append(args, os.Getenv(envImage), "-c", script)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	return string(out), err
}

// inVolume runs a shell command against the volume, as the task's uid so the
// files it writes are the ones the backup will read.
func inVolume(t *testing.T, vol, script string) string {
	t.Helper()
	out, err := exec.Command("docker", "run", "--rm",
		"-v", vol+":/data", "--user", "65532:65532",
		"alpine:3.22", "sh", "-c", script).CombinedOutput()
	if err != nil && !strings.Contains(string(out), "No such file") {
		t.Fatalf("volume command %q: %v\n%s", script, err, out)
	}
	return string(out)
}

func mustDocker(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("docker %v: %v\n%s", args, err, out)
	}
}

// A backup is only worth having if it can be found again. This checks the
// listing the dashboard reads is parseable and carries the tags a restore
// selects on — the two properties that turn a repository into something a
// person can restore from rather than a directory of opaque blobs.
func TestSnapshotsAreListedWithTheirTarget(t *testing.T) {
	d := testDestination(t)
	app := envOr(envPGHost, "oz-pg-src")
	target := postgresTarget()

	out, err := run(t, d, app, backup.BackupScript(backup.DefaultPolicy(), target), postgresEnv())
	if err != nil {
		t.Fatalf("backup failed: %v\n%s", err, out)
	}

	listing, err := run(t, d, app, backup.SnapshotsScript(), nil)
	if err != nil {
		t.Fatalf("listing failed: %v\n%s", err, listing)
	}

	snaps, err := backup.ParseSnapshotsForTest(listing)
	if err != nil {
		t.Fatalf("could not parse the listing: %v\noutput was:\n%s", err, listing)
	}
	if len(snaps) == 0 {
		t.Fatal("no snapshots listed after taking one")
	}

	s := snaps[0]
	if s.ID == "" {
		t.Error("snapshot has no id, so nothing could ask to restore it")
	}
	if s.Target() != "ozymandis" {
		t.Errorf("snapshot target = %q, want ozymandis — a restore selects on this",
			s.Target())
	}
	if s.Kind() != backup.KindPostgres {
		t.Errorf("snapshot kind = %q, want postgres", s.Kind())
	}
	if s.Time.IsZero() {
		t.Error("snapshot has no timestamp, so nobody could tell which one to restore")
	}
}

// The failure that makes a backup worthless: pg_dump dies partway, restic
// faithfully stores the truncated output, and the job exits zero. `set -o
// pipefail` in the generated script is what prevents it, and this is the test
// that the line is actually there and working.
func TestATruncatedDumpFailsTheBackup(t *testing.T) {
	d := testDestination(t)
	app := envOr(envPGHost, "oz-pg-src")

	// A target pointed at a database that does not exist, so pg_dump exits
	// non-zero having written nothing to the pipe. Without pipefail, restic
	// stores an empty dump and the script succeeds.
	target := postgresTarget()
	env := postgresEnv()
	env["PGDATABASE"] = "no-such-database"

	out, err := run(t, d, app, backup.BackupScript(backup.DefaultPolicy(), target), env)
	if err == nil {
		t.Fatalf("a backup whose dump failed was reported as successful:\n%s", out)
	}
	t.Logf("correctly failed:\n%s", out)
}
