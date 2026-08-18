# Running Ozymandis on a VPS

This is the operator's guide: how to get Ozymandis onto a server, put an app on
it, and run that app day to day. It assumes you have a fresh Debian or Ubuntu
VPS and a domain you control.

For what Ozymandis is and why it exists, see [README.md](README.md). For how it
is built, see [ARCHITECTURE.md](ARCHITECTURE.md).

> [!WARNING]
> Ozymandis is under active development. Run it on something you can afford to
> lose, and keep backups you have actually restored from.

---

## Contents

- [Before you start](#before-you-start)
- [Install](#install)
- [First login](#first-login)
- [DNS](#dns)
- [Your first app](#your-first-app)
- [ozymandis.toml](#ozymandistoml)
- [Secrets](#secrets)
- [Deploying](#deploying)
- [Running it day to day](#running-it-day-to-day)
- [Datastores](#datastores)
- [Domains and TLS](#domains-and-tls)
- [Keeping apps apart](#keeping-apps-apart)
- [Upgrading and rolling back](#upgrading-and-rolling-back)
- [When something is wrong](#when-something-is-wrong)
- [What is not there yet](#what-is-not-there-yet)

---

## Before you start

**A server.** Debian or Ubuntu, 2 GB RAM is comfortable, 1 GB works if you are
not building images on it. Ozymandis installs K3s, so the machine is both the
control plane and the only node until you add another.

**Root.** The installer provisions system packages, a systemd unit and a
Postgres role.

**A domain.** Apps are reached at `<app>.<your app domain>`, so you need a
wildcard record pointing at the server. You can install without one and add it
later, but nothing will be routable until you do.

**Ports 80 and 443 open.** Traefik terminates TLS on them, and the ACME HTTP
challenge needs 80 reachable from the internet.

---

## Install

```sh
curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh
```

The script downloads the latest release, verifies its checksum **before**
unpacking, installs K3s and Postgres, writes a systemd unit, and starts the
service.

It is safe to re-run. It replaces the binary and the unit and leaves every
generated secret alone — re-running is the normal way to upgrade.

### Flags

Because the script arrives on stdin, flags go after `sh -s --`:

```sh
curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh -s -- --port 9000
```

| Flag | What it does |
|---|---|
| `--version vX.Y.Z` | Install a specific release rather than the latest |
| `--binary PATH` | Install a binary you already have; no download, no checksum |
| `--port N` | Listen port, default 8080 |
| `--rotate-token` | Issue a new dashboard token instead of keeping the existing one |
| `--skip-k3s` | Use the kubeconfig already present rather than installing K3s |
| `--database-url URL` | Use an existing Postgres instead of installing one |

`--binary` is the path for installing a build from a working tree. It checks
the file is an ELF binary for this architecture — a wrong-architecture binary
installs perfectly, starts, and dies, which reads as the service being broken
rather than as the wrong file having been handed over.

### What lands where

| Path | What |
|---|---|
| `/usr/local/bin/ozymandis` | The server |
| `/usr/local/bin/oz` | The CLI |
| `/etc/ozymandis/ozymandis.env` | Configuration, including generated secrets |
| `/etc/systemd/system/ozymandis.service` | The unit |

The service runs as the `ozymandis` user. Its Postgres role is `ozymandis` and
its database is `ozymandis_command_center`.

### Configuration

Everything is read from the environment, and the installer writes sane values
for all of it. Edit `/etc/ozymandis/ozymandis.env` and
`systemctl restart ozymandis` to change one.

The ones you are most likely to touch:

| Variable | What it is |
|---|---|
| `OZYMANDIS_APP_DOMAIN` | The suffix apps get hostnames under |
| `OZYMANDIS_BASE_URL` | The dashboard's own public URL |
| `OZYMANDIS_ADDR` | Listen address, e.g. `:8080` |
| `OZYMANDIS_CERT_RESOLVER` | Traefik ACME resolver name for per-host TLS |
| `OZYMANDIS_INGRESS_NAMESPACE` | Namespace Traefik runs in — see [Keeping apps apart](#keeping-apps-apart) |
| `OZYMANDIS_RESERVED_DOMAINS` | Hostnames apps may not claim |
| `OZYMANDIS_AUTH_TOKEN` | The dashboard/API token |
| `OZYMANDIS_SECRET_KEY` | Seals app secrets at rest — **losing this loses every secret** |

> [!IMPORTANT]
> `OZYMANDIS_SECRET_KEY` is the key every app secret is sealed with. Re-running
> the installer never mints a new one, for exactly this reason. If you restore
> this machine from a backup, restore that key with it or every secret becomes
> unreadable.

---

## First login

The installer prints a token when it finishes. Open the dashboard at whatever
you set `OZYMANDIS_BASE_URL` to and sign in with it.

Then point the CLI at the same install:

```sh
oz auth login          # asks for the endpoint and token
oz auth whoami         # endpoint, team, role
```

Credentials live in `~/.config/ozymandis/config.toml`. One file can hold several
installs; pick between them with `--context NAME`, `OZ_CONTEXT`, or by whichever
`oz auth login` set last.

`oz` runs anywhere — your laptop, CI, the server itself. It only needs to reach
the API over HTTPS.

---

## DNS

Two records:

```
A      ozymandis.example.com     <server IP>     # the dashboard
A      *.apps.example.com        <server IP>     # every app
```

Set `OZYMANDIS_APP_DOMAIN=apps.example.com` and an app named `web` answers at
`web.apps.example.com`, with a certificate issued on first request.

A custom hostname on top of that is a `CNAME` to the app's own name — see
[Domains and TLS](#domains-and-tls).

---

## Your first app

Apps are created in the dashboard, or over the API. The CLI has no `create`
yet; it operates on apps that already exist.

**Source** is the one decision that shapes everything after it:

| Source | What it means |
|---|---|
| `image` | Pull a public container image |
| `git` | Build from a repository on every deploy |
| `postgres` | A managed Postgres for your apps |
| `redis` | A managed Redis |
| `template` | Start from a preset |

For a `git` app you give a repository URL, a branch, and optionally a
subdirectory — a monorepo holding three services is three apps pointing at
three subdirectories of one repo.

A private repository needs a deploy key. Generate one per app:

```sh
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://ozymandis.example.com/api/v1/apps/web/deploy-key
```

That returns a public key. Add it to the repository's own deploy keys on GitHub
— **the repository's, not your account's**. A deploy key grants access to one
repo, which is the whole point: a build for one app cannot read another.

---

## ozymandis.toml

Put an `ozymandis.toml` beside your code and the CLI stops needing `--app` on
every command. It is also the file `oz deploy` converges the app to.

```toml
name = "web"

[build]
repo   = "ssh://git@github.com/you/your-repo.git"
branch = "main"
subdir = "services/web"      # optional, for a monorepo

[deploy]
release_command = "./migrate.sh"   # runs against the new image before traffic moves

[service]
port     = 8000
internal = false             # true = no public route at all

[health]
path     = "/ready"
liveness = false

[scale]
replicas = 2

[env]
LOG_LEVEL   = "INFO"
ENVIRONMENT = "production"

[[volumes]]
name = "data"
path = "/var/lib/data"
size = "5Gi"

[[domains]]
host = "www.example.com"
```

Write the current server-side config out to a file to start from:

```sh
oz config --app web            # print it
oz config --app web --write    # write ./ozymandis.toml
```

### Three things the file does not own

These are deliberate, and each of them has bitten somebody:

**Replicas.** `[scale] replicas` applies when the app is created, and is then
left alone. Replicas is the field you change while something is on fire, and
the right value is a property of right now rather than of the commit — a deploy
from a checkout that says `2` must not undo an emergency scale to `10`. Use
`oz deploy --scale` to apply it deliberately, or `oz scale N`.

**Volume size.** Storage grows and never shrinks. The file's size is a floor
applied at creation, not a target converged to.

**Domains.** Additive. A host in the file that the app lacks is added; a host on
the app that the file lacks is left alone and reported. Deleting a line from a
config file should not silently drop a certificate and start returning NXDOMAIN
to live traffic. Remove one with `oz domains remove HOST`.

### Secrets are not in it

A file that tries to carry `[secrets]` is rejected with an error that says why.
Secrets have no read path at all — not in the API, not in the dashboard, not in
`oz config`. Once set, a value can be replaced but never displayed.

---

## Secrets

```sh
oz secrets list                     # names only, always
oz secrets set API_KEY=... DB_URL=...
oz secrets unset API_KEY
```

Setting a secret rolls the pods, and you do not need to deploy afterwards.
This is not incidental: environment comes from a Kubernetes Secret by
reference, so changing the Secret alone would leave every running pod on the
old value until something unrelated restarted it. The pod template carries a
hash of the secret material, so a change to a secret is a change to the
template and the rollout follows.

### Loading a file

Values on a command line are visible in `ps` and land in your shell history.
Use `--stdin`, which reads `KEY=value` lines and skips blanks and `#` comments:

```sh
oz secrets set --app web --stdin < prod.env
```

That way the value never becomes an argument. The command prints the keys it
set, never the values.

### --plain

`--plain` stores a variable readable instead of sealed — for a log level, not a
password. The difference is whether you can ever get it back: a sealed value has
no read path anywhere in the system, so `oz config` shows plain variables under
`[env]` and omits sealed ones entirely.

Prefer `[env]` in `ozymandis.toml` for genuinely non-sensitive configuration.
It lives in your repository where you can read it back and diff it.

---

## Deploying

### From the CLI

```sh
oz deploy                 # converge config, then deploy
oz deploy --dry-run       # show what would change and stop
oz deploy --watch         # wait for it to finish; non-zero if it fails
oz deploy --no-config     # skip the config converge, just redeploy
oz deploy --scale         # also apply [scale] replicas
```

`--watch` is what you want in CI. It waits for the rollout to actually
complete — every replica on the new template and available — rather than for
one pod to report ready, which a rolling update satisfies while the old version
is still serving.

### From CI

Mint a token, put it in the repository's secrets, and call the API. A minimal
GitHub Actions step:

```yaml
- name: Deploy
  env:
    OZ_TOKEN: ${{ secrets.OZYMANDIS_TOKEN }}
  run: |
    curl -fsSL -X POST \
      -H "Authorization: Bearer $OZ_TOKEN" \
      https://ozymandis.example.com/api/v1/apps/web/deploy
```

Or install `oz` in the job and use `oz deploy --watch`, which gives you the
rollout gate and a non-zero exit for free.

### Reading a build

A `git` app builds in-cluster. When a build fails, the log is on the API rather
than only in the dashboard:

```sh
curl -H "Authorization: Bearer $TOKEN" \
  "https://ozymandis.example.com/api/v1/apps/web/deployments/$ID/build?tail=100"
```

`tail` limits it to the last N lines and the response says whether it was
truncated. In CI, printing this on failure turns "the deploy failed" into the
compiler error that caused it.

---

## Running it day to day

```sh
oz apps                       # everything, with status and URL
oz status --app web           # one app: replicas, image, source, port, URL
oz logs --app web -f          # follow
oz logs --app web -n 500      # history
oz releases --app web         # recent deployments
oz scale --app web 3
```

> [!NOTE]
> Flags come **before** the positional argument: `oz scale --app web 3`, not
> `oz scale 3 --app web`. Argument parsing stops at the first non-flag, so the
> second form silently ignores `--app` and then complains it was given three
> arguments instead of one.

### Getting into a container

```sh
oz console --app web                  # a shell; tries bash, falls back to sh
oz exec --app web -- ls -la /app      # one command
oz exec --app web -- sh -c 'echo hi'
```

`exec` allocates a terminal only when stdin is one, so it is safe in a CI job —
output comes back as plain bytes with no control sequences, and `oz` exits with
whatever the command exited with, which is what makes it usable in a script.

Both are admin-only. A shell in a running container reads every secret the app
holds, so it sits behind the same role that can delete the app.

### Choosing a replica

```sh
oz console --app web --pod web-7d9f8b6c4-xk2mn
```

Without `--pod` you get the lowest-named ready one.

---

## Datastores

A `postgres` or `redis` app is an ordinary app from the platform's point of
view, with a volume and no public route.

They are reachable from your other apps at the in-cluster address, which
`oz config` will show you in the consuming app's `[env]`:

```
postgres.ozymandis-<hex>.svc.cluster.local:5432
```

Each app gets its own namespace (`ozymandis-<hex>`), so the hostname includes
it. Copy it from `oz config` rather than assembling it by hand.

Datastores should be `internal = true`. There is no reason for Postgres to have
a public hostname, and [the next section](#keeping-apps-apart) explains what
that flag now buys you.

---

## Domains and TLS

Every app gets `<name>.<app domain>` automatically. To add your own:

```sh
oz domains list --app web
oz domains add --app web www.example.com
oz domains remove --app web www.example.com
```

Adding a host claims it; nothing routes to it yet. `oz domains list` shows the
target to point a `CNAME` at, and the domain is then verified in the dashboard
— there is no API for that step.

TLS is issued per host through Traefik's ACME resolver on first request —
nothing to run, but the first request to a new hostname is slow while the
certificate is obtained, and port 80 must be reachable for the challenge.

Hostnames in `OZYMANDIS_RESERVED_DOMAINS` cannot be claimed by an app, which is
what stops someone taking the dashboard's own name.

---

## Keeping apps apart

By default every namespace in a Kubernetes cluster can reach every other one.
On a single-tenant install that is merely untidy; if you host anything you did
not write, it is a hole — `internal = true` removes the *route* to an app, but
without a NetworkPolicy any other pod in the cluster can still reach it
directly by service address.

Ozymandis writes a deny-all-ingress NetworkPolicy into each app namespace,
allowing only:

- other namespaces belonging to the same owner, and
- the ingress controller's namespace.

That second allowance is why `OZYMANDIS_INGRESS_NAMESPACE` matters. It must name
the namespace Traefik actually runs in — on a default K3s install, `kube-system`:

```sh
OZYMANDIS_INGRESS_NAMESPACE=kube-system
```

**If it is unset, the policy is skipped entirely** rather than written in a form
that would black-hole your own traffic. So an install that never set it has no
isolation, and setting it takes effect on each app's next deploy — existing apps
keep running under the old arrangement until you deploy them.

K3s runs kube-router in-process, so NetworkPolicy is enforced with nothing extra
to install. Verify with a pod in one namespace curling a service in another: it
should hang and time out, not connect.

---

## Upgrading and rolling back

Upgrading is re-running the installer. It replaces the binary and the unit and
keeps your configuration and secrets:

```sh
curl -sSL https://kingzion24.github.io/ozymandis/install.sh | sudo sh
```

The previous binary is kept beside the new one, so a bad upgrade is reversible
without a download:

```sh
sudo cp -f /usr/local/bin/ozymandis.bak /usr/local/bin/ozymandis
sudo systemctl restart ozymandis
```

Restarting the control plane does **not** interrupt your apps. They are ordinary
Kubernetes Deployments and keep serving while Ozymandis is down; what stops is
the dashboard, the API, and anything that needs them — deploys, log streaming,
`oz`.

### What to back up

| | |
|---|---|
| `/etc/ozymandis/ozymandis.env` | Config, **and `OZYMANDIS_SECRET_KEY`** |
| The `ozymandis_command_center` database | Apps, deployments, sealed secrets |
| Your app volumes | Whatever your apps wrote |

The env file and the database are a pair. The database holds secrets sealed with
the key in the env file; either one alone will not restore.

---

## When something is wrong

**Start with the control plane.**

```sh
systemctl status ozymandis
journalctl -u ozymandis -n 100 --no-pager
```

**Then the app.** `oz status` gives you replicas and the image actually running;
`oz logs` gives you the app's own output.

| Symptom | Usually |
|---|---|
| `ImagePullBackOff` | A private image and a registry credential that did not load. The deploy now fails with the registry named rather than deferring it to the pod. |
| Pods never become ready | The readiness path returns non-200. Probes allow 3s per check, every 10s, 3 failures. A probe that checks a slow dependency fails as a timeout rather than as what it found. |
| Restart loop | A liveness probe checking an external dependency. Liveness restarts the container, so wiring a database into it turns one outage into a restart loop that cannot recover. Check dependencies in readiness, not liveness. |
| Deploy "succeeds", old code serving | You waited for a pod, not for the rollout. Use `oz deploy --watch`. |
| A secret change did nothing | Fixed — setting a secret rolls the pods. If you are on an older build, redeploy. |
| Certificate never issues | Port 80 unreachable, or DNS not pointing here yet. |
| App unreachable but running | `internal = true`, or DNS, or the wildcard record is missing. |

**A word on probes.** Readiness decides whether an instance takes traffic;
liveness decides whether it gets killed. Check hard dependencies in readiness
and nothing external in liveness. An instance that cannot reach its database
should stop taking requests, not be restarted into a loop.

---

## What is not there yet

Being straight about the edges, so you do not discover them at a bad moment:

- **No `oz create`.** Apps are created in the dashboard or over the API.
- **Custom domains need a CNAME target you configure yourself**, and there is
  no API for domain verification.
- **No deploy from a tag or a specific SHA** — deploys take a branch's head.
- **No metrics or alerting.** `oz logs` and `oz status` are what you have.
- **Backups are not automated for you.** See [what to back up](#what-to-back-up).
- **One node** unless you add more yourself.

Every workload is an ordinary Kubernetes Deployment, so `kubectl` keeps working
for anything Ozymandis does not cover yet, and nothing you build here is locked
in.
