package app

import (
	"context"
	"errors"
	"testing"
)

// credImages is stubImages with a say in what the credential read does. The
// other methods belong to the build path and are never reached here.
type credImages struct {
	stubImages
	config []byte
	err    error
}

func (c credImages) DockerConfig(context.Context) ([]byte, error) {
	return c.config, c.err
}

// An app pulling a public image never reaches the registry, so a broken
// registry must not stop it deploying.
func TestPullAuthSkipsAnAppThatDoesNotUseTheRegistry(t *testing.T) {
	s := &Service{images: credImages{err: errors.New("registry is down")}}

	for _, src := range []Source{SourceImage, SourcePostgres, SourceRedis} {
		auth, err := s.pullAuth(context.Background(), App{Name: "db", Source: src})
		if err != nil {
			t.Errorf("source %q: pullAuth: %v", src, err)
		}
		if auth != nil {
			t.Errorf("source %q: got a credential for a public image", src)
		}
	}
}

func TestPullAuthReturnsTheCredentialForABuiltImage(t *testing.T) {
	want := []byte(`{"auths":{}}`)
	s := &Service{images: credImages{config: want}}

	auth, err := s.pullAuth(context.Background(), App{Name: "web", Source: SourceGit})
	if err != nil {
		t.Fatalf("pullAuth: %v", err)
	}
	if string(auth) != string(want) {
		t.Fatalf("auth = %q, want %q", auth, want)
	}
}

// The deploy has to fail here. This app's image is in the registry, so
// applying it without a credential does not avoid the failure — it defers it
// into a pod that cannot pull, reported minutes later as ImagePullBackOff with
// nothing pointing back at the registry that broke.
func TestPullAuthFailsTheDeployForABuiltImage(t *testing.T) {
	sentinel := errors.New("registry is down")
	s := &Service{images: credImages{err: sentinel}}

	auth, err := s.pullAuth(context.Background(), App{Name: "web", Source: SourceGit})
	if err == nil {
		t.Fatal("a built image deployed without the credential it needs to pull")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("err = %v, want it to wrap the registry failure", err)
	}
	if auth != nil {
		t.Errorf("auth = %q, want none alongside an error", auth)
	}
}

// An install with no registry configured has nothing to ask.
func TestPullAuthIsSilentWithoutARegistry(t *testing.T) {
	s := &Service{}

	auth, err := s.pullAuth(context.Background(), App{Name: "web", Source: SourceGit})
	if err != nil {
		t.Fatalf("pullAuth: %v", err)
	}
	if auth != nil {
		t.Fatalf("auth = %q, want none", auth)
	}
}
