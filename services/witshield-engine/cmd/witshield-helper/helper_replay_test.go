// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/action"
)

type countingPlaybook struct{ applies *atomic.Int32 }

func (countingPlaybook) Type() action.Type              { return "helper_count" }
func (countingPlaybook) Validate(json.RawMessage) error { return nil }
func (countingPlaybook) Precheck(context.Context, action.Invocation) (action.Result, error) {
	return action.Result{Summary: "checked"}, nil
}
func (countingPlaybook) Preview(context.Context, action.Invocation) (action.Result, error) {
	return action.Result{Summary: "previewed"}, nil
}
func (p countingPlaybook) Apply(context.Context, action.Invocation) (action.ApplyResult, error) {
	p.applies.Add(1)
	return action.ApplyResult{Result: action.Result{Summary: "applied"}, State: json.RawMessage(`{"undo":true}`)}, nil
}
func (countingPlaybook) Verify(context.Context, action.Invocation) (action.Result, error) {
	return action.Result{Summary: "verified"}, nil
}
func (countingPlaybook) Rollback(context.Context, action.Invocation) (action.Result, error) {
	return action.Result{Summary: "rolled back"}, nil
}

// startReplayHelper serves a real Unix socket with a durable receipt cache.
func startReplayHelper(t *testing.T) (*receiptCache, *atomic.Int32, func(helperRequest) helperResponse) {
	t.Helper()
	applies := &atomic.Int32{}
	engine, err := action.NewEngine(countingPlaybook{applies: applies})
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("/tmp", "witshield-helper-replay-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err = os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	cache, err := newReceiptCache(filepath.Join(root, "receipts"))
	if err != nil {
		t.Fatal(err)
	}
	socketPath := filepath.Join(root, "helper.sock")
	listener, err := listenUnix(socketPath, -1)
	if err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("a", 64)
	helper := &server{engine: engine, receipts: cache, token: []byte(token)}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- helper.serve(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		_ = listener.Close()
		<-served
	})
	call := func(request helperRequest) helperResponse {
		t.Helper()
		request.Token = token
		client, dialErr := net.DialUnix("unix", nil, &net.UnixAddr{Name: socketPath, Net: "unix"})
		if dialErr != nil {
			t.Fatal(dialErr)
		}
		defer client.Close()
		encoded, _ := json.Marshal(request)
		if _, writeErr := client.Write(append(encoded, '\n')); writeErr != nil {
			t.Fatal(writeErr)
		}
		line, readErr := bufio.NewReader(client).ReadBytes('\n')
		if readErr != nil {
			t.Fatal(readErr)
		}
		var response helperResponse
		if decodeErr := json.Unmarshal(line, &response); decodeErr != nil {
			t.Fatalf("%v: %s", decodeErr, line)
		}
		return response
	}
	return cache, applies, call
}

// The Agent re-sends a command under the same attempt ID when it lost the
// response. The Helper must answer from its durable receipt and never run the
// privileged change a second time.
func TestHelperReplaysDurableReceiptInsteadOfRunningTwice(t *testing.T) {
	cache, applies, call := startReplayHelper(t)
	request := helperRequest{AttemptID: "cmd-1", ActionID: "act-1", Type: "helper_count", Operation: action.OperationExecute, Parameters: json.RawMessage(`{"n":1}`)}
	first := call(request)
	if !first.OK || first.AuditReceipt == nil || applies.Load() != 1 {
		t.Fatalf("first execution: ok=%v receipt=%v applies=%d error=%q", first.OK, first.AuditReceipt != nil, applies.Load(), first.Error)
	}
	replay := call(request)
	if !replay.OK || replay.AuditReceipt == nil || replay.AuditReceipt.ID != first.AuditReceipt.ID || applies.Load() != 1 {
		t.Fatalf("replay re-ran or changed the receipt: ok=%v applies=%d", replay.OK, applies.Load())
	}
	conflicting := request
	conflicting.Parameters = json.RawMessage(`{"n":2}`)
	if response := call(conflicting); response.OK || applies.Load() != 1 {
		t.Fatalf("same attempt with other parameters ran: ok=%v applies=%d", response.OK, applies.Load())
	}
	// A start marker without a final receipt means a previous Helper process may
	// have crossed into the playbook: report the unknown outcome, never re-run.
	if err := cache.begin("cmd-crashed", "act-2", "helper_count", action.OperationExecute, json.RawMessage(`{"n":3}`), nil); err != nil {
		t.Fatal(err)
	}
	crashed := call(helperRequest{AttemptID: "cmd-crashed", ActionID: "act-2", Type: "helper_count", Operation: action.OperationExecute, Parameters: json.RawMessage(`{"n":3}`)})
	if crashed.OK || crashed.Error != action.ExecutionIndeterminateMessage || applies.Load() != 1 {
		t.Fatalf("interrupted attempt was not reported indeterminate: ok=%v error=%q applies=%d", crashed.OK, crashed.Error, applies.Load())
	}
}

// Only execute, rollback and confirm get a durable start marker, replay and the
// action mutex, and they are the only operations the Agent sends. A bare apply
// would mutate the host outside that path; other phases are not part of the
// protocol either.
func TestHelperRejectsOperationsOutsideTheDurableReceiptPath(t *testing.T) {
	_, applies, call := startReplayHelper(t)
	for _, operation := range []action.Operation{action.OperationApply, action.OperationPrecheck, action.OperationPreview, action.OperationVerify, "bogus"} {
		response := call(helperRequest{AttemptID: "cmd-" + string(operation), ActionID: "act-1", Type: "helper_count", Operation: operation, Parameters: json.RawMessage(`{}`)})
		if response.OK || response.AuditReceipt != nil {
			t.Errorf("operation %q was executed: ok=%v", operation, response.OK)
		}
	}
	if applies.Load() != 0 {
		t.Fatalf("a non-durable operation changed the host %d times", applies.Load())
	}
}
