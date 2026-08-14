package web

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/a-h/templ"
	"github.com/google/uuid"

	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/backup"
	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// A gallery of every visual state the dashboard can be in.
//
// Two jobs. It renders documentation images without needing a cluster or a
// database, and it is the scaffolding for visual regression: the states that
// are hardest to reach by clicking — a degraded workload, a failed deploy, a
// node at 95% — are exactly the ones that silently rot, because nobody sees
// them until a customer does.
//
// Writes files only when OZYMANDIS_GALLERY_OUT is set, so the normal test run stays
// a normal test run:
//
//	OZYMANDIS_GALLERY_OUT=/tmp/gallery go test ./internal/web -run Gallery
func TestGallery(t *testing.T) {
	out := os.Getenv("OZYMANDIS_GALLERY_OUT")
	if out == "" {
		t.Skip("set OZYMANDIS_GALLERY_OUT to write gallery HTML")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	writeGalleryAssets(t, out)

	for _, g := range galleryPages() {
		path := filepath.Join(out, g.file)
		f, err := os.Create(path)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		slots := DefaultSlots{}.Slots(context.Background(),
			httptest.NewRequest("GET", g.path, nil))
		slots.Breadcrumb = g.crumbs
		if err := Layout(slots, g.page).Render(context.Background(), f); err != nil {
			f.Close()
			t.Fatalf("render %s: %v", g.file, err)
		}
		f.Close()
		t.Logf("wrote %s", path)
	}
}

// writeGalleryAssets copies the embedded assets next to the rendered HTML.
//
// The pages ask for /assets/css/app.css, and nothing answers that when the
// output is opened on its own. Without this the gallery renders unstyled, which
// is worse than not rendering at all: an unstyled page still looks like a page,
// so the states this exists to check are missing without anything saying so.
//
// Written from the same embedded FS the server serves, so what is reviewed here
// is what ships rather than whatever happens to be in the working tree.
func writeGalleryAssets(t *testing.T, out string) {
	t.Helper()

	count := 0
	err := fs.WalkDir(assetsFS, "assets", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := assetsFS.ReadFile(p)
		if err != nil {
			return err
		}
		// p is already "assets/...", so joining it onto out reproduces the
		// layout the /assets URLs expect when out is the document root.
		dst := filepath.Join(out, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		count++
		return os.WriteFile(dst, b, 0o644)
	})
	if err != nil {
		t.Fatalf("write gallery assets: %v", err)
	}
	t.Logf("wrote %d assets", count)
}

type galleryPage struct {
	file   string
	path   string
	crumbs []Crumb
	page   templ.Component
}

// section renders a labelled band so several states can share one image.
func section(title, note string, body templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		fmt.Fprintf(w, `<div class="mb-8"><div class="mb-2 flex items-baseline gap-3">`+
			`<span class="micro">%s</span>`+
			`<span class="text-[11.5px] text-muted-foreground">%s</span></div>`,
			templ.EscapeString(title), templ.EscapeString(note))
		if err := body.Render(ctx, w); err != nil {
			return err
		}
		_, err := w.Write([]byte(`</div>`))
		return err
	})
}

func stack(parts ...templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		for _, p := range parts {
			if err := p.Render(ctx, w); err != nil {
				return err
			}
		}
		return nil
	})
}

func galleryPages() []galleryPage {
	now := time.Now()

	mk := func(name, image string, replicas, ready int32, phase orchestrator.Phase,
		known bool, msg string) app.App {
		return app.App{
			ID: uuid.New(), OwnerID: "owner-local", Name: name,
			Namespace: app.Namespace("owner-local", name),
			Image:     image, Replicas: replicas, Port: 8080,
			Variables: varsFrom(map[string]string{"LOG_LEVEL": "info", "NODE_ENV": "production"}),
			CreatedAt: now.Add(-72 * time.Hour), UpdatedAt: now.Add(-11 * time.Minute),
			StatusKnown: known,
			Status: orchestrator.AppStatus{
				Phase: phase, Desired: replicas, Ready: ready,
				Available: ready, Message: msg,
			},
		}
	}

	running := mk("web", "nginx:alpine", 2, 2, orchestrator.PhaseRunning, true, "")
	degraded := mk("api", "ghcr.io/codeblocktz/api:v2.4.1", 3, 1,
		orchestrator.PhaseDegraded, true, "1/3 replicas ready")
	pending := mk("worker", "ghcr.io/codeblocktz/worker:v1", 2, 0,
		orchestrator.PhasePending, true, "")
	stopped := mk("batch", "ghcr.io/codeblocktz/batch:v3", 0, 0,
		orchestrator.PhaseStopped, true, "")
	unknown := mk("cache", "valkey/valkey:8-alpine", 1, 0, "", false, "")
	failing := mk("mailer", "ghcr.io/codeblocktz/mailer:v9", 1, 0,
		orchestrator.PhasePending, true,
		`Back-off pulling image "ghcr.io/codeblocktz/mailer:v9": ErrImagePull`)

	// Hostnames on some apps and not others, over both schemes: an install
	// with no ACME resolver is a state that must look right too.
	running.Host, running.TLS = "web.apps.example.com", true
	degraded.Host = "api.apps.example.com"

	allApps := []app.App{running, degraded, pending, stopped, unknown, failing}

	pods := []orchestrator.PodInfo{
		{Name: "web-7d9f4b6c85-2xk9p", Namespace: "ozymandis-a1b2", Phase: "Running",
			Node: "ozymandis-cp", Ready: 1, Total: 1, CreatedAt: now.Add(-4 * time.Hour)},
		{Name: "api-5c8b7d94f6-hq2mz", Namespace: "ozymandis-c3d4", Phase: "Running",
			Node: "ozymandis-w1", Ready: 1, Total: 2, Restarts: 4, CreatedAt: now.Add(-time.Hour)},
		{Name: "worker-64b9c7f8d2-pl4vn", Namespace: "ozymandis-e5f6", Phase: "Pending",
			Node: "", Ready: 0, Total: 1, CreatedAt: now.Add(-90 * time.Second)},
		{Name: "mailer-8f7d6c5b4a-zz91k", Namespace: "ozymandis-0718", Phase: "Failed",
			Node: "ozymandis-w1", Ready: 0, Total: 1, Restarts: 12, CreatedAt: now.Add(-20 * time.Minute)},
	}

	nodes := []orchestrator.NodeInfo{
		{
			Name: "ozymandis-cp", Ready: true, Roles: []string{"control-plane", "master"},
			Address: "10.0.0.4", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "arm64",
			CPUCapacityMillis: 4000, CPUUsedMillis: 760,
			MemCapacityBytes: 8 << 30, MemUsedBytes: 3<<30 + 512<<20,
			Pods: 23, PodCapacity: 110, UsageKnown: true, Pool: "system",
		},
		{
			Name: "ozymandis-w1", Ready: true, Roles: []string{"worker"},
			Address: "10.0.0.5", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "arm64",
			CPUCapacityMillis: 2000, CPUUsedMillis: 1640,
			MemCapacityBytes: 4 << 30, MemUsedBytes: 3<<30 + 300<<20,
			Pods: 31, PodCapacity: 110, UsageKnown: true, Pool: "apps",
		},
		{
			Name: "ozymandis-w2", Ready: false, Roles: []string{"worker"},
			Address: "10.0.0.6", Version: "v1.36.2+k3s1", OS: "linux", Architecture: "amd64",
			CPUCapacityMillis: 2000, CPUUsedMillis: 1960,
			MemCapacityBytes: 4 << 30, MemUsedBytes: 3<<30 + 900<<20,
			Pods: 8, PodCapacity: 110, UsageKnown: true, Unschedulable: true,
		},
	}

	summary := orchestrator.ClusterSummary{
		Nodes: 3, NodesReady: 2, Pods: 62, PodCapacity: 330,
		CPUUsedMillis: 4360, CPUCapacityMillis: 8000,
		MemUsedBytes: 10<<30 + 700<<20, MemCapacityBytes: 16 << 30,
		Volumes: 13, UsageKnown: true,
	}

	deployments := []app.Deployment{
		{ID: uuid.New(), Image: "nginx:alpine", Revision: "initial", Status: "running",
			StartedAt: now.Add(-12 * time.Minute)},
		{ID: uuid.New(), Image: "nginx:1.27", Revision: "redeploy", Status: "succeeded",
			StartedAt: now.Add(-6 * 24 * time.Hour)},
		{ID: uuid.New(), Image: "nginx:1.26", Revision: "scale:4", Status: "failed",
			Message: "readiness probe never passed", StartedAt: now.Add(-7 * 24 * time.Hour)},
		{ID: uuid.New(), Image: "nginx:1.25", Revision: "redeploy", Status: "cancelled",
			StartedAt: now.Add(-21 * 24 * time.Hour)},
	}

	return []galleryPage{
		{
			file: "states-apps.html", path: "/apps",
			crumbs: []Crumb{{Label: "Apps"}},
			page: stack(
				section("App states", "running · degraded · pending · stopped · unknown · image pull failure",
					panelWrap(appRows(allApps))),
				section("Empty state", "no workloads yet", panelWrap(emptyApps())),
				section("Cluster unreachable", "records still render; live status does not",
					clusterCallout(`Get "https://10.0.0.4:6443/version": dial tcp: i/o timeout`)),
			),
		},
		{
			file: "states-detail.html", path: "/apps/web",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "web"}},
			page: AppDetail(AppDetailData{
				App: running, Siblings: allApps, Tab: "",
				Pods: pods[:2], Deployments: deployments,
			}),
		},
		{
			file: "states-detail-degraded.html", path: "/apps/api",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "api"}},
			page: AppDetail(AppDetailData{
				App: degraded, Siblings: allApps, Tab: "metrics", Pods: pods[1:3],
				Deployments: deployments[2:],
			}),
		},
		// The states that rot fastest here: a schedule that has never run, one
		// whose last run failed, and the two ways there is nothing to
		// configure. None of them is reachable in a dev environment without a
		// destination, a cluster, and a night passing.
		{
			file: "states-backups.html", path: "/apps/db/backups",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "db"}},
			page: stack(
				section("Scheduled, with runs", "the ordinary state — one failure among successes",
					AppBackups(BackupsData{
						App: "db",
						Data: app.Backups{
							Available: true, Configured: true, HasPolicy: true,
							Policy:  backup.DefaultPolicy(),
							Targets: []backup.Target{{Kind: backup.KindPostgres, Name: "ozymandis"}},
							Runs: []orchestrator.RunInfo{
								{Name: "db-backup-29011601", StartedAt: now.Add(-3 * time.Hour),
									Succeeded: boolp(true)},
								{Name: "db-backup-29010161", StartedAt: now.Add(-27 * time.Hour),
									Succeeded: boolp(false)},
								{Name: "db-backup-29008721", StartedAt: now.Add(-51 * time.Hour),
									Succeeded: boolp(true)},
							},
						},
					})),
				section("Never run", "a schedule nobody has seen succeed is not yet a backup",
					AppBackups(BackupsData{
						App: "db",
						Data: app.Backups{
							Available: true, Configured: true, HasPolicy: true,
							Policy:  backup.DefaultPolicy(),
							Targets: []backup.Target{{Kind: backup.KindPostgres, Name: "ozymandis"}},
						},
					})),
				section("No destination", "the install has nowhere to write to",
					AppBackups(BackupsData{
						App: "db",
						Data: app.Backups{
							Available: true,
							Because:   "no backup destination is configured for this install",
						},
					})),
				section("Nothing to copy", "a stateless app",
					AppBackups(BackupsData{
						App: "web",
						Data: app.Backups{
							Available: true, Configured: true,
							Because: "this app has no database and no volumes, so there is nothing to copy",
						},
					})),
				section("A failed run's output", "the log is the only place the reason appears",
					AppBackups(BackupsData{
						App: "db",
						Data: app.Backups{
							Available: true, Configured: true, HasPolicy: true,
							Policy:  backup.DefaultPolicy(),
							Targets: []backup.Target{{Kind: backup.KindPostgres, Name: "ozymandis"}},
						},
						Error: "The backup did not finish. The log below says why.",
						Log: "Initialising the repository at s3:https://x/ozymandis-backups/db\n" +
							"pg_dump: error: connection to server at \"db\" (10.43.0.9), " +
							"port 5432 failed: FATAL:  password authentication failed\n",
					})),
			),
		},
		{
			file: "states-backup-destination.html", path: "/settings/backups",
			crumbs: []Crumb{{Label: "Settings", Href: "/settings"}, {Label: "Backups"}},
			page: stack(
				section("Not configured", "the first time somebody opens it",
					BackupDestination(DestinationData{})),
				section("Configured", "secrets are never shown again",
					BackupDestination(DestinationData{
						Configured: true,
						Destination: backup.Destination{
							Endpoint:    "https://account.r2.cloudflarestorage.com",
							Bucket:      "ozymandis-backups",
							Prefix:      "prod",
							Region:      "auto",
							AccessKeyID: "0a1b2c3d4e5f",
						},
					})),
				section("No encryption key", "credentials cannot be sealed, so they are refused",
					BackupDestination(DestinationData{KeyRequired: true})),
			),
		},
		{
			file: "states-cluster.html", path: "/cluster/nodes",
			crumbs: []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Cluster"}},
			page: Cluster(ClusterData{
				Tab: "nodes", OK: true, Summary: summary, Nodes: nodes,
			}),
		},
		{
			file: "states-pods.html", path: "/cluster/pods",
			crumbs: []Crumb{{Label: "Infrastructure", Href: "/cluster/nodes"}, {Label: "Cluster"}},
			page: Cluster(ClusterData{
				Tab: "pods", OK: true, Summary: summary, Pods: pods,
			}),
		},
		{
			file: "states-form.html", path: "/apps/new",
			crumbs: []Crumb{{Label: "Apps", Href: "/apps"}, {Label: "New"}},
			page: NewApp(NewAppData{
				Error: `app: name already in use`,
				Form: NewAppForm{
					Name: "web", Image: "nginx:alpine", Port: "8080", Replicas: "2",
					Env: "LOG_LEVEL=info\nNODE_ENV=production",
				},
			}),
		},
		{
			file: "states-settings.html", path: "/settings",
			crumbs: []Crumb{{Label: "Settings"}},
			page: Settings(SettingsData{
				OwnerID: "owner-local", OwnerName: "Eric", Authenticated: true,
				Version: "v0.1.0", ClusterOK: true,
				AppDomain: "apps.example.com", CertResolver: "letsencrypt",
			}),
		},
		{
			file: "states-sign-in.html", path: "/sign-in",
			page: stack(
				section("Sign in", "the form, and the same form after a refusal",
					stack(
						SignIn(SignInData{}),
						SignIn(SignInData{
							Username: "batman",
							Error:    "Incorrect username or password.",
						}),
					)),
			),
		},
		{
			file: "states-overview.html", path: "/",
			crumbs: []Crumb{{Label: "Overview"}},
			page: Overview(OverviewData{
				OwnerName: "Eric", ClusterOK: true, AppCount: 6,
				Summary: summary, Apps: allApps, Activity: activityBusy(),
			}),
		},
		{
			file: "states-activity.html", path: "/",
			crumbs: []Crumb{{Label: "Overview"}, {Label: "Deploy activity"}},
			page: stack(
				section("Typical", "a month of ordinary deploys, one column per day",
					deployActivity(activityBusy())),
				section("Nothing yet", "a fresh install — the panel stays and says so, "+
					"rather than vanishing and leaving the reader to wonder",
					deployActivity(activityEmpty())),
				section("One busy day", "a spike twenty times the median: the scale "+
					"follows the data, so the quiet days must stay visible",
					deployActivity(activitySpike())),
				section("A bad afternoon", "failures sit at the top of each column, "+
					"where the eye lands",
					deployActivity(activityFailing())),
			),
		},
	}
}

// activityDays builds a window ending today, so the axis labels read as real
// dates rather than as an epoch.
func activityDays(n int) []app.DeployDay {
	today := time.Now().UTC().Truncate(24 * time.Hour)
	days := make([]app.DeployDay, n)
	for i := range days {
		days[i].Day = today.AddDate(0, 0, -(n - 1 - i))
	}
	return days
}

// total rolls the per-day counts up, so a gallery fixture cannot state a total
// that its own columns contradict.
func activityFrom(days []app.DeployDay) app.DeployActivity {
	a := app.DeployActivity{Days: days}
	for _, d := range days {
		a.Succeeded += d.Succeeded
		a.Failed += d.Failed
		a.Cancelled += d.Cancelled
		a.Running += d.Running
	}
	return a
}

func activityBusy() app.DeployActivity {
	days := activityDays(30)
	pattern := []int{0, 2, 1, 0, 4, 3, 1, 0, 0, 5, 2, 1, 3, 0, 2}
	for i := range days {
		n := pattern[i%len(pattern)]
		days[i].Succeeded = n
		if i%9 == 4 && n > 0 {
			days[i].Succeeded = n - 1
			days[i].Failed = 1
		}
	}
	days[len(days)-1].Running = 1
	return activityFrom(days)
}

func activityEmpty() app.DeployActivity {
	return activityFrom(activityDays(30))
}

func activitySpike() app.DeployActivity {
	days := activityDays(30)
	for i := range days {
		if i%3 == 0 {
			days[i].Succeeded = 1
		}
	}
	days[20].Succeeded = 20
	return activityFrom(days)
}

func activityFailing() app.DeployActivity {
	days := activityDays(30)
	for i := range days {
		switch {
		case i > 24:
			days[i].Failed = 3
			days[i].Succeeded = 1
		case i%2 == 0:
			days[i].Succeeded = 2
		}
	}
	days[27].Cancelled = 2
	return activityFrom(days)
}

// panelWrap puts a component inside the standard panel chrome, so gallery
// sections match how the component appears in the product.
func panelWrap(inner templ.Component) templ.Component {
	return templ.ComponentFunc(func(ctx context.Context, w io.Writer) error {
		if _, err := w.Write([]byte(`<div class="panel">`)); err != nil {
			return err
		}
		if err := inner.Render(ctx, w); err != nil {
			return err
		}
		_, err := w.Write([]byte(`</div>`))
		return err
	})
}

// boolp is for RunInfo.Succeeded, which is a pointer because nil is a third
// state: the run has not finished, and reporting that as failure or as success
// would both be wrong.
func boolp(b bool) *bool { return &b }
