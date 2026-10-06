package app

import (
	"context"
	"testing"
)

// A restore must not die with the request that started it. Run under the
// request's own context, closing the browser tab cancelled the wait and the
// cleanup deleted the Job halfway through replacing the data.
func TestDetachTaskSurvivesItsCaller(t *testing.T) {
	type key struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), key{}, "kept"))

	task := detachTask(parent)
	cancel()

	if parent.Err() == nil {
		t.Fatal("the parent was not cancelled, so this proves nothing")
	}
	if err := task.Err(); err != nil {
		t.Fatalf("the task context was cancelled with its caller: %v", err)
	}
	select {
	case <-task.Done():
		t.Fatal("the task context is done although nothing cancelled it")
	default:
	}
	if task.Value(key{}) != "kept" {
		t.Error("the task context lost the caller's values")
	}
}
