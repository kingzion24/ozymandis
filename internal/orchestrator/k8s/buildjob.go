package k8s

import (
	"fmt"
	"k8s.io/apimachinery/pkg/api/resource"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// The steps of a build, named so their logs can be told apart.
const (
	cloneContainer      = "clone"
	dockerfileContainer = "dockerfile"
	buildpackContainer  = "buildpack"
	pushContainer       = "push"
)

// The image artifacts a build step writes to the shared volume, to be pushed
// afterwards. Writing the image to disk rather than pushing it from the build
// step is the whole point of the split: the step that runs a repository's own
// code never holds the push credential, so there is nothing there for that code
// to read and use against another tenant's images. See pushStep.
const (
	// dockerfileArtifact is the OCI archive BuildKit writes.
	dockerfileArtifact = "/workspace/image.oci"
	// buildpackLayoutDir is the OCI layout the buildpack lifecycle writes.
	buildpackLayoutDir = "/workspace/layout"
)

// commitMarker prefixes the one line of the clone's output that is data rather
// than log. Reading it out of the stream avoids a second call into the cluster
// to ask what was checked out.
const commitMarker = "ozymandis-commit:"

// uidMarker prefixes the resolved numeric uid, the same way.
const uidMarker = "ozymandis-uid:"

// dockerfileMarker is written by the Dockerfile step when it has pushed an
// image, and read by the buildpack step as its instruction to do nothing.
//
// Kubernetes has no conditional step: every container in a pod runs. So the
// two strategies are both present and the second one checks whether the first
// already succeeded — which is also why they are init containers, since those
// run in order while ordinary containers run at once.
const dockerfileMarker = "/workspace/.dockerfile-built"

// buildVolume holds the checkout and the marker, shared by all three steps.
const buildVolume = "workspace"

// The user the Cloud Native Buildpacks lifecycle runs as, and which it stamps
// on the layers it writes.
//
// Read out of the pinned builder image rather than assumed: it publishes
// CNB_USER_ID=1001 and CNB_GROUP_ID=1000, and the uid is not the gid. Guessing
// 1000 for both fails with "unknown userid 1000" after the clone has already
// run, which is a slow way to find out.
//
// They are constants because the builder image is pinned. Unpinning it means
// reading these from the image, which is what the lifecycle's own tooling does.
const (
	cnbUID = 1001
	cnbGID = 1000
)

// buildUID is the user the clone and BuildKit steps run as. It shares the
// buildpack step's group so all three can write the shared volume.
const buildUID = 1000

// insecureHost names the one registry a build may reach over plain HTTP.
func insecureHost(req orchestrator.BuildRequest) string {
	if !req.Insecure {
		return ""
	}
	host, _, _ := strings.Cut(req.Image, "/")
	return host
}

// buildJob describes one build.
//
// Four steps, each its own image — clone, BuildKit, buildpacks, push — so none
// has to be a custom image this project builds and publishes, which would make
// running Ozymandis depend on Ozymandis having somewhere to push.
//
// The first three run a repository's own code (its Dockerfile, its buildpack
// detect and build) and hold NO registry credential: they write the finished
// image to the shared volume and stop. Only the last step, which runs no
// repository code, has the push credential, and all it does is ship the
// artifact. This is the deploy-key rule applied to the registry credential too
// — a secret a build step can read is a secret any Dockerfile can exfiltrate,
// and the install-wide push credential read by one tenant's build is every
// other tenant's images, so it lives only where repository code cannot reach.
func buildJob(
	name string, req orchestrator.BuildRequest, pushSecret, sshSecret string,
) *batchv1.Job {
	// Never retried. A build that failed will fail the same way, and a second
	// attempt would double the log, double the wait, and end where it started.
	// Kubernetes' own default is six.
	var backoff int32
	// Cleaned up by the caller, but this is the backstop for a Ozymandis that
	// stopped between creating the Job and deleting it.
	ttl := int32(3600)

	labels := map[string]string{
		orchestrator.LabelManagedBy: orchestrator.ManagedByValue,
		orchestrator.LabelOwner:     sanitiseLabel(string(req.Owner)),
		"ozymandis/build":           "true",
	}

	deadline := int64((buildTimeout + buildDeadlineSlack).Seconds())

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: BuildNamespace, Labels: labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoff,
			// The cluster's own cap, a little past the one this process
			// enforces. That one lives in a goroutine, and a process restarted
			// during a hung build takes it along: the Job then ran for ever,
			// and the reconciler left it alone for as long as it was running.
			ActiveDeadlineSeconds:   &deadline,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,

					// A build runs whatever a repository's Dockerfile says to
					// run. Without this it would run it with a token for the
					// Kubernetes API mounted at a well-known path — which is
					// the standard finding against in-cluster builders, and
					// why Tekton and Knative both disable it and bind their
					// build pods to a service account holding no permissions.
					ServiceAccountName:           BuildServiceAccount,
					AutomountServiceAccountToken: boolPtr(false),

					// The three steps share a volume and do not share a user:
					// the buildpack lifecycle runs as 1001 while the clone and
					// BuildKit run as 1000. An emptyDir belongs to root, so
					// without a common group the second step to touch it
					// cannot write. fsGroup is what Kubernetes provides for
					// exactly this, and all three are in group 1000.
					SecurityContext: &corev1.PodSecurityContext{
						FSGroup: int64Ptr(cnbGID),
					},
					Volumes: append([]corev1.Volume{
						{
							Name: buildVolume,
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
						{
							// Defined on the pod, but mounted into the push step
							// alone — see pushStep. The clone, Dockerfile and
							// buildpack steps never reference it.
							Name: "registry",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: pushSecret,
									Items: []corev1.KeyToPath{{
										Key: corev1.DockerConfigJsonKey, Path: "config.json",
									}},
								},
							},
						},
					}, sshVolumes(sshSecret)...),
					// Clone, build and push run in order: init containers run one
					// after another, and the push — the only step holding the
					// credential — is the final, ordinary container, so it runs
					// after every repository-code step has finished.
					InitContainers: []corev1.Container{
						bounded(cloneStep(req, sshSecret)),
						bounded(dockerfileStep(req)),
						bounded(buildpackStep(req)),
					},
					Containers: []corev1.Container{bounded(pushStep(req))},
				},
			},
		},
	}
}

// cloneStep fetches the repository.
//
// The URL and ref are passed as environment variables rather than interpolated
// into the script. A repository URL is somebody's input, and a value spliced
// into a shell command is a command they get to extend — validation upstream
// makes that unlikely, and this makes it impossible.
func cloneStep(req orchestrator.BuildRequest, sshSecret string) corev1.Container {
	c := corev1.Container{
		Name:  cloneContainer,
		Image: gitImage,
		Env: []corev1.EnvVar{
			{Name: "REPO_URL", Value: req.RepoURL},
			{Name: "REPO_REF", Value: req.Ref},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
# The checkout has to be group-writable, and that is not tidiness.
#
# This step runs as ` + itoa(buildUID) + ` and the buildpack step runs as ` + itoa(cnbUID) + `,
# sharing only the group. A default umask leaves the tree drwxr-xr-x, so the
# buildpack can read the source and cannot write beside it — and several
# buildpacks do exactly that: "go mod vendor", "npm install", anything that
# materialises dependencies into the source tree. The failure lands after
# detection has already passed and the toolchain has been installed, as a bare
# "permission denied" from a command the operator never typed.
#
# umask covers what the clone creates; the chmod covers what git sets modes on
# itself.
umask 002
echo "Cloning $REPO_URL at $REPO_REF"
git clone --depth 1 --branch "$REPO_REF" --single-branch "$REPO_URL" /workspace/src
chmod -R g+w /workspace/src
echo "` + commitMarker + `$(git -C /workspace/src rev-parse HEAD)"
echo "Cloned $(git -C /workspace/src rev-parse --short HEAD)"
`},
		SecurityContext: hardened(buildUID),
		VolumeMounts:    []corev1.VolumeMount{{Name: buildVolume, MountPath: "/workspace"}},
	}

	if sshSecret == "" {
		return c
	}

	// The deploy key, mounted read-only into THIS STEP ONLY.
	//
	// Only the clone: the BuildKit and buildpack steps run whatever the
	// repository tells them to, and a key readable there is a key any
	// Dockerfile can copy into a layer and publish with the image.
	//
	// StrictHostKeyChecking=accept-new rather than yes: pinning a host key
	// would mean shipping GitHub's, and it rotates. accept-new still refuses a
	// host whose key CHANGES mid-life, which is the attack worth catching here;
	// "no" would accept anything and is what most examples on the internet say.
	c.Env = append(c.Env, corev1.EnvVar{
		Name: "GIT_SSH_COMMAND",
		Value: "ssh -i /ssh/id -o IdentitiesOnly=yes " +
			"-o StrictHostKeyChecking=accept-new -o UserKnownHostsFile=/tmp/known_hosts",
	})
	c.VolumeMounts = append(c.VolumeMounts,
		corev1.VolumeMount{Name: sshVolume, MountPath: "/ssh", ReadOnly: true},
		// subPath so this replaces the one file rather than mounting over
		// /etc, which would hide everything else the image keeps there.
		corev1.VolumeMount{
			Name: passwdVolume, MountPath: passwdPath,
			SubPath: "passwd", ReadOnly: true,
		},
	)
	return c
}

// sshVolume is the name of the deploy-key volume.
const sshVolume = "deploy-key"

// passwdVolume carries one passwd line, and passwdPath is where it lands.
const (
	passwdVolume = "build-passwd"
	passwdPath   = "/etc/passwd"
)

// buildPasswdLine is the passwd entry the clone step runs as.
//
// ssh(1) calls getpwuid(getuid()) before it opens a socket and exits with "No
// user exists for uid N" when the id resolves to nothing. alpine/git ships
// entries for root, sshd, games, ntp, guest and nobody — not for buildUID — so
// every clone over ssh failed there, having sent no packet and read no key.
//
// What made it expensive to find is what git says next: "Could not read from
// remote repository. Please make sure you have the correct access rights."
// That is the message for a rejected key, so it sends you to check the deploy
// key, the repository, and the URL — none of which were ever consulted.
//
// Built from the same constants the SecurityContext uses, so a uid changed in
// one place cannot leave the other behind.
func buildPasswdLine() []byte {
	return []byte(fmt.Sprintf(
		"root:x:0:0:root:/root:/bin/sh\nbuilder:x:%d:%d:builder:/tmp:/sbin/nologin\n",
		buildUID, cnbGID))
}

// sshVolumes is the volume list for a build that has a deploy key, or nothing.
//
// Mode 0400: ssh refuses a private key any group or other can read, and the
// failure it gives — "permissions are too open" — is one people spend an
// afternoon on.
func sshVolumes(sshSecret string) []corev1.Volume {
	if sshSecret == "" {
		return nil
	}
	mode := int32(0o400)

	// 0444 rather than 0400: this one is read by the C library resolving the
	// process's own uid, and a passwd file the process cannot read is the
	// failure it exists to prevent. Its own volume rather than a second item
	// beside the key, so /ssh holds a key and nothing else.
	readable := int32(0o444)

	return []corev1.Volume{{
		Name: sshVolume,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  sshSecret,
				DefaultMode: &mode,
				Items:       []corev1.KeyToPath{{Key: "id", Path: "id"}},
			},
		},
	}, {
		Name: passwdVolume,
		VolumeSource: corev1.VolumeSource{
			Secret: &corev1.SecretVolumeSource{
				SecretName:  sshSecret,
				DefaultMode: &readable,
				Items:       []corev1.KeyToPath{{Key: "passwd", Path: "passwd"}},
			},
		},
	}}
}

// dockerfileStep builds a Dockerfile with rootless BuildKit, if there is one.
//
// --oci-worker-no-process-sandbox is what lets BuildKit run without the
// privileges it would otherwise need to set up its own sandbox. The trade is
// stated rather than hidden: builds are less isolated from each other than
// they would be under a privileged daemon, which is why this namespace runs
// nothing but these Jobs.
func dockerfileStep(req orchestrator.BuildRequest) corev1.Container {
	unconfined := corev1.SeccompProfileTypeUnconfined
	user := int64(buildUID)
	group := int64(cnbGID)

	return corev1.Container{
		Name:  dockerfileContainer,
		Image: buildkitImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			{Name: "BUILDKITD_FLAGS", Value: "--oci-worker-no-process-sandbox"},
			{Name: "ARTIFACT", Value: dockerfileArtifact},
			// The registry host, so a Dockerfile that pulls its base image FROM
			// this install's own plain-HTTP registry can still reach it. The
			// push is no longer this step's job, so there is no push credential
			// here and no push output option.
			{Name: "INSECURE_HOST", Value: insecureHost(req)},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
# A plain-HTTP registry has to be declared to buildkitd so a base image pulled
# FROM this install's own registry resolves. It is declared for pulls only; the
# push happens later, in a separate step that holds the credential.
if [ -n "$INSECURE_HOST" ]; then
  printf '[registry."%s"]\n  http = true\n' "$INSECURE_HOST" > /tmp/buildkitd.toml
  export BUILDKITD_FLAGS="$BUILDKITD_FLAGS --config /tmp/buildkitd.toml"
fi

cd /workspace/src/$SUBDIR
if [ ! -f Dockerfile ]; then
  echo "No Dockerfile here — falling back to buildpacks."
  exit 0
fi
echo "Building the Dockerfile for $IMAGE"
# Built to an OCI archive on the shared volume rather than pushed. This step
# runs the repository's own Dockerfile, so it deliberately holds no credential;
# the push step that does hold one runs no repository code.
buildctl-daemonless.sh build \
  --frontend dockerfile.v0 \
  --local context=. \
  --local dockerfile=. \
  --output type=oci,name=$IMAGE,dest=$ARTIFACT \
  --progress plain
touch ` + dockerfileMarker + `
echo "Built $IMAGE"
`},
		// This is BuildKit's own documented Kubernetes configuration for the
		// rootless image, and the deviations from it were tried and reverted
		// rather than assumed to be safe improvements.
		//
		// Unconfined seccomp is required: rootlesskit cannot fork under
		// RuntimeDefault, which fails with "fork/exec /proc/self/exe:
		// operation not permitted". That single requirement is what forces
		// this namespace to "privileged", since baseline forbids Unconfined.
		//
		// allowPrivilegeEscalation stays true and capabilities are not
		// dropped, which looks like an omission and is not. Rootless BuildKit
		// sets up its user namespace with newuidmap, a setuid helper; denying
		// privilege escalation sets no_new_privs and the build dies with
		// "newuidmap: operation not permitted". Dropping ALL takes CAP_SETUID
		// out of the bounding set and breaks the same call a second way. The
		// clone and buildpack steps take both, because they need neither.
		//
		// AppArmor is unconfined for the same class of reason: on a node that
		// enforces it, the default profile blocks the mounts BuildKit makes.
		// It is a no-op where AppArmor is not enforced.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:       &user,
			RunAsGroup:      &group,
			SeccompProfile:  &corev1.SeccompProfile{Type: unconfined},
			AppArmorProfile: &corev1.AppArmorProfile{Type: corev1.AppArmorProfileTypeUnconfined},
		},
		// No registry mount: this step runs the repository's Dockerfile, and a
		// credential it could read is one any RUN line could exfiltrate.
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
		},
	}
}

// hardened is the security context for the steps that need nothing special.
//
// Only the BuildKit step needs the seccomp exception; the clone and the
// buildpack lifecycle are ordinary programs. Giving them the same relaxation
// would widen it for no reason.
func hardened(user int64) *corev1.SecurityContext {
	return hardenedAs(user, cnbGID)
}

func hardenedAs(uid, gid int64) *corev1.SecurityContext {
	return &corev1.SecurityContext{
		RunAsUser:                &uid,
		RunAsGroup:               &gid,
		RunAsNonRoot:             boolPtr(true),
		AllowPrivilegeEscalation: boolPtr(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func boolPtr(b bool) *bool    { return &b }
func int64Ptr(i int64) *int64 { return &i }

// itoa keeps the lifecycle flags built from the same constants the security
// context uses, so the two cannot drift apart.
func itoa(i int64) string { return strconv.FormatInt(i, 10) }

// buildpackStep builds a repository that has no Dockerfile.
//
// The lifecycle runs inside the builder image rather than on its own, because
// the buildpacks that detect what the code is live in that image — a lifecycle
// with no buildpacks detects nothing.
//
// It exits immediately when the Dockerfile step already built. Every container
// in a pod runs, so "did the previous step handle this" is a question this one
// has to ask rather than something the pod can express.
//
// Like the Dockerfile step, it holds no credential and pushes nothing: the
// lifecycle exports to a local OCI layout on the shared volume with -layout,
// and the push step ships it afterwards. The lifecycle runs a repository's own
// buildpacks, so the credential must not be within its reach.
func buildpackStep(req orchestrator.BuildRequest) corev1.Container {

	return corev1.Container{
		Name:  buildpackContainer,
		Image: packImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "SUBDIR", Value: req.Subdir},
			{Name: "LAYOUT_DIR", Value: buildpackLayoutDir},
			// The platform API the lifecycle is asked to speak. Pinned rather
			// than left to default, because the builder image and the
			// lifecycle inside it move independently of this code.
			{Name: "CNB_PLATFORM_API", Value: "0.13"},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
if [ -f ` + dockerfileMarker + ` ]; then
  echo "Already built from the Dockerfile."
  exit 0
fi
echo "No Dockerfile — detecting what this is with buildpacks."
# The lifecycle writes layers, a cache and a platform directory, and refuses
# to guess where. They go on the shared volume rather than the image's own
# filesystem, which is read-only to a non-root user.
mkdir -p /workspace/layers /workspace/cache /workspace/platform/env "$LAYOUT_DIR"

# Tell git the checkout is trustworthy, through the platform directory.
#
# The clone runs as a different uid to this step, and git refuses to read a
# repository owned by another user — "dubious ownership", exit 128. Nothing
# here runs git directly; a buildpack does. Go stamps VCS information into a
# binary by default, shells out to git for it, and turns that refusal into
# "error obtaining VCS status" — failing the build after detection has passed
# and the toolchain is already installed.
#
# Written as files rather than set on the container, because that is the only
# environment a buildpack sees: the lifecycle builds the build environment from
# this directory and does not forward its own. Setting them as container
# variables looks right, changes nothing, and was tried first.
#
# Trusting the path rather than passing -buildvcs=false keeps the commit in the
# built binary, which is the reason the stamping exists.
printf '%s' '1'              > /workspace/platform/env/GIT_CONFIG_COUNT
printf '%s' 'safe.directory' > /workspace/platform/env/GIT_CONFIG_KEY_0
printf '%s' '*'              > /workspace/platform/env/GIT_CONFIG_VALUE_0

# -layout exports to a local OCI layout under -layout-dir instead of pushing to
# a registry, so this step needs no credential. The image reference becomes a
# layout path; the push step reads it back out of $LAYOUT_DIR.
/cnb/lifecycle/creator \
  -app="/workspace/src/$SUBDIR" \
  -layers=/workspace/layers \
  -cache-dir=/workspace/cache \
  -platform=/workspace/platform \
  -layout -layout-dir="$LAYOUT_DIR" \
  -uid=` + itoa(cnbUID) + ` -gid=` + itoa(cnbGID) + ` \
  -log-level=info \
  "$IMAGE"
echo "Built $IMAGE"
`},
		SecurityContext: hardenedAs(cnbUID, cnbGID),
		// No registry mount: the lifecycle runs repository buildpacks.
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
		},
	}
}

// pushStep ships the built image to the registry.
//
// This is the only step that holds the push credential, and the only one that
// runs no repository code — that pairing is the point. The clone, Dockerfile
// and buildpack steps each run something the repository supplied, so a
// credential within their reach is a credential a repository can take; this
// step runs a fixed `skopeo copy` of an artifact those steps already produced,
// so there is nothing repository-controlled executing beside the secret.
//
// It pushes whichever artifact exists: the OCI archive from the Dockerfile
// step, or the OCI layout from the buildpack step. The credential is read from
// /registry, exactly where the build steps used to read it — the move is which
// container that mount appears in, not where in the filesystem it lands.
func pushStep(req orchestrator.BuildRequest) corev1.Container {
	tlsFlag := ""
	if req.Insecure {
		tlsFlag = " --dest-tls-verify=false"
	}

	return corev1.Container{
		Name:  pushContainer,
		Image: pushImage,
		Env: []corev1.EnvVar{
			{Name: "IMAGE", Value: req.Image},
			{Name: "ARTIFACT", Value: dockerfileArtifact},
			{Name: "LAYOUT_DIR", Value: buildpackLayoutDir},
			{Name: "MARKER", Value: dockerfileMarker},
			// skopeo reads its credential from this file.
			{Name: "REGISTRY_AUTH_FILE", Value: "/registry/config.json"},
		},
		Command: []string{"/bin/sh", "-euc"},
		Args: []string{`
if [ -f "$MARKER" ]; then
  echo "Pushing $IMAGE from the Dockerfile build"
  SRC="oci-archive:$ARTIFACT"
else
  echo "Pushing $IMAGE from the buildpack build"
  # The lifecycle wrote an OCI layout under $LAYOUT_DIR; skopeo reads the whole
  # layout and pushes the single image it holds.
  SRC="oci:$LAYOUT_DIR"
fi
skopeo copy` + tlsFlag + ` "$SRC" "docker://$IMAGE"
echo "Pushed $IMAGE"

# Best effort: report the uid the pushed image runs as, so the deploy can honour
# runAsNonRoot. A numeric user is usable as-is; a named one is left for the
# image to resolve at runtime, exactly as before this was discovered here.
USER_FIELD="$(skopeo inspect --config` + tlsFlag + ` "docker://$IMAGE" 2>/dev/null \
  | sed -n 's/.*"User"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
case "$USER_FIELD" in
  ''|*[!0-9]*) : ;;                       # empty or not purely numeric: leave it
  *) echo "` + uidMarker + `$USER_FIELD" ;;
esac
`},
		SecurityContext: hardenedAs(buildUID, cnbGID),
		VolumeMounts: []corev1.VolumeMount{
			{Name: buildVolume, MountPath: "/workspace"},
			{Name: "registry", MountPath: "/registry", ReadOnly: true},
		},
	}
}

// buildDeadlineSlack is how far past buildTimeout the Job's own deadline sits,
// so the process's cap — which can say why — is the one that normally fires.
const buildDeadlineSlack = 5 * time.Minute

// bounded gives a build step a memory ceiling.
//
// The build namespace has no LimitRange, so a step ran with whatever the node
// had: a compiler with a leak, or three pushes building at once, took memory
// from the apps and from the control plane's own Postgres, which share the
// machine. The ceiling is high because builds are legitimately hungry, and it
// is on memory alone — CPU is shared out by request when it is short, and a
// CPU limit would only make every build slower on an idle node.
func bounded(c corev1.Container) corev1.Container {
	c.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	return c
}
