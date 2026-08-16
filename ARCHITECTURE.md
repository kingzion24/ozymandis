# Architecture

How Ozymandis works, end to end.

The [README's Architecture section](README.md#architecture) gives the shape in a
table. This document gives the mechanism: what runs where, what happens on each
request, and — where it matters — what happens when a step fails. Where a
decision looks arbitrary, the reason is usually a failure mode that was chosen
against; those are called out rather than left for a reader to rediscover.

---

## 1. Shape of the system

One Go binary (`cmd/ozymandis`), one Postgres database, one Kubernetes cluster.
Nothing else is required, and nothing runs as a sidecar or a daemon of its own.

```
                       ┌──────────────────────────────────────┐
  browser ───HTML/HTMX─┤                                      │
  oz CLI  ───JSON/WS───┤   ozymandis (single Go process)      │
  GitHub  ───webhook───┤                                      │
                       │   web ── api ── app ── orchestrator  │
                       └────────┬──────────────────┬──────────┘
                                │                  │
                          ┌─────▼─────┐      ┌─────▼──────────┐
                          │ Postgres  │      │ Kubernetes     │
                          │           │      │ (K3s + Traefik)│
                          │ which     │      │ how they are   │
                          │ apps      │      │ doing          │
                          │ exist     │      │                │
                          └───────────┘      └────────────────┘
```

**The split between the two stores is the load-bearing invariant.** Postgres is
the source of truth for *which apps exist*; the cluster is the source of truth
for *how they are doing*. Neither is asked the other's question. That is why
listing apps never enumerates Deployments, and why status is never cached in a
column that can go stale — a status column is a lie with a timestamp on it.

### Startup, in order

`run()` in `cmd/ozymandis/main.go:44` does the whole of the wiring, in this
order, and the order is not incidental:

1. **Load config** from the environment (`internal/config`). `.env` is a
   convenience, not a requirement.
2. **Migrate** — embedded SQL in `internal/store/migrations`, applied before
   anything opens a pool.
3. **Connect** the pgx pool.
4. **Connect the orchestrator.** `k8s.New` against a kubeconfig or in-cluster
   credentials. *If the cluster is unreachable, startup does not fail* — it
   falls back to `orchestrator.NewNoop()` and logs a warning. A self-hoster
   should be able to reach the dashboard and fix their kubeconfig from there,
   rather than face a process that refuses to boot.
5. **Build the credential-shaped things eagerly**, so they fail at startup
   rather than at first use: the mailer, the `secret.Keeper`. A malformed
   secret key that only surfaced the first time somebody saved a secret would
   read as the feature being broken.
6. **Decide identity** (§3).
7. **Assemble the app service** with whatever optional capabilities this
   install actually has (below).
8. **Ensure the owner row** exists.
9. **Mount** the router (§2), start two background loops (§4.5), serve.

### Capability gating

Ozymandis switches surfaces *off* rather than offering them and failing. Two
facts decide almost everything:

| Gate | Turns on |
|---|---|
| `OZYMANDIS_SECRET_KEY` is set | image registry, builds, add-node, stack templates, deploy-on-push, backups |
| orchestrator implements `orchestrator.Builder` | building from a Git repository |
| orchestrator implements `orchestrator.Runner` | release commands, backups, push polling |
| orchestrator implements `NodeManager` | cordon / drain / remove node |
| orchestrator implements `HTTPLogger` | per-request HTTP logs |

The secret-key rule is one rule applied five times: **an install that cannot
seal a credential declines to hold it, rather than storing it readable.** A
registry password, a cluster join token, a webhook secret, a deploy key and a
backup encryption password are all credentials, and a credential protected in
name only is worse than one nobody stored — nothing about it looks wrong.

The optional interfaces are asserted for, not required. An orchestrator that
cannot run a Job is still a working orchestrator; folding `Build` into the
required interface would make every implementation carry it.

---

## 2. Request path

`mount()` builds a root `http.ServeMux` with exactly three entries:

```
/api/       → internal/api    JSON + WebSocket. Returns status codes.
/webhooks/  → api.WebhookHandler   No identity middleware at all.
/           → internal/web    HTML + HTMX. Redirects to a sign-in form.
```

They are separate mounts rather than routes inside one server, and that is
deliberate on both counts.

**The dashboard and the API are different surfaces for different callers.** One
renders pages and redirects an unauthenticated visitor to a sign-in form; the
other returns 401 and never redirects anywhere. Mounting them separately keeps
the dashboard's browser-shaped middleware from ever running on an API request.
The pattern is `/api/` rather than `/api/v1/` because `ServeMux` matches the
longest pattern — so this claims every future version, and an unrecognised one
gets the API's JSON 404 instead of the dashboard's HTML.

**The webhook endpoint is outside the identity middleware entirely.** GitHub
carries no credential of ours; the delivery's HMAC signature *is* the
authentication. Mounted here rather than inside the API router so it cannot
inherit an auth middleware by accident — a webhook behind a bearer-token gate is
a webhook that never fires.

### The JSON API

chi, rooted at `/api/v1`, with role gates rather than a flat authenticated/not
split (`internal/api/server.go:155`):

```
Member (read)                        Admin (write)
  GET  /whoami                         POST   /apps
  GET  /apps                           DELETE /apps/{name}
  GET  /apps/{name}                    POST   /apps/{name}/deploy
  GET  /apps/{name}/status             POST   /apps/{name}/scale
  GET  /apps/{name}/deployments        PUT    /apps/{name}/config
  GET  /apps/{name}/secrets  (keys)    PUT    /apps/{name}/secrets
  GET  /apps/{name}/config             DELETE /apps/{name}/secrets/{key}
  GET  /apps/{name}/domains            POST   /apps/{name}/domains
  GET  /apps/{name}/logs               DELETE /apps/{name}/domains/{id}
                                       POST   /apps/{name}/deploy-key
                                       GET    /apps/{name}/exec  (WebSocket)
```

`GET /apps/{name}/secrets` returns keys, never values. There is no read path for
a secret's value by design: `SetVariable` is how one is replaced.

`POST /apps/{name}/deploy-key` mints a pair and returns the **public** half;
`GET /apps/{name}` carries that same public half on every read. Both halves of
that matter. Without the endpoint, this API could create an app from a private
repository and not give it the credential to clone one — the fix lived on a
dashboard page a script cannot reach. Without the field, the only way to see
which key a repository should trust would be to mint another, and minting
replaces the pair — a read that revokes the key already working.

Like `exec` and `domains`, it is **absent from the router** rather than mounted
and failing where the install cannot serve it: no `OZYMANDIS_SECRET_KEY` means
no way to seal a private key, so there is no endpoint to call.

Where accounts are off, `Roles` is nil and every authenticated caller acts as the
owner. That is not a permissive default — it is the literal truth of a
shared-token install, and it is the same conclusion the dashboard reaches.

### The `oz` CLI

`cmd/oz` is a client over that API and nothing more. Stdlib `flag` and a table of
subcommands, no CLI framework: the engine has ten direct dependencies and a
stated goal of being readable in an afternoon.

```
oz auth       oz apps      oz status    oz scale     oz releases
oz config     oz deploy    oz secrets   oz logs      oz domains
oz console    oz exec
```

An `Env` carrying the resolved context and client is built once in `main`, so the
precedence rules for `--context` live in one place — *a deploy going to the wrong
install is the mistake the whole context mechanism exists to make visible.* Data
goes to stdout, everything a person reads goes to stderr, and a command that ran
in a container and exited non-zero exits `oz` with the same code.

### Interactive console

`GET /apps/{name}/exec` upgrades to a WebSocket (`internal/api/exec.go`).

- **`CheckOrigin` refuses cross-origin upgrades.** The browser sends no preflight
  for a WebSocket and its same-origin policy does not cover them, so without this
  any page a signed-in person visits could open a shell in their containers using
  their cookie. A request with no `Origin` header at all is allowed — that is a
  CLI, and `Origin` is a browser concept.
- **Which pod is decided server-side, from pods this app actually has.** A pod
  name on its own is enough to reach any tenant's container, so the choice is the
  security boundary, not a convenience.
- Only **ready** pods are attachable — a shell in a crashlooping container is a
  race against the restart — and among those, **the lowest name sorted**.
  Deterministic rather than meaningful, so two consecutive consoles land in the
  same place. **The caller is always told which one**; the choice being arbitrary
  is exactly why it has to be visible.
- Sessions are recorded (`exec_sessions`, migration `00025`).

---

## 3. The identity seam

`internal/identity` answers one question: **who owns this request?** Everything
downstream — handlers, storage, the orchestrator — takes an `OwnerID` and never
asks where it came from.

Three configurations, decided once at startup by `newIdentity`:

| Config | Provider | Who gets in |
|---|---|---|
| `OZYMANDIS_BASE_URL` set | `First(tokens, sessions)` | anyone who signs in, resolved to a team |
| `OZYMANDIS_AUTH_TOKEN` set | `NewStaticToken` | anyone with the token, acting as the single owner |
| neither | `NewSingleOwner` | **anyone who can reach the port** |

Each branch says so in the log, because an operator must be able to tell from a
startup line which one is active.

`identity.First` tries providers in order and takes the first valid `Owner`.
**Order is significance, not preference:** a request carrying both a cookie and a
bearer header resolves as the *header*, because a token is explicit and
per-request while a cookie is ambient and attached by the browser whether or not
the caller meant it. Someone testing an API token from a signed-in tab should be
testing the token.

A provider returning an error means "not mine", not "reject this" — that is what
a session provider says about a request with no cookie. Only when every provider
declines does the chain decline, and *the reason is deliberately not reported*:
which credential was absent or malformed is information about what would have
worked.

`MustFromContext` panics rather than returning a zero owner. Behind the
middleware, a missing owner is a wiring bug, and panicking makes it loud in
development instead of silently serving another owner's data.

**Every table carries `owner_id`, and unique constraints are scoped by it.** The
single-owner engine writes one value there forever; it exists so scoping is a
cheap indexed predicate rather than a join added later — a predicate is a check
that gets written, a join is a check that gets skipped.

---

## 4. App lifecycle

`internal/app` is the layer that keeps the database and the cluster agreeing.

### 4.1 Namespaces

One namespace per app:

```go
"ozymandis-" + hex(sha256(ownerID + "/" + name))[:16]
```

A fixed-width hash rather than a slug. Slugging then truncating to Kubernetes'
63-character limit is how two differently-named apps end up sharing a namespace
and deleting each other — truncation silently removes the part that made them
unique. The readable name lives in a label instead.

Every namespace gets Pod Security Admission `restricted` and a default
`LimitRange`, so a workload that requests nothing still cannot run unbounded.
It is deliberately not possible to create a namespace with no limits at all.

### 4.2 `apply` — the convergence step

`Service.apply` (`internal/app/service.go:593`) is the single path to the
cluster. Every mutation funnels through it:

1. `EnsureNamespace` — posture and limits.
2. `reconcileHosts` — bring the managed hostname in line with current config,
   return every **routable** host. Done on every apply rather than only at
   create, so changing the app domain moves each app's URL on its next deploy
   rather than rewriting every app at startup, which would put a config typo in
   the path of the whole install at once.
3. **If the image is still `PendingImage`, stop here.** The app gets its
   namespace and its hostname and nothing else. Applying the placeholder would
   create a Deployment that cannot pull, and Kubernetes would report
   `ImagePullBackOff` — an error about the image name, for an app whose image
   simply does not exist yet.
4. Read volumes and env **from the store, not from the caller's copy** — an
   attach writes a row and then applies, and a stale slice would deploy the
   workload without the storage just created for it.
5. Split env into plaintext and sealed; sealed values become a Kubernetes Secret
   read with `envFrom`, so they are absent from `kubectl get deploy -o yaml` —
   the copy people read, paste into issues, and check into repositories.
6. Parse the command line into argv.
7. `orch.ApplyApp(AppSpec{…})`.

`apply` takes the caller's store handle, so `Create` can pass its open
transaction and have the reconcile read the app row it has not committed yet.

### 4.3 Deploy paths

Three entry points, and which one is used depends on whether there is
minutes-scale work to do:

```
Scale / most edits ──────────────► apply ──► endDeployment      (synchronous)

Redeploy, no build, no release ──► apply ──► endDeployment      (synchronous)

Create-from-Git, Redeploy with
a build OR a release command ────► deployInBackground           (goroutine)
                                      │
                                      ├─ buildIfNeeded  ──► §5
                                      ├─ runRelease     ──► §4.4   ◄── veto
                                      ├─ apply
                                      └─ endDeployment
```

The release condition on that third branch is not an optimisation. Without it an
image-sourced app's release command would never run at all — the synchronous
branch applies directly, and the release hook lives in `deployInBackground`. An
image app with migrations is an ordinary thing to have.

**The goroutine's context is detached from the request's**
(`context.WithoutCancel`, capped at a 40-minute `deployTimeout`). A build
outlives the HTTP request that asked for it by minutes, and one cancelled when
the browser navigated away would leave a deployment stuck on "running" with a
half-built image behind it.

The background deploy is backgrounded *after* the transaction commits, never
inside it. A transaction held open for a build would block every other write to
these tables — including the build's own log, written from the goroutine that
would be waiting on it.

**Deployment rows.** `beginDeployment` supersedes earlier rows and writes a new
one as `running`; `endDeployment` writes `active` or `failed` with the cause.
Both are recorded whichever way it went — *a deploy that failed and left no trace
is one nobody can find afterwards.*

### 4.4 The release command

Runs **between the build and the apply**, against the new image, before any
traffic moves (`internal/app/release.go`). It is a veto: a release that fails
leaves `apply` uncalled, so the previous image keeps serving. That is the entire
point — a migration that cannot run is caught before the code assuming it ran
starts taking requests.

- Capped at 10 minutes, well under the task default. A release that runs for an
  hour has already gone wrong, and a hung one should fail the deploy rather than
  hold it until the deploy's own timeout.
- The release task carries its **own** `RegistryAuth`, rather than relying on a
  pull secret in the namespace. A release runs *before* the deploy that would put
  that secret there, so on an app's first deploy a task relying on one would fail
  with `ImagePullBackOff` — on exactly the deploy a release command matters most,
  the one that creates the database it is about to migrate.
- Every deployment records a release status — `skipped`, `succeeded`, `failed`,
  `unavailable` — so "no release ran" and "this row predates the column" do not
  look alike.
- `ErrReleaseFailed` is its own error because the distinction matters to every
  caller: a failed build means nothing was produced, a failed apply means the
  cluster refused what was produced, and this means both worked and **the app
  itself said do not ship this.**

### 4.5 The two background loops

Started in `run()`, both level-triggered against the cluster rather than driven
by anything the process remembers — which is what makes them correct after a
restart and correct with several replicas running at once.

**`RunReconciler`** — every minute, settles builds that claim to be running. A
build is driven by a goroutine, and *a goroutine is not a source of truth*: it
does not survive a restart and never existed on the other replicas. The Job does,
and every replica can read it, so "is this still running" is a lookup rather than
a guess about a process.

Three details carry the correctness:

- A **2-minute grace period**, because the build row is written before the Job is
  created. A reconcile landing in that window would see no Job, conclude the
  build had died, and fail a build that was about to start.
- **The cluster not answering is not evidence a build died.** The pass logs and
  moves on; marking it failed would turn an unreachable API server into a wave of
  failed deployments.
- A build whose Job is gone is recorded **failed**, not something softer. It
  produced no image, so there is nothing to deploy — but it says *why*, which is
  the difference between a useless status and a usable one.

**`RunPoller`** — every five minutes, checks auto-deploy repositories for new
commits. This is the fallback for installs GitHub cannot reach; an install with a
working webhook simply finds nothing new on every pass. **One Job per pass, not
one per app**: a single `git ls-remote` across every repository, printing one line
each. A pod per app per five minutes would be twelve pods an hour for one app and
rather a lot for twenty, on a cluster whose whole selling point is that it fits on
a small machine. The control plane does not run `git` itself and has no business
gaining a git binary — and the deploy key a private repository needs already has a
path into a build pod.

### 4.6 Deploy on push

```
GitHub ──POST /webhooks/…──► VerifySignature ──► ParsePush ──► deploy
                              (HMAC-SHA256)
```

- `hmac.Equal`, not `==`. A byte-by-byte string comparison returns as soon as it
  finds a difference, so how long it takes says how much of the prefix was right —
  a signature is then guessable one byte at a time by anybody who can measure that.
- The `sha256=` prefix is required rather than tolerated. The algorithm is part of
  what is being asserted, and accepting a bare hex digest would accept a signature
  computed with something weaker.
- **The payload's `repository` field decides nothing.** Anybody can POST a body
  naming any repository; the signature is what proves which app a delivery is for.
  The field is read for logging and for a candidate lookup, and for nothing else.
- `LastDeployedSHA` deduplicates. GitHub redelivers on its own schedule and a
  person can replay a delivery, so "have I already built this" must be answerable
  from state rather than from trusting the sender not to repeat itself.
- `commits` is a **pointer** to a slice, because absent and empty are different
  and the difference decides whether a monorepo app with a `subdir` builds.

### 4.7 `ozymandis.toml`

`internal/appspec` is the part of an app's config that lives with its code. One
rule governs the whole package: **every optional field is a pointer, a slice, or
a map.** The file is a partial description by design, so "the file did not mention
replicas" and "the file set replicas to 0" are opposite instructions and must
never collapse into the same value — *the deploy that scales production to nothing
must not look like the deploy that said nothing about scaling.*

What a config converge actually applies is written down, because someone editing
the file needs to know which of their edits take effect:

| | |
|---|---|
| **Converged on every deploy** (dashboard drift is reverted) | `[service]` port/internal, `[health]` path/liveness, `[env]` — the complete set, **including removals** |
| **Recorded and reported, applied by a deploy** | `[build]` repo, branch, subdir, image |
| **Never converged** — the operational axis | `[scale]` replicas, `[[volumes]]` size |

---

## 5. Build pipeline

Only where the install has a registry (`OZYMANDIS_SECRET_KEY` + registry
configured) and an orchestrator that implements `Builder`. Otherwise the Git
source is *listed as unavailable* rather than offered and failed.

```
runBuild ──► ImageFor(owner, app, revision)      revision = "d"+deployID[:16]
         ──► CreateBuild row                     (log streams into it live)
         ──► SetBuildJob(name)   ◄── BEFORE the build starts
         ──► builder.Build(BuildRequest{repo, ref, subdir, auth, sshKey, log})
         ──► FinishBuild(status, image, commitSHA)
         ──► SetAppRunAsUser(uid)
```

**The Job name is recorded before the build starts.** A Job whose name was never
stored is a Job the reconciler cannot ask about — which is exactly the build that
would then sit claiming to run forever.

**The revision tag is derived from the deployment id**, not the commit. Two
deploys of the same commit must not overwrite each other's image, because rolling
back means pulling the older one.

**The log streams into the row as it arrives**, not at the end. The log is most
worth reading while the build is running, and a build killed halfway would
otherwise leave a row with nothing in it. The log writer takes its own
non-cancellable context: when a build is cancelled the last thing written is
usually the reason, and a logger cancelled with it would drop exactly that line.

### Inside the cluster

Builds run in a **separate namespace** (`internal/orchestrator/k8s/build.go`),
under a service account created with **no RoleBinding of any kind**.

That namespace is PSA `privileged`, and that is the honest cost of building images
in-cluster: rootless BuildKit still needs a seccomp profile the restricted policy
forbids. The alternative was weakening every tenant namespace or having no builds.
What replaces the enforcement is scope — *nothing runs in this namespace except
Jobs this code creates, from images named as constants, and no tenant workload is
ever scheduled into it.* A build executes whatever a repository's Dockerfile says
to execute, so the account it runs under is the blast radius.

Three **init containers**, sharing one workspace volume, run in order:

| Step | Image (pinned) | Does |
|---|---|---|
| `clone` | `alpine/git:2.47.2` | checkout; prints `ozymandis-commit:` and `ozymandis-uid:` markers |
| `dockerfile` | `moby/buildkit:v0.19.0-rootless` | builds a Dockerfile if there is one; touches a marker file on success |
| `buildpack` | `paketobuildpacks/builder-jammy-base` | reads the marker; does nothing if the Dockerfile step already pushed |

Init containers rather than ordinary ones because **Kubernetes has no conditional
step** — every container in a pod runs — and init containers at least run *in
order*, so the second strategy can check whether the first succeeded.

The clone is its own step so its failures (a bad URL, a private repo, a ref that
does not exist) are reported as clone failures rather than as a build that
produced nothing. Data comes back out of the log stream via those markers, which
avoids a second call into the cluster to ask what was checked out.

Images are **pinned**, not floating. A build that silently changes what compiled
it is one nobody can reproduce, and `:latest` here means the toolchain moves under
a running install.

The deploy key is a **per-build** Secret deleted with the Job. A key that outlived
its build would be a key sitting in the cluster; and it must land somewhere the
clone can read and nowhere the built image can — a key baked into a layer is a key
published with the image.

That Secret carries a second thing: one line of `/etc/passwd`, mounted over the
file with `subPath`. `ssh(1)` calls `getpwuid(getuid())` before it opens a
socket and exits when the id resolves to nothing, and `alpine/git` has no entry
for the uid the clone runs as. Every ssh clone therefore failed having sent no
packet and read no key — reported by git as *"Could not read from remote
repository. Please make sure you have the correct access rights"*, which is its
message for a **rejected** key and sends you to check the one thing that was
never consulted. Public repositories clone over https, need none of it, and get
neither the volume nor the mount.

Buildkit runs **rootless** because the alternative is a privileged container with
the host's Docker socket, which is a root shell on the node for anybody who can
influence a Dockerfile.

Caps: 30 minutes for a build, 40 for the deploy around it — *a deploy that timed
out while applying an image that built fine would be the most confusing possible
failure.*

---

## 6. Routing and per-host TLS

### Hostnames

Two kinds, converging on one path:

- **Managed** — `<app>.<OZYMANDIS_APP_DOMAIN>`, minted the moment an app is
  created. Retired when the app loses its port or goes internal: a workload with
  no port takes no traffic, so holding a globally unique name against every other
  app would be for nothing.
- **Custom** — a name somebody brings, routed **only once verified** by DNS
  lookup. Routing an unverified claim would let somebody take traffic for a name
  they do not control. The verification target is stored on the row, so changing
  the platform target shows up as a domain needing re-verification rather than
  silently invalidating one that was proven against the old target.

### Certificates

**Ozymandis does not obtain certificates.** It writes one annotation naming an
ACME resolver and the ingress controller does the rest.

```go
type Certificate string
const (
    CertNone   Certificate = ""        // plain HTTP, no TLS block written
    CertIssued Certificate = "issued"  // its own certificate, for this name alone
)
```

**Two values, and the third one was deleted deliberately.** There used to be a
case for "covered by a wildcard the controller already holds", and it was the bug:
a managed hostname served from a wildcard could never obtain a certificate of its
own — there was no branch that reached `CertIssued` for one. On an install whose
controller issues per host, that left every platform subdomain served under the
controller's built-in self-signed certificate, *with the deploy green and nothing
anywhere reporting it.* More generally, a wildcard covers the names it was issued
for and nothing else, so routing a name outside it still completed the request
under the wrong certificate — which a browser reports as the site being
impersonated. **That is worse than no TLS, because no TLS at least fails
honestly.** Deleting the value rather than adding a third case is what keeps it
from coming back: there is no longer a way to express it.

So one fact decides everything: does the install have a resolver? If yes, every
routed hostname is `CertIssued` — platform subdomain and customer domain alike,
one path, not two.

`HostSpec` pairs the name with its certificate source rather than keeping two
parallel lists, because *a name whose certificate is decided somewhere else is a
name served under whatever certificate happened to be nearest* — the bug the type
exists to make unrepresentable.

### The Ingress

`internal/orchestrator/k8s/ingress.go`. `ingressClassName` is left unset so the
cluster's default applies — naming a class would hard-code which controller is
installed. Annotations are written **only when the corresponding setting is on**,
so an install running neither controller gets an Ingress with no annotations
rather than configuration for something that is not there:

| Annotation | Written when |
|---|---|
| `external-dns.alpha.kubernetes.io/target` | the app is CNAME-only *and* the install has a target |
| `traefik.…/router.entrypoints: websecure` | the app is HTTPS-only |
| `traefik.…/router.tls.certresolver` | there is at least one issued host *and* a resolver is configured |

**The TLS block names no `secretName`, and that is the point.** A `secretName` is
how an Ingress points at a certificate somebody else put in the namespace — the
shape cert-manager needs. Traefik's ACME resolver does not work that way: it keeps
what it issues in its own store (`acme.json`) and serves it directly. Naming a
Secret would be worse than redundant — Traefik would look for a Secret nothing
ever creates, find it missing, and fall back to its built-in self-signed
certificate, while the Ingress, the pod and the deploy all stayed green.

### The failure Ozymandis cannot detect

`IssuerRef` names a resolver in *Traefik's* static configuration. Nothing here can
check that a resolver by that name exists. **Name one that does not, and the
annotation is written, the Ingress is accepted, the deploy is green, and every
visitor gets the controller's own certificate.** Silent by construction — which is
why the resolver is install-level configuration that no tenant can set, why the
name is printed at startup so it is checkable against the controller's config, and
why the README tells you to verify the *issuer on the served certificate* rather
than the status code:

```bash
echo | openssl s_client -connect your.app.example:443 \
  -servername your.app.example 2>/dev/null | openssl x509 -noout -issuer
```

An app asking for a certificate on an install with no resolver is **refused at
validation**, not quietly downgraded to plain HTTP. The downgrade would be
invisible: the deploy succeeds, the Ingress routes, and the failure surfaces as a
certificate warning to whoever brought the domain. Refusing puts it in front of
the person who can fix it.

---

## 7. Volumes, secrets, backups

### Volumes

`ReadWriteOnce` claims, which mounts on one node at a time. Two consequences are
enforced rather than discovered:

- **A workload with storage runs one replica.** Refused at validation, not left to
  the cluster, where it appears as a pod stuck `Pending` with the reason somewhere
  nobody is looking. `Scale` refuses for the same reason, and says *storage* is the
  reason rather than replicas.
- **Attaching storage makes deploys recreate rather than roll.**

Sizes are held in **bytes**, not Kubernetes quantity strings, so comparing a new
size against the old is arithmetic — and that comparison is the whole expansion
rule. Volumes only grow: Kubernetes cannot shrink a claim, and neither can
anything else, because the filesystem on it may be full.

`FSGroup` is what makes storage and the security posture compatible at all: a
freshly provisioned claim belongs to root, so a container running as anyone else
cannot write to it.

### Secrets

`internal/secret` — AES-256-GCM, one key from `OZYMANDIS_SECRET_KEY`
(base64, 32 bytes exactly). Nothing shorter is accepted: 128-bit is fine
cryptographically, but accepting two lengths means one of them gets chosen by
accident. An **all-zero key is refused** — that is what a caller gets from an unset
variable decoded anyway, or a placeholder nobody replaced, and it encrypts
perfectly well, which is exactly why it has to be rejected.

Sealed values are opened only to apply, and reach the cluster as a Kubernetes
Secret via `envFrom` rather than as literals in the pod template. A database dump
is then useless without the key, which lives in configuration rather than in the
database it protects.

That design has one consequence worth stating, because getting it wrong is
invisible. `envFrom` names a Secret and does not carry its values, so rewriting
the Secret leaves the pod template byte-identical — and Kubernetes rolls a
Deployment when its **template** changes, not when something the template
*points at* changes. Left there, setting a secret returned success, stored the
new value, and left every running pod reading the old one, with no error
anywhere to say so.

So the revision annotation on the pod template (`specHash`, `internal/orchestrator/k8s/app.go`)
covers the **content** of the secrets as well as the fields you would expect.
What lands in the template is still only a digest — sixteen hex characters,
irreversible — so the property `envFrom` exists for is intact, and a changed
credential now rolls the pods that read it. The hash is length-prefixed per key
and value, because `{"AB":"C"}` and `{"A":"BC"}` otherwise agree, and sorted,
because an unsorted map would roll the app on every apply.

### Backups

`internal/backup`. The problem is specific: K3s stores volumes on `local-path` by
default — an app's data is a directory on one node's disk, unreplicated. Everything
else can be rebuilt from configuration. The contents of a volume cannot.

Two rules follow, and both are load-bearing:

1. **Backups go off the machine, always.** There is no local destination, and
   adding one would be adding the option most people would pick and the one that
   protects against nothing.
2. **Restoring is an ordinary operation, from the dashboard.** A backup nobody has
   restored from is a hypothesis, and this is the only way anybody finds out the
   backups were wrong while it is still cheap to find out.

S3-compatible destination, encrypted with a sealed password, scheduled with
retention, run as a `TaskSpec` — which mounts an *existing* app volume by name and
cannot provision one, because a task that could provision would be a second place
volumes come from. An app with no storage is told `ErrNothingToBackUp` rather than
handed a nightly job that backs up an empty directory and reports success.

---

## 8. Cluster operations

`internal/cluster` is deliberately outside the orchestrator seam. `K3S_URL` and
`K3S_TOKEN` are K3s specifics, and putting them behind an interface whose whole
value is not knowing which Kubernetes it is talking to would be the first crack in
it — every future distribution would widen an interface no orchestrator method
needs. Join tokens are sealed; nodes carry a pool label (`ozymandis/pool`) whose
allowed character set is also what makes it safe to interpolate into a command
somebody pastes into a root shell.

`NodeManager` is separate from `ClusterInspector`, and that split is the point:
inspection is read-only and can be served from a cache, a read replica, or a
restricted credential, whereas evicting pods across every namespace and deleting a
node object is the one thing that cannot. An implementation that cannot do it
simply does not satisfy the interface, and the surface offering it stays off rather
than failing when somebody presses it.

`Drain` **requests** eviction rather than deleting: it respects disruption budgets,
so a pod whose budget would be violated stays and is reported. It returns once the
evictions are requested, not once they have finished — the caller watches the node
empty rather than holding a request open for however long that takes. `DeleteNode`
removes the node object and does not touch the machine; shutting it down is a
separate act performed where the machine is.

---

## 9. The four seams

| # | Package | Interface | Engine ships | A wrapper supplies |
|---|---|---|---|---|
| 1 | `orchestrator` | `Orchestrator` | single K8s cluster, plus a no-op | multi-cluster placement, per-owner scheduling |
| 2 | `identity` | `Provider` | single owner, bearer token, sessions | tokens resolved to an account |
| 3 | `web` | `SlotProvider` | plain navigation | account switcher, usage, billing |
| 4 | `notify` | `Mailer` | SMTP, Resend, log | whatever sends the org's mail |

Two rules keep seam 1 usable, and they are the ones to defend in review:

1. **No Kubernetes types cross the boundary.** Everything is a plain Go type
   defined in `orchestrator`. Callers never import `client-go`, and a
   non-Kubernetes backend stays possible.
2. **Naming policy lives with the caller.** The caller decides namespaces and
   resource names; implementations only apply them. A wrapping layer needing
   different naming is what this buys.

Seam 2 is worth a note, because it looks like it was breached and was not. The
engine now ships accounts, teams and sessions — but `account.Sessions` is an
*implementation* of `identity.Provider`, not a replacement for it. The engine's own
sign-in goes through the same interface a wrapping application swaps out, which is
why adding all of that changed no handler, no store query, and no orchestrator
call.

---

## 10. Failure semantics, collected

The recurring shapes, in one place, because they are the load-bearing part of the
design:

| Situation | Behaviour | Why |
|---|---|---|
| Cluster unreachable at startup | boot with a no-op orchestrator, warn | fix the kubeconfig from the dashboard, not from a log line you have to find |
| No secret key | the surface is not mounted at all | a credential that cannot be sealed is not held |
| No registry / no builder | Git source listed unavailable | "not set up" ≠ "broken" |
| Build fails | nothing applied; previous image keeps serving | redeploying the old image under a row that says it built the new commit is a lie the history would keep telling |
| Release fails | `apply` never called; previous image keeps serving | the veto is the feature |
| Process dies mid-build | reconciler settles it as failed, **with a reason** | a build has no resumable state; the honest recovery is to notice and say so |
| Cluster unreachable mid-reconcile | leave the build alone, retry next pass | an unreachable API server must not become a wave of failed deployments |
| Registry unreachable at deploy | no pull credential, deploy proceeds | a registry problem must not stop apps that do not use the registry |
| Named ACME resolver does not exist | **undetectable here** — verify the served issuer | see §6 |
| Host wants a certificate, no resolver | refused at validation | the downgrade would be invisible |
| Unauthenticated request | uniform rejection, no reason given | which credential was missing is information about what would have worked |
| A secret is changed | the pods that read it roll | `envFrom` names a Secret rather than carrying it, so nothing else would change the pod template — and a credential live in the store but absent from the process reports no error at all |
| A dependency an app needs is down | readiness fails, pod leaves the Service, no restart | liveness would restart a process that is fine; the outage is elsewhere |
| An install cannot serve an endpoint | 503 with the reason, never 500 | "this install is not configured for that" is not an internal failure, and a client must not retry it forever |

---

## Reading order

For someone new to the codebase, this order gets you oriented fastest:

1. `cmd/ozymandis/main.go` — the whole wiring, ~450 lines with the reasoning inline.
2. `internal/orchestrator/orchestrator.go` — the contract everything below is
   written against.
3. `internal/app/service.go` — `apply`, then `Create`, then `Redeploy`.
4. `internal/app/build.go` + `internal/orchestrator/k8s/buildjob.go` — the pipeline.
5. `internal/orchestrator/k8s/ingress.go` — short, and the most subtle file here.

The package doc comments carry the reasoning; this document is a map of them, not
a replacement.
