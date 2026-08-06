package k8s

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/exec"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// Exec opens an interactive session inside a running container.
//
// The only operation here that does not go through the typed clientset: exec is
// a bidirectional stream, not a request and a response, so it is built from the
// same REST config the clientset was and upgraded to SPDY.
//
// Which pod is NOT decided here. The spec names it, resolved by the owner-scoped
// layer, because a pod name is enough to reach any tenant's container and this
// package is handed a namespace without knowing whose it is. See ExecSpec.Pod.
func (o *Orchestrator) Exec(ctx context.Context, spec orchestrator.ExecSpec) error {
	if err := spec.Ref.Validate(); err != nil {
		return err
	}
	if spec.Pod == "" {
		return errors.New("k8s: exec needs a pod, and choosing one is not this layer's decision")
	}
	if len(spec.Command) == 0 {
		return errors.New("k8s: exec needs a command")
	}
	if o.restCfg == nil {
		// An orchestrator built against a fake clientset has no API server to
		// dial. Said plainly rather than panicking on a nil dereference.
		return errors.New("k8s: this orchestrator was built without a cluster connection")
	}

	req := o.client.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(spec.Pod).
		Namespace(spec.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Command: spec.Command,
			Stdin:   spec.Stdin != nil,
			Stdout:  spec.Stdout != nil,
			// A TTY merges stderr into stdout — a terminal has one stream — so
			// asking for both is asking for something the protocol cannot give.
			Stderr: spec.Stderr != nil && !spec.TTY,
			TTY:    spec.TTY,
		}, scheme.ParameterCodec)

	// SPDY rather than the newer WebSocket executor: SPDY is what every
	// supported Kubernetes version speaks for this subresource, and the
	// WebSocket path is still gated behind a feature flag on older clusters.
	// This engine targets K3s on whatever a self-hoster already has.
	executor, err := remotecommand.NewSPDYExecutor(o.restCfg, "POST", req.URL())
	if err != nil {
		return fmt.Errorf("k8s: open a session in %s: %w", spec.Pod, err)
	}

	streamErr := executor.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:             spec.Stdin,
		Stdout:            spec.Stdout,
		Stderr:            spec.Stderr,
		Tty:               spec.TTY,
		TerminalSizeQueue: sizeQueue(spec.Resize),
	})

	// A command that ran and exited non-zero is not a failure of the session.
	// Reported as a typed error so a caller can pass the code on — which is
	// what makes `oz exec` usable in a script — rather than as an opaque
	// string every caller would have to parse.
	var codeErr exec.CodeExitError
	if errors.As(streamErr, &codeErr) {
		return &orchestrator.ExitError{Code: codeErr.Code}
	}
	if streamErr != nil {
		return fmt.Errorf("k8s: session in %s: %w", spec.Pod, streamErr)
	}
	return nil
}

// sizeQueue adapts a channel of sizes to what remotecommand wants.
//
// Nil in, nil out: a session with no resize channel must pass a nil queue
// rather than one that blocks forever, because remotecommand calls Next in a
// loop and a queue that never returns holds a goroutine for the life of the
// session.
func sizeQueue(ch <-chan orchestrator.TerminalSize) remotecommand.TerminalSizeQueue {
	if ch == nil {
		return nil
	}
	return &resizeQueue{ch: ch}
}

type resizeQueue struct {
	ch <-chan orchestrator.TerminalSize
}

// Next blocks for the next size, and returns nil when the channel closes.
//
// Returning nil is how remotecommand is told to stop asking. Blocking forever
// instead would leak the goroutine it calls this from.
func (q *resizeQueue) Next() *remotecommand.TerminalSize {
	size, ok := <-q.ch
	if !ok {
		return nil
	}
	return &remotecommand.TerminalSize{Width: size.Cols, Height: size.Rows}
}

// Compile-time check that this satisfies the optional seam.
var _ orchestrator.Executor = (*Orchestrator)(nil)
