// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package action

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func setFakeProcessExecutable(f *fakeProcessController, executable string) {
	f.mu.Lock()
	f.identity.Executable = executable
	f.mu.Unlock()
}

func suspendForTest(t *testing.T, actionID, executable string) (*TemporaryProcessSuspendPlaybook, *fakeProcessController, ApplyResult) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	controller := &fakeProcessController{identity: ProcessRuntimeIdentity{PID: 93, PPID: 1, UID: 0, StartTime: 994, Executable: executable}}
	playbook, err := NewTemporaryProcessSuspendPlaybook(controller, dir)
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(TemporaryProcessSuspendParams{PID: 93, StartTime: 994, Executable: "/tmp/dropper", TTLSeconds: 30})
	apply, err := playbook.Apply(context.Background(), Invocation{ActionID: actionID, Parameters: params})
	if err != nil || !controller.isStopped() {
		t.Fatalf("apply err=%v stopped=%v", err, controller.isStopped())
	}
	// Drive the timer path explicitly instead of waiting for the 30s TTL.
	playbook.cancelTimer(actionID)
	return playbook, controller, apply
}

// /proc/<pid>/exe gains a " (deleted)" suffix when the binary is unlinked,
// which is a normal response step while the process is stopped. The PID and
// kernel start time still identify the same process, so the TTL resume and an
// administrator's early resume must still send SIGCONT.
func TestTemporaryProcessSuspendResumesAfterExecutableIsDeleted(t *testing.T) {
	t.Run("timed resume", func(t *testing.T) {
		playbook, controller, apply := suspendForTest(t, "act_deleted_timer", "/tmp/dropper")
		setFakeProcessExecutable(controller, "/tmp/dropper (deleted)")
		if _, err := playbook.Verify(context.Background(), Invocation{State: apply.State}); err != nil {
			t.Fatalf("verify treated the unlinked binary as another process: %v", err)
		}
		playbook.expire("act_deleted_timer")
		playbook.cancelTimer("act_deleted_timer")
		if controller.isStopped() {
			t.Fatal("process stayed stopped after its TTL because the deleted executable looked like PID reuse")
		}
		if _, err := os.Stat(playbook.journalPath("act_deleted_timer")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("resume journal was not removed: %v", err)
		}
	})
	t.Run("early rollback", func(t *testing.T) {
		playbook, controller, apply := suspendForTest(t, "act_deleted_rollback", "/tmp/dropper")
		setFakeProcessExecutable(controller, "/tmp/dropper (deleted)")
		result, err := playbook.Rollback(context.Background(), Invocation{ActionID: "act_deleted_rollback", State: apply.State})
		if err != nil || controller.isStopped() {
			t.Fatalf("rollback reported %q err=%v but stopped=%v", result.Summary, err, controller.isStopped())
		}
	})
	t.Run("binary deleted before suspension", func(t *testing.T) {
		playbook, controller, apply := suspendForTest(t, "act_deleted_before", "/tmp/dropper (deleted)")
		if _, err := playbook.Verify(context.Background(), Invocation{State: apply.State}); err != nil {
			t.Fatal(err)
		}
		playbook.expire("act_deleted_before")
		playbook.cancelTimer("act_deleted_before")
		if controller.isStopped() {
			t.Fatal("process suspended with an already-unlinked binary was not resumed")
		}
	})
	t.Run("different executable is still not signaled", func(t *testing.T) {
		playbook, controller, apply := suspendForTest(t, "act_other_exe", "/tmp/dropper")
		setFakeProcessExecutable(controller, "/tmp/dropper-other")
		result, err := playbook.Rollback(context.Background(), Invocation{ActionID: "act_other_exe", State: apply.State})
		if err != nil || !controller.isStopped() || result.Summary != "PID was reused; no signal sent to the replacement process" {
			t.Fatalf("replacement process was signaled: summary=%q err=%v stopped=%v", result.Summary, err, controller.isStopped())
		}
	})
}
