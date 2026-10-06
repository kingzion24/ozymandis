package orchestrator

import (
	"context"
	"errors"
	"testing"
)

// The point of Unavailable is that nothing on it succeeds. A write that
// returned nil would be recorded upstream as a deploy that went out.
func TestUnavailableRefusesEverything(t *testing.T) {
	cause := errors.New("no kubeconfig")
	u := NewUnavailable(cause)
	ctx := context.Background()

	_, statusErr := u.AppStatus(ctx, Ref{})
	_, podsErr := u.Pods(ctx, PodListOptions{})
	_, streamErr := u.LogStream(ctx, LogOptions{})

	for name, err := range map[string]error{
		"EnsureNamespace": u.EnsureNamespace(ctx, NamespaceSpec{}),
		"DeleteNamespace": u.DeleteNamespace(ctx, "ns"),
		"ApplyApp":        u.ApplyApp(ctx, AppSpec{}),
		"DeleteApp":       u.DeleteApp(ctx, Ref{}),
		"Ping":            u.Ping(ctx),
		"AppStatus":       statusErr,
		"Pods":            podsErr,
		"LogStream":       streamErr,
	} {
		if !errors.Is(err, ErrUnavailable) {
			t.Errorf("%s: err = %v, want ErrUnavailable", name, err)
		}
		if !errors.Is(err, cause) {
			t.Errorf("%s: err = %v, does not carry the reason", name, err)
		}
	}
}
