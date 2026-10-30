// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package main

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/action"
)

// begin() writes its start marker with O_CREATE|O_EXCL and only then writes and
// fsyncs the content; the privileged Run starts after that fsync. A crash in
// between leaves an empty marker whose attempt never ran. That file must not keep
// the Helper from starting (newReceiptCache is fatal in main) or block every
// later action, and the interrupted attempt must be retryable.
func TestReceiptCacheRecoversFromTornStartMarker(t *testing.T) {
	dir := t.TempDir()
	cache, err := newReceiptCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	params := json.RawMessage(`{"packages":["openssl"]}`)
	if err = cache.begin("cmd-done", "act-done", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil); err != nil {
		t.Fatal(err)
	}
	if err = cache.save("cmd-done", "act-done", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil, helperResponse{OK: true}); err != nil {
		t.Fatal(err)
	}
	torn := cache.path("cmd-torn")
	if err = os.WriteFile(torn, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted, err := newReceiptCache(dir)
	if err != nil {
		t.Fatalf("a torn start marker kept the helper receipt cache from opening: %v", err)
	}
	if _, found, loadErr := restarted.load("cmd-torn", "act-torn", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil); loadErr != nil || found {
		t.Fatalf("torn marker for an attempt that never ran: found=%v err=%v", found, loadErr)
	}
	if err = restarted.begin("cmd-torn", "act-torn", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil); err != nil {
		t.Fatalf("the interrupted attempt could not enter the helper again: %v", err)
	}
	if err = restarted.begin("cmd-next", "act-next", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil); err != nil {
		t.Fatalf("later actions are blocked: %v", err)
	}
	done, found, err := restarted.load("cmd-done", "act-done", action.TypePackageSecurityUpgrade, action.OperationExecute, params, nil)
	if err != nil || !found || !done.OK {
		t.Fatalf("final receipt was lost during recovery: %#v found=%v err=%v", done, found, err)
	}
}

// Fail closed for anything that is not an empty marker: a corrupt or foreign
// receipt still requires an operator, because it could hide a completed action.
func TestReceiptCacheStillRejectsCorruptReceipts(t *testing.T) {
	dir := t.TempDir()
	cache, err := newReceiptCache(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(cache.path("cmd-corrupt"), []byte(`{"state":"final"`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err = newReceiptCache(dir); err == nil {
		t.Fatal("corrupt receipt was silently accepted")
	}
	if _, err = os.Stat(cache.path("cmd-corrupt")); err != nil {
		t.Fatalf("corrupt receipt evidence was removed: %v", err)
	}
}
