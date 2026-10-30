// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package controllercmd

import (
	"context"
	"net"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// MainVersion closes the store in a deferred call. Its background workers must
// be stopped and finished first on every return path; otherwise they keep
// running against a closed database (and leak) when a listener fails to bind.
func TestMainVersionStopsWorkersBeforeClosingTheStore(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	dir := t.TempDir()
	err = MainVersion(context.Background(), []string{
		"-data-dir", filepath.Join(dir, "data"),
		"-listen", busy.Addr().String(),
		"-web-dir", filepath.Join(dir, "web"),
	}, "test")
	if err == nil {
		t.Fatal("controller started on an address that is already in use")
	}
	stacks := make([]byte, 1<<20)
	dump := string(stacks[:runtime.Stack(stacks, true)])
	for _, worker := range []string{
		"controllercmd.runMaintenance",
		"scheduler.(*Scheduler).Run",
		"httpapi.(*Server).RunNotificationWorker",
		"httpapi.(*Server).RunSecurityEngineerWorker",
	} {
		if strings.Contains(dump, worker) {
			t.Errorf("%s still runs after MainVersion returned and closed the store", worker)
		}
	}
}
