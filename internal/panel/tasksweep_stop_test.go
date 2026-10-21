package panel

import (
	"context"
	"testing"
	"time"
)

// StopContext must not block forever when an in-flight upstream task request
// cannot be interrupted (task APIs have no context parameter). Production main
// relies on the timeout branch and then exits the process.
func TestTaskSweeperStopContextBounded(t *testing.T) {
	s := &TaskSweeper{done: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if s.StopContext(ctx) {
		t.Fatal("StopContext reported exit while done is still open")
	}

	close(s.done)
	if !s.StopContext(context.Background()) {
		t.Fatal("StopContext should report true after the loop has exited")
	}
}
