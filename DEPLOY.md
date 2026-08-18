# Deploying an app

The full path: what a deploy actually does, how to run one, how to put several
apps from one repository into a single project, and how to read the logs when
something goes wrong.

For installing Ozymandis itself, see [USER_GUIDE.md](USER_GUIDE.md).

---

## Contents

- [What a deploy does](#what-a-deploy-does)
- [Deploying from the CLI](#deploying-from-the-cli)
- [Deploying from CI](#deploying-from-ci)
- [Several apps from one repository](#several-apps-from-one-repository)
- [Projects: grouping apps in the dashboard](#projects-grouping-apps-in-the-dashboard)
- [Reading the logs](#reading-the-logs)
- [When a deploy goes wrong](#when-a-deploy-goes-wrong)

---

## What a deploy does

A deploy moves through these stages. Knowing which one you are in is most of
troubleshooting, because each fails differently.

```
  1. Converge config     ozymandis.toml -> the app's stored config
  2. Build               git apps only: clone, build, push to the registry
  3. Release command     optional: runs against the NEW image, before traffic
  4. Apply               write the Deployment; Kubernetes rolls the pods
  5. Rollout             replicas move to the new template, old ones retire
```

**1. Converge.** `oz deploy` sends your `ozymandis.toml` to the server first, so
the deploy that follows uses the config in your checkout. `--no-config` skips
this and redeploys what the server already has.

**2. Build.** For a `git` app, a Job clones the repository at the branch head,
builds an image, and pushes it to this install's registry. An `image` app skips
straight to step 4. A build has its own status — `running`, `succeeded`,
`failed` — and its own log, separate from the app's.

**3. Release command.** If `[deploy] release_command` is set, it runs against
the new image *before* any traffic moves to it. This is where migrations go. If
it fails, **the deploy stops** and nothing is rolled — that is the point of it
running here rather than as part of the app's startup.

**4. Apply.** The Deployment is written with the new image, environment, volumes
and probes. The pod template carries a hash of the secret material, so changing
a secret is itself a change to the template.

**5. Rollout.** Kubernetes starts new pods and retires old ones. A new pod takes
traffic only once its readiness probe passes.

### Deployment statuses

| Status | Means |
|---|---|
| `running` | In progress right now |
| `active` | The one currently serving |
| `superseded` | Replaced by a later deploy — **this worked**, it is history |
| `failed` | Did not make it |

`superseded` is deliberately distinct from `failed`: a deployment that was
replaced did its job, and reading a history of failures where none happened is
worse than no history at all.

---

## Deploying from the CLI

From a directory with an `ozymandis.toml`:

```sh
oz deploy
```

| Flag | What it does |
|---|---|
| `--dry-run` | Show what would change and stop |
| `--watch` | Wait for the rollout to finish; exit non-zero if it fails |
| `--no-config` | Skip the converge, just redeploy |
| `--scale` | Also apply `[scale] replicas` from the file |
| `--app NAME` | Deploy an app other than the file's |

### Use `--watch` in anything automated

Without it, `oz deploy` returns as soon as the deploy is accepted, which is not
the same as it having worked. `--watch` waits for the rollout to genuinely
complete — every replica on the new template, and available — rather than for
one pod to report ready, which a rolling update satisfies while the old version
is still serving.

That distinction is why a deploy can look green and still leave old code
running.

---

## Deploying from CI

Mint a token, store it as a repository secret, and either call the API or
install `oz`.

**With the API** — no binary to install:

```yaml
- name: Deploy
  env:
    OZ_TOKEN: ${{ secrets.OZYMANDIS_TOKEN }}
  run: |
    curl -fsSL -X POST \
      -H "Authorization: Bearer $OZ_TOKEN" \
      https://ozymandis.example.com/api/v1/apps/web/deploy
```

That starts the deploy and returns. To gate the job on the result, poll the
deployment and check `rollout_complete` on the app's status, or use `oz deploy
--watch`, which does it for you and exits non-zero on failure.

**Print the build log when it fails.** This is the difference between "the
deploy failed" and the compiler error that caused it:

```sh
curl -fsSL -H "Authorization: Bearer $OZ_TOKEN" \
  "$BASE/api/v1/apps/web/deployments/$DEPLOY_ID/build?tail=200"
```

Deploy ids come from `GET /api/v1/apps/web/deployments` — the first element of
the `deployments` array is the most recent.

---

## Several apps from one repository

This is the ordinary case for anything with more than one process — a web
service, a worker, a background consumer. **One repository, several apps, one
app per directory.**

Each app is a separate app in Ozymandis, pointing at the same repository with a
different `subdir`:

```toml
# services/web/ozymandis.toml
name = "web"
[build]
repo   = "ssh://git@github.com/you/your-repo.git"
branch = "main"
subdir = "services/web"
```

```toml
# services/worker/ozymandis.toml
name = "worker"
[build]
repo   = "ssh://git@github.com/you/your-repo.git"
branch = "main"
subdir = "services/worker"
[service]
internal = true          # a worker has nothing to route to it
```

Each builds only its own subdirectory, gets its own image, its own namespace,
its own secrets, and its own scale.

### Two environments from one repository

Point them at different branches. A production app on `main` and a staging app
on `staging` are two apps with different names, different secrets, and no way
to reach each other:

| App | Branch | Subdir |
|---|---|---|
| `web` | `main` | `services/web` |
| `web-uat` | `staging` | `services/web` |

Because the branch decides the environment, a deploy triggered from `staging`
cannot touch production — the app it deploys is a different app.

### Deploy keys are per app

Each app needs its own deploy key for a private repository:

```sh
curl -X POST -H "Authorization: Bearer $TOKEN" \
  https://ozymandis.example.com/api/v1/apps/web/deploy-key
```

Add the returned public key to **the repository's** deploy keys on GitHub, not
to your account. Six apps from one repository means six keys on that one
repository, which is correct: a key is per app so that revoking one app's access
does not revoke another's.

---

## Projects: grouping apps in the dashboard

Projects are the dashboard's grouping — the "category" that holds the apps
belonging to one system. A project has a **canvas**: a view drawing its apps
and what connects them, so a repository's web service, worker and datastore
read as one system rather than as unrelated rows in a list.

```
/projects              the projects this team has, with app counts
/projects/{slug}       the canvas
```

Create one from the projects page, then create apps into it. Cards can be
dragged, and the arrangement is the **team's**, not yours alone — a canvas two
people arrange differently is two pictures of one system, and neither can be
pointed at in a conversation. `Arrange` re-lays it out from scratch.

### Moving an app between projects

An app's **Settings** tab has a **Project** panel: pick a canvas, press Move.

The control appears only when there is somewhere to move to — with a single
project, a select whose only option is where the app already sits reads as a
broken control rather than a choice.

Moving is **not a deploy**. The namespace, the image and the running pods are
untouched; a project is only how apps are grouped on screen. The card's saved
position is forgotten in the move, so it is laid out with the rest of its new
canvas instead of landing wherever the old arrangement had put it — possibly on
top of another app, or outside the visible area.

> [!NOTE]
> Grouping is **manual**: apps are not gathered into a project automatically by
> sharing a repository URL. Two apps built from the same repo are related only
> because you put them in the same project.
>
> Nothing is ever stranded, though. An app with no project — one created before
> projects existed, or whose project was deleted — is adopted by the default
> project the next time anything reads it, so it always appears on some canvas.

---

## Reading the logs

### From the CLI

```sh
oz logs --app web              # last 200 lines
oz logs --app web -n 1000      # more history
oz logs --app web -f           # follow
```

`-f` streams until you stop it. For several apps from one repository, run one
per terminal — there is no combined stream across apps.

### From the dashboard

The app's **Logs** tab shows live output with search.

Each deployment opens a **sheet** with three views, named after what produced
the output rather than where it is read from — because that is the actual
question when a deploy goes wrong: did it fail to build, fail to start, or start
and serve errors.

| View | What it shows | Use it when |
|---|---|---|
| **Build** | The image build: clone, dependencies, compile, push | The deploy never produced an image |
| **Deploy** | The container's own output | It built, but the app will not start or stay up |
| **HTTP** | Requests the ingress recorded — timeline chart, method filter, search, paged | It is running and serving the wrong answers |

An app deployed from an image has no build, and the page says so rather than
showing an empty log.

There is also a **previous run** toggle. When a container has restarted, its
predecessor's output is the only place the reason lives — the current container
started cleanly and knows nothing about why the last one died. This is the first
thing to check on a crash loop.

HTTP logs are opt-in per app and enabled from that tab.

### Which log answers which question

| Symptom | Look at |
|---|---|
| Deploy failed, no image | **Build** |
| `ImagePullBackOff` | The deploy's error — a registry credential, not a log |
| Pod starts then dies | **Deploy**, with **previous run** on |
| Pods never become ready | **Deploy** — the readiness path is returning non-200 |
| Running, wrong responses | **HTTP** |
| Deploy "succeeded", old code | Not a log: you waited for a pod, not the rollout. Use `--watch`. |

---

## When a deploy goes wrong

**Find out which stage failed first.** `oz releases --app web` gives you the
deployments and their statuses; the dashboard's deployment sheet gives you the
three logs above. A deploy that failed before the build has no build log, and
looking for one wastes time.

**A failed release command stops the deploy.** Nothing rolled, and the previous
version is still serving. Fix the migration and deploy again — you are not in a
half-applied state.

**A build that succeeded and a rollout that never finished** is almost always
readiness. The probe allows 3 seconds per check, every 10 seconds, and fails the
pod after 3 consecutive failures. A readiness check that queries a slow
dependency without its own timeout fails as a timeout rather than as the thing
it found.

**Roll back by deploying the previous commit.** There is no deploy-by-SHA yet:
deploys take a branch's head, so rolling back means moving the branch. For an
`image` app, set the previous tag and deploy.
