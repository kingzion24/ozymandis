package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"iter"
)

// ErrUnavailable is returned by every call on an install with no cluster
// connection at all.
var ErrUnavailable = errors.New("orchestrator: no cluster is connected")

// Unavailable is the Orchestrator of an install that could not build a cluster
// client — no kubeconfig, or one that does not parse.
//
// It refuses everything. The alternative that used to stand here was Noop,
// which accepts everything: a deploy was recorded as live having reached no
// cluster, a delete removed the row and left the workload running, and the
// health check answered ok throughout. A control plane that cannot reach its
// cluster has to say so on every path, because the one path that stays quiet is
// the one somebody will trust.
type Unavailable struct {
	cause error
}

// Compile-time check that Unavailable satisfies the interface.
var _ Orchestrator = (*Unavailable)(nil)

// NewUnavailable returns an orchestrator that fails every call with cause.
func NewUnavailable(cause error) *Unavailable {
	return &Unavailable{cause: cause}
}

func (u *Unavailable) err() error {
	if u.cause == nil {
		return ErrUnavailable
	}
	return fmt.Errorf("%w: %w", ErrUnavailable, u.cause)
}

func (u *Unavailable) EnsureNamespace(context.Context, NamespaceSpec) error { return u.err() }
func (u *Unavailable) DeleteNamespace(context.Context, string) error        { return u.err() }
func (u *Unavailable) ApplyApp(context.Context, AppSpec) error              { return u.err() }
func (u *Unavailable) DeleteApp(context.Context, Ref) error                 { return u.err() }
func (u *Unavailable) Ping(context.Context) error                           { return u.err() }

func (u *Unavailable) AppStatus(context.Context, Ref) (AppStatus, error) {
	return AppStatus{}, u.err()
}

func (u *Unavailable) ClusterSummary(context.Context) (ClusterSummary, error) {
	return ClusterSummary{}, u.err()
}

func (u *Unavailable) Nodes(context.Context) ([]NodeInfo, error)        { return nil, u.err() }
func (u *Unavailable) Events(context.Context, int) ([]EventInfo, error) { return nil, u.err() }
func (u *Unavailable) Logs(context.Context, LogOptions) ([]LogLine, error) {
	return nil, u.err()
}

func (u *Unavailable) LogStream(
	context.Context, LogOptions,
) (iter.Seq2[LogLine, error], error) {
	return nil, u.err()
}

func (u *Unavailable) Volumes(context.Context, OwnerID) ([]VolumeInfo, error) {
	return nil, u.err()
}

func (u *Unavailable) Pods(context.Context, PodListOptions) ([]PodInfo, error) {
	return nil, u.err()
}
