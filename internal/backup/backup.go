// Package backup keeps copies of an install's data somewhere the install
// cannot destroy.
//
// The problem it exists for is specific. K3s stores volumes on local-path by
// default: an app's data is a directory on one node's disk, unreplicated, with
// nothing between it and that disk failing. Everything else in this engine can
// be rebuilt from configuration — the apps, the routing, the certificates. The
// contents of a volume cannot.
//
// Two things follow, and both are load-bearing:
//
//  1. Backups go off the machine, always. There is no local destination and
//     adding one would be adding the option most people would pick and the one
//     that protects against nothing.
//  2. A backup nobody has restored from is a hypothesis. This package makes
//     restoring an ordinary operation rather than an emergency procedure —
//     which is the only way anybody finds out the backups were wrong while it
//     is still cheap to find out.
package backup

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// Errors callers distinguish.
var (
	// ErrNoDestination means nowhere has been configured to write to. Not a
	// failure — it is how an install with backups switched off is detected, and
	// the surfaces that need one say so rather than erroring.
	ErrNoDestination = errors.New("backup: no destination configured")

	// ErrNoPolicy means this app is not backed up.
	ErrNoPolicy = errors.New("backup: app has no backup policy")

	// ErrNothingToBackUp means the app holds no data this can copy. A
	// stateless web app is the ordinary case, and it is worth saying plainly
	// rather than scheduling a job that would back up an empty directory every
	// night and report success.
	ErrNothingToBackUp = errors.New("backup: app has no storage to back up")

	// ErrUnsupportedRestore means the app's shape does not have a restore path
	// this can carry out unattended.
	ErrUnsupportedRestore = errors.New("backup: cannot restore this app automatically")
)

// Destination is the S3-compatible storage backups are written to.
//
// Credentials are held here in the clear only in memory, and only for as long
// as it takes to build a task from them. At rest they are sealed with the
// install's secret key — see store.go.
type Destination struct {
	// Endpoint is the S3 endpoint, such as
	// https://<account>.r2.cloudflarestorage.com. Required even for AWS,
	// because there is no default that is right for the other providers.
	Endpoint string

	Bucket string
	Prefix string
	Region string

	AccessKeyID     string
	SecretAccessKey string

	// RepoPassword encrypts the repository. Restic encrypts client-side, so
	// this is what stands between the storage provider and the contents — and
	// losing it loses every snapshot with no recovery path.
	RepoPassword string
}

// Repository is the restic repository URL for an app.
//
// One repository per app rather than one per install. Restic locks a
// repository while writing, so a shared one would serialise every app's backup
// behind every other's — and worse, `restic forget` applies a retention policy
// to a whole repository, so one app's retention would silently prune another's
// snapshots.
func (d Destination) Repository(app string) string {
	// s3:<endpoint>/<bucket>/<prefix>/<app>, which is restic's own form for an
	// S3-compatible backend.
	path := strings.TrimSuffix(d.Endpoint, "/") + "/" + d.Bucket
	if p := strings.Trim(d.Prefix, "/"); p != "" {
		path += "/" + p
	}
	return "s3:" + path + "/" + app
}

// bucketRE is the intersection of what the S3-compatible providers accept:
// lowercase, digits, dots and hyphens, starting and ending alphanumeric.
var bucketRE = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

// Validate checks a destination somebody typed.
//
// Checked here rather than discovered by the first backup, because the first
// backup runs at 3am in a cluster, and its failure is a Job condition nobody is
// looking at. A destination that cannot work should be refused by the form
// that accepted it.
func (d Destination) Validate() error {
	u, err := url.Parse(d.Endpoint)
	if err != nil || u.Host == "" {
		return fmt.Errorf("backup: endpoint %q must be a URL like "+
			"https://s3.example.com", d.Endpoint)
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("backup: endpoint scheme %q must be http or https", u.Scheme)
	}
	// The storage credential and every byte of every backup cross this
	// connection, so plain HTTP is refused — except where it cannot leave the
	// network it started on.
	//
	// The exception is not a convenience. A self-hosted install pointing at
	// MinIO on the other side of a private network, or in the same cluster, is
	// a legitimate destination and one that has no certificate to present.
	// Refusing it outright would push people to the only other thing that works
	// — a public bucket over the internet — which is a worse answer to a
	// question about keeping data safe.
	if u.Scheme == "http" && isPublic(u.Hostname()) {
		return fmt.Errorf("backup: endpoint %q must be https — the storage "+
			"credential and the backups themselves cross this connection. Plain "+
			"http is allowed only for a private address or a cluster-internal name",
			d.Endpoint)
	}

	if !bucketRE.MatchString(d.Bucket) {
		return fmt.Errorf("backup: %q is not a valid bucket name", d.Bucket)
	}
	if strings.Contains(d.Prefix, "..") {
		return fmt.Errorf("backup: prefix %q must not contain ..", d.Prefix)
	}
	if d.AccessKeyID == "" {
		return errors.New("backup: an access key id is required")
	}
	if d.SecretAccessKey == "" {
		return errors.New("backup: a secret access key is required")
	}

	// The one credential with no recovery path, so it is the one with a length
	// floor. A short repository password is not meaningfully protecting
	// anything from the storage provider it is meant to protect it from.
	if len(d.RepoPassword) < 12 {
		return errors.New("backup: the repository password must be at least 12 characters — " +
			"it is the only thing encrypting these backups, and it cannot be changed " +
			"after the first one without starting over")
	}
	return nil
}

// isPublic reports whether a host could route off the local network.
//
// Deliberately conservative in the direction that matters: anything this cannot
// prove is private is treated as public, so a name it does not recognise
// requires https. The failure mode of guessing wrong the other way is
// credentials in clear text across the internet.
func isPublic(host string) bool {
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsLoopback() &&
			!ip.IsPrivate() &&
			!ip.IsLinkLocalUnicast() &&
			!ip.IsUnspecified()
	}

	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if host == "localhost" {
		return false
	}

	// A single label — "minio" — is a Kubernetes Service name resolved by the
	// cluster's own DNS. It cannot be a public name: those need a dot.
	if !strings.Contains(host, ".") {
		return false
	}

	// The suffixes a cluster resolves internally. Matched on the label
	// boundary, never as a substring: "notcluster.local" ends with
	// "cluster.local" and is a different name entirely.
	for _, suffix := range []string{
		".svc", ".svc.cluster.local", ".cluster.local", ".local", ".internal",
	} {
		if strings.HasSuffix(host, suffix) {
			return false
		}
	}
	return true
}

// Policy is what gets backed up for one app, and how often.
type Policy struct {
	AppID   string
	AppName string
	Enabled bool

	// Schedule is a five-field cron expression, in the cluster's timezone.
	Schedule string

	KeepDaily   int
	KeepWeekly  int
	KeepMonthly int
}

// DefaultSchedule is 03:17 daily.
//
// The odd minute is deliberate. Everything scheduled on the hour contends with
// everything else scheduled on the hour, and on a single small node that
// contention is the difference between a backup that finishes and one that is
// still running when the next is due.
const DefaultSchedule = "17 3 * * *"

// DefaultPolicy is what an app gets when somebody switches backups on without
// saying more.
//
// A week of dailies, a month of weeklies, half a year of monthlies. Chosen so
// that the two failures people actually have are both covered: noticing
// yesterday that something broke, and noticing in March that a table has been
// wrong since December.
func DefaultPolicy() Policy {
	return Policy{
		Enabled:     true,
		Schedule:    DefaultSchedule,
		KeepDaily:   7,
		KeepWeekly:  4,
		KeepMonthly: 6,
	}
}

// cronFieldRE is the shape of one field of a cron expression. Kubernetes parses
// it for real; this rejects what is obviously not one, before it reaches a
// cluster that reports the problem as a CronJob it could not create.
var cronFieldRE = regexp.MustCompile(`^[-0-9*/,A-Za-z?]+$`)

// Validate checks a policy.
func (p Policy) Validate() error {
	fields := strings.Fields(p.Schedule)
	if len(fields) != 5 {
		return fmt.Errorf("backup: %q is not a five-field cron expression, "+
			"such as %q", p.Schedule, DefaultSchedule)
	}
	for _, f := range fields {
		if !cronFieldRE.MatchString(f) {
			return fmt.Errorf("backup: %q is not a valid part of a cron expression", f)
		}
	}

	if p.KeepDaily < 0 || p.KeepWeekly < 0 || p.KeepMonthly < 0 {
		return errors.New("backup: retention counts must not be negative")
	}

	// The check that matters most in this file. `restic forget` with every
	// count at zero matches no snapshot as worth keeping and deletes all of
	// them — including the one the job just took. A policy that reads as "keep
	// nothing" behaves as "destroy the backups", so it is refused rather than
	// saved.
	if p.KeepDaily+p.KeepWeekly+p.KeepMonthly == 0 {
		return errors.New("backup: keep at least one daily, weekly or monthly " +
			"snapshot — keeping none would delete every backup on the next run")
	}
	return nil
}

// Kind is what a backup target holds, which decides how it is copied.
type Kind string

const (
	// KindPostgres is a database, copied with pg_dump over the network rather
	// than by reading its files.
	//
	// The distinction is not a preference. Postgres's data directory is only
	// consistent on disk between checkpoints; copying it from a running server
	// yields files that restore into a database that may or may not start, and
	// finding out which happens at restore time. A dump is a transaction.
	KindPostgres Kind = "postgres"

	// KindVolume is a filesystem, copied by reading the mounted claim.
	//
	// Read-only, and still not a consistent snapshot of an application that is
	// writing to it — the storage layer offers nothing better here, and saying
	// so is more useful than implying otherwise.
	KindVolume Kind = "volume"
)

// Target is one thing to copy.
type Target struct {
	Kind Kind

	// Name identifies the target within the app: a volume's name, or the
	// database name. It becomes part of the restic tag, so a restore can ask
	// for the right one.
	Name string

	// MountPath is where the volume is mounted, for KindVolume.
	MountPath string

	// Service and Port are how a KindPostgres target is reached.
	Service string
	Port    int32
}

// Snapshot is one point-in-time copy, as restic reports it.
type Snapshot struct {
	ID   string
	Time time.Time

	// Tags carry the target this snapshot is of, which is how a restore of a
	// multi-volume app picks the right one.
	Tags []string

	// SizeBytes is what restic recorded for the files in this snapshot. Zero
	// where the repository was written by a restic old enough not to record it.
	SizeBytes int64
}

// Target returns the tag naming what this snapshot is of.
func (s Snapshot) Target() string {
	for _, t := range s.Tags {
		if name, ok := strings.CutPrefix(t, targetTagPrefix); ok {
			return name
		}
	}
	return ""
}

// Kind returns the tag naming how this snapshot was taken.
func (s Snapshot) Kind() Kind {
	for _, t := range s.Tags {
		if k, ok := strings.CutPrefix(t, kindTagPrefix); ok {
			return Kind(k)
		}
	}
	return ""
}

// Tag prefixes. Restic tags are opaque strings, so these are what give them
// structure — and they are read back on restore, which is why they are
// constants rather than formatted at each call site.
const (
	targetTagPrefix = "target:"
	kindTagPrefix   = "kind:"
)

// Tags returns the restic tags for a target.
func (t Target) Tags() []string {
	return []string{targetTagPrefix + t.Name, kindTagPrefix + string(t.Kind)}
}
