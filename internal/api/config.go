package api

import (
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/kingzion24/ozymandis/internal/app"
	"github.com/kingzion24/ozymandis/internal/appspec"
)

// The two axes.
//
// The file is authoritative over what it can COMPLETELY describe, and has no
// say over anything else. That line — rather than "the file wins" — is what
// makes the model predictable, and it splits every field into one of two sets:
//
// DECLARATIVE AND CONVERGED HERE. [service], [health], [env]. The file is the
// truth and drift made in the dashboard is reverted. Plaintext env is fully
// declarative, keys absent from the file included, because the file CAN
// describe the complete set of plaintext variables and therefore owns them
// completely.
//
// DECLARATIVE, APPLIED BY A DEPLOY. [build]. The file owns these too, but
// changing an app's source or image is a deploy-path operation — it needs an
// image pulled or built and a deployment row recording which commit produced
// what runs. A converge reconciles settings against a workload that already
// exists; it is not the verb for changing what the workload IS. Reported here
// with that reason, applied by `oz deploy`.
//
// OPERATIONAL. Replicas, and volume sizes. Never converged. Replicas is the one
// field a person changes while something is on fire, and its correct value is a
// property of right now rather than of the commit — so a deploy from a stale
// checkout that says 2 must not undo an emergency scale to 10. Applied only at
// creation, or when the caller passes scale=true. Volume sizes are operational
// for the same reason and only ever grow.
//
// NEITHER. Secrets and custom domains. The file cannot describe either
// completely — a sealed value has no read path, and a verified custom domain is
// a fact about DNS rather than about the commit — so it owns neither. Domains
// in the file are ADDITIVE: a host it names that the app lacks is added, and a
// host the app has that the file lacks is left alone and reported. Deleting a
// line from a config file must not silently drop a certificate and start
// returning NXDOMAIN to live traffic.

// Change is one thing a converge would do, or did.
type Change struct {
	// Field is a dotted path into the spec: "service.port", "env.LOG_LEVEL".
	Field string `json:"field"`

	// From and To are rendered values. Strings rather than typed values because
	// this is for a person reading a terminal, and a diff that needs a schema to
	// interpret is not a diff.
	From string `json:"from"`
	To   string `json:"to"`

	// Axis is "declarative" or "operational", so a caller can explain WHY a
	// change is being skipped rather than only that it is.
	Axis string `json:"axis"`

	// Skipped marks a difference this converge will NOT apply, with Reason
	// saying why. A converge that silently declined to do something the file
	// asked for would be indistinguishable from one that did it.
	Skipped bool   `json:"skipped,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

const (
	axisDeclarative = "declarative"
	axisOperational = "operational"
)

// ConfigResult is what a converge did, or would do.
type ConfigResult struct {
	Changes []Change `json:"changes"`

	// DryRun reports whether anything was actually applied. Carried explicitly
	// so a client cannot misread a preview as a result.
	DryRun bool `json:"dry_run"`

	// UntrackedDomains are hostnames the app has that the file does not name.
	// Reported on every run rather than only when they change, because the file
	// is not a faithful description of the app's routing and somebody reading
	// only the file would never learn that.
	UntrackedDomains []string `json:"untracked_domains,omitempty"`
}

// configGet returns the app as a spec.
//
// Round-trippable on purpose: what comes back here, written to a file and sent
// to configPut, must be a no-op. That is the property that makes `oz config
// show > ozymandis.toml` a safe way to adopt an app somebody created in the
// dashboard.
func (s *Server) configGet(w http.ResponseWriter, r *http.Request) {
	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, specFor(a))
}

// specFor renders an app as the file that would produce it.
func specFor(a app.App) appspec.Spec {
	spec := appspec.Spec{Name: a.Name}

	if a.Repo.Set() {
		b := &appspec.Build{Repo: strPtr(a.Repo.URL)}
		if a.Repo.Branch != "" {
			b.Branch = strPtr(a.Repo.Branch)
		}
		if a.Repo.Subdir != "" {
			b.Subdir = strPtr(a.Repo.Subdir)
		}
		spec.Build = b
	} else if a.Image != "" {
		spec.Build = &appspec.Build{Image: strPtr(a.Image)}
	}

	spec.Service = &appspec.Service{Port: int32Ptr(a.Port), Internal: boolPtr(a.Internal)}

	if a.HealthPath != "" {
		spec.Health = &appspec.Health{
			Path: strPtr(a.HealthPath), Liveness: boolPtr(a.Liveness),
		}
	}

	if a.ReleaseCommand != "" {
		spec.Deploy = &appspec.Deploy{ReleaseCommand: strPtr(a.ReleaseCommand)}
	}

	spec.Scale = &appspec.Scale{Replicas: int32Ptr(a.Replicas)}

	// Plaintext only. A sealed value has no read path, so it cannot appear —
	// which is also why the rendered file is not a complete description and why
	// `oz config show` prints a note saying so.
	env := map[string]string{}
	for _, v := range a.Variables {
		if !v.Secret {
			env[v.Key] = v.Value
		}
	}
	if len(env) > 0 {
		spec.Env = env
	}

	for _, vol := range a.Volumes {
		spec.Volumes = append(spec.Volumes, appspec.Volume{
			Name: vol.Name, Path: vol.MountPath,
			Size: strPtr(fmt.Sprintf("%d", vol.SizeBytes)),
		})
	}

	return spec
}

// ConfigRequest is the body of PUT /apps/{name}/config.
type ConfigRequest struct {
	Spec appspec.Spec `json:"spec"`

	// Scale opts the operational axis in for this one request. `oz deploy
	// --scale`. Absent or false leaves the running replica count alone whatever
	// the file says.
	Scale bool `json:"scale,omitempty"`
}

// configPut converges an app onto a spec.
func (s *Server) configPut(w http.ResponseWriter, r *http.Request) {
	var in ConfigRequest
	if err := decodeJSON(r, &in); err != nil {
		writeInvalid(w, "that body is not the JSON this expects: "+err.Error())
		return
	}
	if err := in.Spec.Validate(); err != nil {
		writeInvalid(w, err.Error())
		return
	}

	a, err := s.lookup(r)
	if err != nil {
		writeServiceError(w, s.log, "get app", err)
		return
	}

	hosts, err := s.hostsOf(r, a)
	if err != nil {
		writeServiceError(w, s.log, "read networking", err)
		return
	}

	dryRun := r.URL.Query().Get("dry_run") == "true"
	result := plan(a, in.Spec, hosts, in.Scale, s.nets != nil)
	result.DryRun = dryRun

	if dryRun {
		// Nothing is applied and nothing is written. The whole point is that a
		// caller can see what a converge would revert BEFORE it reverts it.
		writeJSON(w, s.log, http.StatusOK, result)
		return
	}

	if err := s.apply(r, a, in.Spec, result); err != nil {
		writeServiceError(w, s.log, "converge app", err)
		return
	}
	writeJSON(w, s.log, http.StatusOK, result)
}

// plan computes what a converge would do, without doing any of it.
//
// Pure, and the single source of truth for both the dry run and the real one:
// apply consumes exactly this. A preview computed by different code from the
// thing it previews is a preview that can lie, and this is the one endpoint
// whose entire value is that it does not.
//
// hosts is the app's current custom hostnames and canAddDomains whether this
// install has a networking surface at all. Both are passed in rather than read
// here, so this stays a pure function of its arguments — which is what lets the
// axis rules be tested exhaustively without a database or a cluster, including
// the degraded install that has no domain support.
func plan(
	a app.App, spec appspec.Spec, hosts []string, withScale, canAddDomains bool,
) ConfigResult {
	var changes []Change

	// --- Declarative ---

	// [build] is DEFERRED, and on principle rather than for want of a method.
	//
	// Changing an app's source or image is a deploy-path operation: a new image
	// has to be pulled or built, and the deployment history has to record which
	// commit produced what is running. A config converge is none of those
	// things — it reconciles settings against a workload that already exists.
	// Routing an image change through here would produce a deployment nobody
	// triggered, with no build behind it and no row explaining it.
	//
	// So the seam is "does this belong to the deploy", not "does this happen to
	// have a service method". That line holds as the engine grows; the other
	// one moves every time somebody adds a setter.
	//
	// Reported rather than omitted, because omitting would make a file that
	// disagrees with its app look like one that matches — somebody would edit
	// the image, converge, see no complaint, and believe it took effect.
	if b := spec.Build; b != nil {
		if b.Image != nil && *b.Image != a.Image && !a.Repo.Set() {
			changes = append(changes, viaDeploy("build.image", a.Image, *b.Image))
		}
		if b.Repo != nil && *b.Repo != a.Repo.URL {
			changes = append(changes, viaDeploy("build.repo", a.Repo.URL, *b.Repo))
		}
		if b.Branch != nil && *b.Branch != a.Repo.Branch {
			changes = append(changes, viaDeploy("build.branch", a.Repo.Branch, *b.Branch))
		}
		if b.Subdir != nil && *b.Subdir != a.Repo.Subdir {
			changes = append(changes, viaDeploy("build.subdir", a.Repo.Subdir, *b.Subdir))
		}
	}

	// [service] is declarative and applied. The port is routing configuration —
	// where Kubernetes sends traffic, not what the process listens on — so it
	// changes live without a rebuild. Nothing passes a port to a build.
	if sv := spec.Service; sv != nil {
		if sv.Port != nil && *sv.Port != a.Port {
			changes = append(changes, decl("service.port",
				fmt.Sprint(a.Port), fmt.Sprint(*sv.Port)))
		}
		if sv.Internal != nil && *sv.Internal != a.Internal {
			changes = append(changes, decl("service.internal",
				fmt.Sprint(a.Internal), fmt.Sprint(*sv.Internal)))
		}
	}

	// [deploy] release_command is declarative and converged here. It is not a
	// build concern despite naming a command: it changes what a deploy DOES,
	// not what the app IS, and it takes effect on the next deploy either way.
	if d := spec.Deploy; d != nil && d.ReleaseCommand != nil {
		if *d.ReleaseCommand != a.ReleaseCommand {
			c := decl("deploy.release_command", a.ReleaseCommand, *d.ReleaseCommand)

			// The second of the three surfaces carrying the no-volumes rule —
			// the others are the doc comment on runRelease and the preamble in
			// the release log itself. Said HERE because this is the moment
			// somebody configures it, which is the only moment they can still
			// design around it. Learning it from "No such file or directory"
			// three deploys later is the failure this prevents.
			if *d.ReleaseCommand != "" && len(a.Volumes) > 0 {
				c.Reason = "note: this app has storage, and a release runs beside " +
					"the app rather than in it — its volumes are not mounted. " +
					"A release that reads or writes them will not find them."
			}
			changes = append(changes, c)
		}
	}

	if h := spec.Health; h != nil {
		if h.Path != nil && *h.Path != a.HealthPath {
			changes = append(changes, decl("health.path", a.HealthPath, *h.Path))
		}
		if h.Liveness != nil && *h.Liveness != a.Liveness {
			changes = append(changes, decl("health.liveness",
				fmt.Sprint(a.Liveness), fmt.Sprint(*h.Liveness)))
		}
	}

	// Plaintext env is fully declarative: the file can describe the complete
	// set, so keys it does not name are removed. Secrets are untouched and
	// unreachable — they are not in spec.Env and cannot be.
	if spec.Env != nil {
		current := map[string]string{}
		for _, v := range a.Variables {
			if !v.Secret {
				current[v.Key] = v.Value
			}
		}
		for _, k := range spec.EnvKeys() {
			if cur, ok := current[k]; !ok || cur != spec.Env[k] {
				changes = append(changes, decl("env."+k, cur, spec.Env[k]))
			}
		}
		var gone []string
		for k := range current {
			if _, ok := spec.Env[k]; !ok {
				gone = append(gone, k)
			}
		}
		sort.Strings(gone)
		for _, k := range gone {
			changes = append(changes, decl("env."+k, current[k], ""))
		}
	}

	// --- Operational ---

	if sc := spec.Scale; sc != nil && sc.Replicas != nil && *sc.Replicas != a.Replicas {
		c := Change{
			Field: "scale.replicas",
			From:  fmt.Sprint(a.Replicas), To: fmt.Sprint(*sc.Replicas),
			Axis: axisOperational,
		}
		if !withScale {
			c.Skipped = true
			c.Reason = "replicas is operational: it is left as it is running so a " +
				"deploy cannot undo a scale made during an incident. " +
				"Pass --scale to apply the file's value."
		}
		changes = append(changes, c)
	}

	// --- Neither axis: domains are additive ---
	//
	// Added when the file names one the app lacks; never removed when the app
	// has one the file does not. The asymmetry is deliberate and is reported
	// rather than hidden — see UntrackedDomains.

	have := map[string]bool{}
	for _, h := range hosts {
		have[h] = true
	}
	named := map[string]bool{}
	for _, d := range spec.Domains {
		named[d.Host] = true
		if have[d.Host] {
			continue
		}
		if !canAddDomains {
			// This install has no networking surface, so AddDomain would be a
			// call on a nil interface. Reported as unavailable rather than as a
			// change apply will make — the alternative is a plan promising
			// something that panics.
			changes = append(changes, unavailable("domains", "", d.Host,
				"this install has no custom-domain surface, so the hostname in "+
					"the file cannot be added here"))
			continue
		}
		changes = append(changes, decl("domains", "", d.Host))
	}

	var untracked []string
	for _, h := range hosts {
		if !named[h] {
			untracked = append(untracked, h)
		}
	}
	sort.Strings(untracked)

	if changes == nil {
		changes = []Change{}
	}
	return ConfigResult{Changes: changes, UntrackedDomains: untracked}
}

func decl(field, from, to string) Change {
	return Change{Field: field, From: from, To: to, Axis: axisDeclarative}
}

// viaDeploy is a difference that belongs to the deploy rather than to a config
// converge.
//
// Distinct from a skipped operational change, which is policy the caller
// overrides with --scale. This one is a boundary: the change is real and will
// happen, through a different verb. The wording matters — "not supported" reads
// as a permanent wall and would send somebody looking for a workaround, when
// what they need is `oz deploy`.
func viaDeploy(field, from, to string) Change {
	return Change{
		Field: field, From: from, To: to, Axis: axisDeclarative,
		Skipped: true,
		Reason: "recorded, not applied here: " + field + " changes what runs, " +
			"so it is applied by a deploy rather than by a config converge. " +
			"Run `oz deploy` to pick it up.",
	}
}

// unavailable is a difference this install has no surface for at all.
//
// Distinct again from viaDeploy: nothing the caller does will apply this one,
// because the capability is not wired on this install. Saying so beats
// reporting a change that silently never happens.
func unavailable(field, from, to, why string) Change {
	return Change{
		Field: field, From: from, To: to, Axis: axisDeclarative,
		Skipped: true, Reason: why,
	}
}

// apply performs the changes plan computed.
//
// It walks the plan rather than re-deriving anything from the spec, so the
// preview and the action cannot disagree — including about what to skip.
func (s *Server) apply(
	r *http.Request, a app.App, spec appspec.Spec, result ConfigResult,
) error {
	ctx := r.Context()
	owner := ownerOf(r).ID

	for _, c := range result.Changes {
		if c.Skipped {
			continue
		}

		switch {
		case c.Field == "health.path" || c.Field == "health.liveness":
			path, liveness := a.HealthPath, a.Liveness
			if spec.Health != nil && spec.Health.Path != nil {
				path = *spec.Health.Path
			}
			if spec.Health != nil && spec.Health.Liveness != nil {
				liveness = *spec.Health.Liveness
			}
			if err := s.apps.SetHealth(ctx, owner, a.Name, path, liveness); err != nil {
				return err
			}

		case c.Field == "service.port" || c.Field == "service.internal":
			port, internal := a.Port, a.Internal
			if spec.Service != nil && spec.Service.Port != nil {
				port = *spec.Service.Port
			}
			if spec.Service != nil && spec.Service.Internal != nil {
				internal = *spec.Service.Internal
			}
			if err := s.apps.SetService(ctx, owner, a.Name, port, internal); err != nil {
				return err
			}

		case c.Field == "deploy.release_command":
			err := s.apps.SetReleaseCommand(ctx, owner, a.Name, *spec.Deploy.ReleaseCommand)
			if err != nil {
				return err
			}

		case c.Field == "scale.replicas":
			if _, err := s.apps.Scale(ctx, owner, a.Name, *spec.Scale.Replicas); err != nil {
				return err
			}

		case strings.HasPrefix(c.Field, "env."):
			key := strings.TrimPrefix(c.Field, "env.")

			// Presence in the map decides, never the rendered value. `KEY = ""`
			// is a variable set to the empty string and `KEY` absent is a
			// variable to remove; both render as To == "" in the diff, so
			// branching on that would delete a variable somebody deliberately
			// blanked.
			value, keep := spec.Env[key]
			if !keep {
				if err := s.apps.DeleteVariable(ctx, owner, a.Name, key); err != nil {
					return err
				}
				continue
			}
			err := s.apps.SetVariable(ctx, owner, a.Name, app.VariableInput{
				Key: key, Value: value, Secret: false,
			})
			if err != nil {
				return err
			}

		case c.Field == "domains":
			if err := s.nets.AddDomain(ctx, owner, a.Name, c.To); err != nil {
				return err
			}
		}
	}

	return nil
}

// hostsOf lists the app's custom hostnames.
//
// Empty when this install has no networking surface, which makes the domain
// rules degrade cleanly rather than fail: a spec naming a domain on such an
// install reports the change as skipped instead of erroring on an endpoint
// that does not exist here.
func (s *Server) hostsOf(r *http.Request, a app.App) ([]string, error) {
	if s.nets == nil {
		return nil, nil
	}
	net, err := s.nets.Networking(r.Context(), ownerOf(r).ID, a.Name)
	if err != nil {
		return nil, err
	}
	hosts := make([]string, 0, len(net.Custom))
	for _, c := range net.Custom {
		hosts = append(hosts, c.Host)
	}
	return hosts, nil
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }
func int32Ptr(i int32) *int32 { return &i }
