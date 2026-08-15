# Ozymandis

A self-hosted PaaS for Kubernetes. Deploy your app to your own server, with
real Kubernetes underneath rather than a Docker wrapper, and an all-Go control
plane you can read in an afternoon.

Runs on K3s, so a $5 VPS is enough to start.

> [!WARNING]
> **Not ready for production.** Ozymandis is under active development. Interfaces,
> the database schema, and behaviour all still change without notice, there has
> been no tagged release yet, and none of it has run anywhere long enough to
> have earned your trust.
>
> Run it on something you can afford to lose, and keep backups you have actually
> restored from. Do not put it in front of anything whose downtime or data loss
> would matter.

**What it does today.** You can deploy from a container image or from a Git
repository, give it a domain, attach storage, and watch it from the dashboard.
See [What works today](#what-works-today).

## Why

Existing self-hosted PaaS options mostly wrap Docker Compose or Swarm. That
works until you want health probes, rolling updates, or a second node — at
which point you are fighting the abstraction instead of shipping.

Kubernetes already solves those problems. K3s makes it small enough to run on a
single cheap box. Ozymandis is a control plane over it that stays out of the way:
every workload is an ordinary Deployment, so `kubectl` keeps working and
nothing you build here is locked in.

## What works today

**Shipping an app**

| | |
|---|---|
| Deploy a container image, with env vars and replicas | ✅ |
| Override the image's command, so one image runs as several apps | ✅ |
| The port an app declares reaches it as `$PORT`, which is what buildpacks bind | ✅ |
| Build and deploy from a Git repository, using buildpacks | ✅ |
| Builds run as an isolated Job, with the log kept | ✅ |
| A built-in registry for the images those builds produce | ✅ |
| Scale, redeploy, delete | ✅ |
| Liveness and readiness probes | ✅ |
| Deployment history, with a log per deployment | ✅ |
| A release command, run against the new image before traffic shifts | ✅ |
| Deploy on push, with an HMAC-verified webhook | ✅ |
| A deploy key per app, so private repositories clone | ✅ |
| Polling for installs GitHub cannot reach | ✅ |
| A shell in a running container, from the CLI | ✅ |
| A JSON API and an `oz` CLI over it | ✅ |
| App logs, and per-request HTTP logs you can search and page | ✅ |
| Live logs, streamed as the container writes them | ✅ |
| Persistent volumes, mounted and expandable | ✅ |
| Secrets sealed at rest, kept out of the app record | ✅ |
| Scheduled backups off the machine, encrypted, with retention | ✅ |
| Restore from a snapshot, from the dashboard | ✅ |
| Deploy a wired stack from a template in one action | ✅ |

**Reaching it**

| | |
|---|---|
| A hostname per app, served the moment it starts | ✅ |
| Custom domains people bring, with verification | ✅ |
| TLS on every hostname, issued for that name alone and renewed automatically | ✅ |

**Running the cluster**

| | |
|---|---|
| Live workload status read from the cluster | ✅ |
| Cluster view — nodes, pods, volumes, events, utilisation | ✅ |
| Add a node, then cordon, drain, or remove one | ✅ |
| Namespace provisioning with enforced security posture | ✅ |
| A project canvas apps can be arranged on | ✅ |

**Accounts and foundations**

| | |
|---|---|
| Username and password sign-in, sessions, sign-out everywhere | ✅ |
| A superuser who creates and removes every other account | ✅ |
| Teams with Owner / Admin / Member | ✅ |
| Identity seam — single owner, bearer token, or session | ✅ |
| Dashboard with pluggable chrome, light and dark | ✅ |
| Postgres schema + embedded migrations | ✅ |

Utilisation percentages need `metrics-server` in the cluster. Without it
everything else still works and those figures read `—` rather than zero.

## Install on a VPS

> **Not live yet.** The scripts below are written and tested, but the URLs do
> not serve anything while this repository is private: GitHub Pages is not
> available for private repositories on the free plan, and release assets need
> a token, so an anonymous `curl` gets a 404 from both. They start working the
> day the repository goes public and the first tag is pushed — no change to the
> scripts is needed. Until then, use [Quick start](#quick-start).

```bash
curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh
```

Debian or Ubuntu, amd64 or arm64. It installs K3s, Postgres, and Ozymandis as a
systemd service, then prints the dashboard URL and a token to sign in with.
About ninety seconds on a fresh box.

**Before there is a release** — which is where this repository stands today —
the same installer will take a binary you built yourself. Everything else it
does is unchanged:

```bash
make build                      # leaves bin/ozymandis and bin/oz
scp bin/ozymandis bin/oz install.sh root@your-box:/tmp/
ssh root@your-box 'sh /tmp/install.sh --binary /tmp/ozymandis'
```

`--binary` skips the download and, with it, the checksum: a file handed over on
the command line has no publisher to check it against, and a checksum you
compute over your own file proves only that the disk read it back. What is
checked instead is that the binary could run here at all — an ELF built for
another architecture installs perfectly, then dies at startup as `ozymandis
exited`, which reads as the service being broken rather than as the wrong file
having been copied. An `oz` sitting beside it is installed too, which is why
the `scp` above carries both. Add `--skip-k3s` on a box that already has a
cluster.

Postgres comes from the distribution's own `postgresql` package — **16** on
Ubuntu 24.04. The installer creates the role `ozymandis` and an *empty*
database, `ozymandis_command_center`; the schema is not its business. Ozymandis
migrates itself at startup, before it opens a pool, so the tables and columns
appear on first run and a hand-written schema would only collide with the
migrator.

**It does not install an ingress controller, and it does not set up TLS.** K3s
is installed with `--disable traefik`, so a fresh box has no edge at all: apps
you deploy are reachable over plain http, and nothing reports that as a fault.
Wiring the edge is a separate step, and deliberately so — the controller is one
piece of cluster infrastructure with one place that configures it, and two
things configuring one Traefik is how you get a controller that never schedules
because something else already holds `:80` and `:443`.

What that leaves you to do, once:

1. Install an ingress controller with an ACME resolver. Traefik with a Let's
   Encrypt resolver over TLS-ALPN-01 is what this is built against.
2. Set `OZYMANDIS_CERT_RESOLVER` to that resolver's name and restart.

Leave `OZYMANDIS_CERT_RESOLVER` empty until step 1 is done — a name matching no
resolver fails silently, which [Certificates](#certificates) explains in full.

To upgrade later:

```bash
curl -sSL https://kingzion24.github.io/ozymandis/upgrade.sh | sudo sh
```

That one only swaps the binary and restarts. If the new version does not come
up healthy it puts the old one back, so a bad release costs a restart rather
than an outage.

| | |
|---|---|
| Binary | `/usr/local/bin/ozymandis` |
| Config | `/etc/ozymandis/ozymandis.env` |
| Service | `systemctl status ozymandis` |
| Logs | `journalctl -u ozymandis -f` |

**Back up `/etc/ozymandis/ozymandis.env`.** It holds `OZYMANDIS_SECRET_KEY`, which seals
your stored secrets and cannot be regenerated — losing it loses them. Re-running
the installer preserves it, and every other generated value, on purpose.

The installer serves the dashboard over plain HTTP, so the token crosses the
network in the clear. Set `OZYMANDIS_BASE_URL` and `OZYMANDIS_APP_DOMAIN` in the config
and put it behind TLS before you rely on it; until then `ssh -L 8080:127.0.0.1:8080`
is the safer way in.

Both scripts are worth reading before you pipe them to a root shell —
[`install.sh`](install.sh) and [`upgrade.sh`](upgrade.sh) are exactly what those
URLs serve, published straight from this repository.

To remove it all:

```bash
sudo systemctl disable --now ozymandis
sudo rm -rf /etc/ozymandis /etc/systemd/system/ozymandis.service /usr/local/bin/ozymandis
sudo /usr/local/bin/k3s-uninstall.sh          # only if you want the cluster gone too
sudo -u postgres dropdb ozymandis_command_center && sudo -u postgres dropuser ozymandis
```

`ozymandis_command_center` is the database the installer creates; the role is
`ozymandis`. The Quick start below builds its own database by hand and names it
whatever its connection string says, so the two need not match.

## Quick start

For development, or to run against a cluster you already have.

Requirements: Go 1.26+, Postgres, and a kubeconfig pointing at a cluster.

```bash
git clone https://github.com/kingzion24/ozymandis.git
cd ozymandis

export OZYMANDIS_DATABASE_URL="postgres://ozymandis:ozymandis@localhost:5432/ozymandis?sslmode=disable"
export OZYMANDIS_KUBECONFIG="$HOME/.kube/config"
export OZYMANDIS_AUTH_TOKEN="$(openssl rand -hex 24)"   # omit only on a trusted network

make run
```

Then open <http://localhost:8080>.

Migrations run automatically at startup. If the cluster is unreachable Ozymandis
still boots and says so on the overview page, so you can fix your kubeconfig
without digging through logs.

### Configuration

| Variable | Default | Notes |
|---|---|---|
| `OZYMANDIS_DATABASE_URL` | — | **Required.** Postgres connection string |
| `OZYMANDIS_ADDR` | `:8080` | Listen address |
| `OZYMANDIS_KUBECONFIG` | `$KUBECONFIG` | Path to a kubeconfig |
| `OZYMANDIS_KUBE_IN_CLUSTER` | `false` | Use the mounted service account instead |
| `OZYMANDIS_AUTH_TOKEN` | — | Bearer token. Unset, and with no accounts, means **no authentication** |
| `OZYMANDIS_OWNER_ID` | `owner-local` | The team every resource belongs to on a fresh install |
| `OZYMANDIS_SUPERUSER_NAME` | `batman` | The built-in administrator, seeded at every startup |
| `OZYMANDIS_SUPERUSER_PASSWORD` | *(a published default)* | **Change this.** The default is a constant in this repository |
| `OZYMANDIS_APP_DOMAIN` | — | Apps get `<name>.<this>`. Point `*.<this>` at the cluster |
| `OZYMANDIS_CERT_RESOLVER` | `letsencrypt` | Name of an ACME resolver **the ingress controller already has**. Wrong name = the controller's own certificate, silently. Empty = plain HTTP |
| `OZYMANDIS_BASE_URL` | — | Public URL the dashboard is reached at. Optional; sign-in no longer depends on it |
| `OZYMANDIS_SMTP_ADDR` / `OZYMANDIS_RESEND_API_KEY` | — | Mail transport, for whatever the install sends. Sign-in sends nothing |
| `OZYMANDIS_DEBUG` | `false` | Verbose logging |

The full list, with the reasoning behind each, is in
[`.env.example`](.env.example).

## Backups

K3s stores volumes on `local-path` by default: your data is a directory on one
node's disk, unreplicated, with nothing between it and that disk failing.
Everything else here can be rebuilt from configuration. That cannot.

Set a destination once, under **Settings › Backups** — any S3-compatible
storage: Cloudflare R2, Backblaze B2, Wasabi, S3, or a MinIO you run. There is
deliberately no option to write to the same machine; a copy on the disk you are
protecting against is not a backup.

Then switch backups on per app. Each gets a nightly [restic][] repository of its
own, encrypted before it leaves the machine:

- **A Postgres app** is dumped with `pg_dump`, not copied file by file. Its data
  directory is only consistent on disk between checkpoints, so a file-level copy
  of a running server restores into a database that may or may not start — and
  you find out which at restore time.
- **Anything else** has each volume copied, mounted read-only.

Retention (`restic forget --prune`) runs in the same job as the backup, so it
cannot silently stop while backups carry on filling the bucket.

[restic]: https://restic.net

### Restoring

**Backups › Restore** lists what the repository holds and puts one back. Do it
once on purpose, while nothing is wrong. A backup nobody has restored from is a
hypothesis, and the moment you need it is the worst moment to test it.

The scripts fail loudly rather than partially — `pipefail` on the dump so a
truncated `pg_dump` cannot be stored as a complete backup, `ON_ERROR_STOP` on
the restore so a half-applied one reports as failed.

Two things worth knowing before you start:

- **The repository password cannot be recovered.** It encrypts every snapshot,
  and it cannot be changed after the first one without starting the repository
  over. Keep it wherever you keep `OZYMANDIS_SECRET_KEY`.
- **`OZYMANDIS_SECRET_KEY` is required.** The storage credential and the
  repository password are sealed with it. Without one they would sit readable in
  the database — including in the backups this makes of it — so they are refused
  rather than stored that way.

`make test-backup` runs the real backup and restore against MinIO and Postgres
in Docker: it seeds a table, backs it up, drops the table, restores, and checks
the rows and the sequence came back.

### Certificates

Every hostname gets its own certificate, issued for that name and no other.
Platform hostnames (`<app>.<OZYMANDIS_APP_DOMAIN>`) and domains people bring are
treated identically — there is one path, not two.

**Ozymandis does not obtain certificates.** It writes one annotation on the
Ingress naming an ACME resolver, and the ingress controller does the rest. The
resolver is `OZYMANDIS_CERT_RESOLVER`, and it must name a resolver **the
controller already has configured**. Setting up that resolver is a cluster
concern, like installing the controller itself; it is not something this
installer does.

That division is worth stating plainly, because of how it fails. Ozymandis
cannot check the resolver exists. Name one that does not and nothing reports an
error — the annotation is written, the Ingress is accepted, the deploy is green,
and the controller answers every request with its own built-in certificate.
Visitors see a browser warning; the dashboard sees a healthy app. **So the check
that matters is the issuer on the served certificate, not the status code.**

```
echo | openssl s_client -connect your.app.example:443 \
  -servername your.app.example 2>/dev/null | openssl x509 -noout -issuer
```

A real issuer means it works. The controller's own default certificate means the
resolver name is wrong, and everything else will look fine.

Set `OZYMANDIS_CERT_RESOLVER` empty to serve every hostname over **plain HTTP**
instead. That is a supported way to run, and the domains page says so on each
app. It is the honest failure: visibly no TLS, rather than TLS nobody trusts.

**There are no wildcard certificates and no cert-manager.** Both are gone
deliberately. A wildcard covers the names it was issued for and nothing else, so
a hostname outside it was still served — under the wrong certificate, which a
browser reports as impersonation rather than as a misconfiguration. Per-host
issuance cannot produce that.

The cost of that choice is a real ceiling, and it is worth knowing before you
commit: Let's Encrypt allows **50 certificates per registered domain per week**.
Every app under `OZYMANDIS_APP_DOMAIN` spends one, so an install creating more
than fifty apps a week under a single app domain will start being refused. A
domain somebody brings is a separate registered domain with its own allowance
and does not count against yours. If you expect to run at that scale under one
domain, this build is not shaped for it.

Validation is over TLS-ALPN-01 or HTTP-01 depending on how the controller's
resolver is configured, and either way a hostname must resolve here before a
certificate can be obtained for it. Ozymandis requires that anyway — nothing is
routed until the claim is verified.

## Security posture

Workloads are hardened by construction, not by configuration. Every namespace
Ozymandis creates is labelled for Pod Security Admission at `restricted` and gets a
default `LimitRange`; every pod runs with:

- `runAsNonRoot`, `allowPrivilegeEscalation: false`, `privileged: false`
- all Linux capabilities dropped
- `seccompProfile: RuntimeDefault`
- a read-only root filesystem, with a writable `/tmp` so that stays practical
- no service account token mounted

There is no API for privileged containers, host networking, or host paths —
not as an omission to be filled in later, but because a request for them has
nowhere to go. Images that genuinely need a writable root filesystem have one
explicit, visible escape hatch (`WritableRootFilesystem`).

An important consequence: **images that run as root will not start.** That is
the intended behaviour. Most official images already ship a non-root user.

## Architecture

The shape is below. [ARCHITECTURE.md](ARCHITECTURE.md) has the mechanism: the
request path, the deploy and build pipelines, how routing and TLS are decided,
and what each step does when it fails.

Ozymandis is built to be wrapped. Anything that needs to differ for a hosted,
multi-tenant deployment goes through one of four seams, so a larger
application can build on this module rather than fork it:

| Seam | Interface | Engine ships | A wrapper supplies |
|---|---|---|---|
| Orchestration | `orchestrator.Orchestrator` | single cluster | multi-cluster placement |
| Identity | `identity.Provider` | single owner, bearer token, sessions | tokens resolved to an account |
| Dashboard chrome | `web.SlotProvider` | plain navigation | account switcher, usage, billing |
| Notification | `notify.Mailer` | SMTP, Resend, log | whatever sends the org's mail |

Two rules keep the seams honest:

1. **No Kubernetes types cross the orchestrator boundary.** Callers never
   import `client-go`, and a non-Kubernetes backend stays possible.
2. **Every table carries `owner_id`, and unique constraints are scoped by it.**
   The engine writes one value there forever. It exists so that scoping is a
   cheap indexed predicate rather than a join added later — a predicate is a
   check that gets written, a join is a check that gets skipped.

```
cmd/ozymandis             entrypoint and wiring
internal/app          workload lifecycle — keeps database and cluster agreeing
internal/account      people: users, passwords, teams, roles
internal/cluster      how a machine joins this cluster
internal/config       environment configuration
internal/domain       hostnames — the one we issue, and the ones people bring
internal/notify       delivers messages to people
internal/registry     where images this install builds are pushed
internal/secret       values that must survive a database dump
internal/identity     SEAM 2 — who owns this request
internal/orchestrator SEAM 1 — the runtime contract
          └── k8s     Kubernetes implementation
internal/store        schema, embedded migrations, sqlc queries
internal/web          SEAM 3 — dashboard, slot-based layout
```

The database is the source of truth for which apps exist; the cluster is the
source of truth for how they are doing. Neither is asked the other's question,
which is why listing apps never enumerates Deployments and why status is never
cached in a column that can go stale.

## Development

```bash
make assets     # templ codegen + Tailwind
make check      # vet + tests
make build
make dev        # rebuild and run
```

`templ` and `sqlc` are Go tool dependencies, and `make css` downloads the
**Tailwind standalone CLI** — a single binary, no Node or npm. So a contributor
needs nothing installed beyond Go.

Generated `*_templ.go` files and the compiled `app.css` are both committed,
which keeps plain `go build` working for anyone who has not run codegen. CI
rebuilds both and fails on drift.

### Design system

The UI uses the [templUI](https://templui.io) / shadcn token set — the same
CSS custom properties, so a component copied in with `templui add <name>`
inherits this theme unchanged. Deliberate departures:

- **Monochrome primary.** A saturated primary button reads as consumer
  software. Colour is reserved for state, where it carries information.
- **Denser than stock.** 13px base, tighter rows, borders instead of shadows,
  a metric strip instead of a grid of stat cards.
- **Status is a dot and a word**, not a filled pill. A page of coloured pills
  is noise, and the one that matters stops standing out.

Light and dark both ship, resolved before first paint so there is no flash on
navigation.

### Visual states

Every state the dashboard can be in — degraded workloads, failed deploys, a
node at 98%, an unreachable cluster — renders from a gallery, without needing a
cluster or a database:

```bash
OZYMANDIS_GALLERY_OUT=/tmp/gallery go test ./internal/web -run Gallery
```

Those are the states that silently rot, because nobody sees them until a
customer does. The generated gallery can be reviewed directly from its output
directory.

Tests use the `client-go` fake clientset, so the full orchestration path —
namespaces, security context, apply idempotency, status — is verified without
a cluster.

## Contributing

Issues and pull requests are welcome. There is no CLA; contributions are
licensed under MIT, the same as the project.

[CONTRIBUTING.md](CONTRIBUTING.md) covers getting set up, what CI enforces, and
the conventions this codebase holds to. One thing worth knowing before your
first test run: **tests that need a database skip themselves when it is absent,
and a skip reads as a pass** — set `OZYMANDIS_TEST_DATABASE_URL`.

Found a vulnerability? Please do not open an issue — see
[SECURITY.md](SECURITY.md). Participation is covered by the
[Code of Conduct](CODE_OF_CONDUCT.md).

## License

MIT — see [LICENSE](LICENSE).
