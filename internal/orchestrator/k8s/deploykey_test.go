package k8s

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

func buildReq() orchestrator.BuildRequest {
	return orchestrator.BuildRequest{
		Owner: "team-a", App: "api", Image: "registry.test/api:d1234",
		RepoURL: "git@github.com:you/private.git", Ref: "main",
	}
}

// The containment property, and the reason the key is mounted per step.
//
// The clone step runs git and nothing else. The BuildKit and buildpack steps
// run whatever the REPOSITORY tells them to — a Dockerfile, a build script —
// so a deploy key readable there is a key any repository can copy into a layer
// and publish with the image. Mounting it into the whole pod would be the
// obvious spelling and would hand every built image a working credential to the
// source it was built from.
func TestTheDeployKeyReachesTheCloneStepAndNothingElse(t *testing.T) {
	job := buildJob("build-x", buildReq(), "registry-secret", "build-x-ssh")

	var sawClone bool
	for _, c := range job.Spec.Template.Spec.InitContainers {
		mounted := hasMount(c.VolumeMounts, sshVolume)
		switch c.Name {
		case cloneContainer:
			sawClone = true
			if !mounted {
				t.Error("the clone step cannot read the deploy key, so a private " +
					"repository cannot be cloned at all")
			}
		default:
			if mounted {
				t.Errorf("%s can read the deploy key — that step runs what the "+
					"repository says to, so the key could be copied into a layer "+
					"and published with the image", c.Name)
			}
		}
	}
	for _, c := range job.Spec.Template.Spec.Containers {
		if hasMount(c.VolumeMounts, sshVolume) {
			t.Errorf("%s can read the deploy key", c.Name)
		}
	}
	if !sawClone {
		t.Fatal("no clone step in the job")
	}
}

// A public repository gets no key and no volume — nothing extra to go wrong.
func TestAPublicBuildHasNoDeployKeyVolume(t *testing.T) {
	job := buildJob("build-x", buildReq(), "registry-secret", "")

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == sshVolume {
			t.Fatal("a build with no deploy key still mounted one")
		}
	}
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != cloneContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "GIT_SSH_COMMAND" {
				t.Error("a public build was given GIT_SSH_COMMAND, which points " +
					"at a key that is not there")
			}
		}
	}
}

// ssh refuses a private key that group or other can read, and the error it
// gives sends people down a long path. 0400 is what it wants.
func TestTheDeployKeyIsMountedUnreadableByAnyoneElse(t *testing.T) {
	job := buildJob("build-x", buildReq(), "registry-secret", "build-x-ssh")

	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name != sshVolume {
			continue
		}
		if v.Secret == nil || v.Secret.DefaultMode == nil {
			t.Fatal("the deploy key volume has no mode set — ssh will refuse it")
		}
		if *v.Secret.DefaultMode != 0o400 {
			t.Errorf("mode = %#o, want 0400 — ssh refuses a key others can read",
				*v.Secret.DefaultMode)
		}
		return
	}
	t.Fatal("no deploy key volume")
}

// The clone must use the mounted key and only that key, and must not accept a
// host whose key changed.
func TestTheCloneUsesTheMountedKey(t *testing.T) {
	job := buildJob("build-x", buildReq(), "registry-secret", "build-x-ssh")

	var cmd string
	for _, c := range job.Spec.Template.Spec.InitContainers {
		if c.Name != cloneContainer {
			continue
		}
		for _, e := range c.Env {
			if e.Name == "GIT_SSH_COMMAND" {
				cmd = e.Value
			}
		}
	}
	if cmd == "" {
		t.Fatal("the clone step has no GIT_SSH_COMMAND, so it would try the " +
			"agent or a key that is not there")
	}

	for _, want := range []string{"-i /ssh/id", "IdentitiesOnly=yes"} {
		if !strings.Contains(cmd, want) {
			t.Errorf("GIT_SSH_COMMAND is missing %q: %s", want, cmd)
		}
	}
	// accept-new, not "no". A host whose key CHANGES is the attack worth
	// catching; "no" accepts anything and is what most examples say.
	if !strings.Contains(cmd, "StrictHostKeyChecking=accept-new") {
		t.Errorf("host key checking is not accept-new: %s", cmd)
	}
	if strings.Contains(cmd, "StrictHostKeyChecking=no") {
		t.Errorf("host key checking is off entirely: %s", cmd)
	}
}

func hasMount(mounts []corev1.VolumeMount, name string) bool {
	for _, m := range mounts {
		if m.Name == name {
			return true
		}
	}
	return false
}
