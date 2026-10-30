// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/identity"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/observation"
	"github.com/qifalab/euler-platform/services/witshield-engine/internal/scanner"
)

// controllerEventContract mirrors the per-batch and per-event checks of the
// Controller's POST /agent/v1/events handler (internal/httpapi/actions_agent.go,
// agentEvents). A batch that fails any of them is answered with HTTP 400, and
// the same signed body fails again on every retry.
func controllerEventContract(t *testing.T, publicKey string, accepted *atomic.Int32) http.HandlerFunc {
	identifier := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}$`)
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent/v1/reports" {
			w.WriteHeader(http.StatusCreated)
			return
		}
		if r.URL.Path != "/agent/v1/events" {
			http.NotFound(w, r)
			return
		}
		var in struct {
			Events            []domain.SecurityEvent `json:"events"`
			IdentitySignature string                 `json:"identitySignature"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&in); err != nil || len(in.Events) == 0 || len(in.Events) > 500 || len(in.IdentitySignature) > 256 {
			http.Error(w, `{"error":{"code":"invalid_events"}}`, http.StatusBadRequest)
			return
		}
		for _, event := range in.Events {
			if !identifier.MatchString(event.ID) || event.OccurredAt.IsZero() || len(event.Payload) > 4*1024 || len(event.SourceIP) > 64 {
				t.Logf("controller rejected event %s: payload=%d bytes sourceIp=%d bytes", event.ID, len(event.Payload), len(event.SourceIP))
				http.Error(w, `{"error":{"code":"invalid_event"}}`, http.StatusBadRequest)
				return
			}
		}
		if err := identity.VerifySecurityEventBatch(publicKey, in.IdentitySignature, identity.SecurityEventBatchProof{DeviceID: "dev_test", Events: in.Events}); err != nil {
			http.Error(w, `{"error":{"code":"invalid_device_proof"}}`, http.StatusUnauthorized)
			return
		}
		accepted.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}
}

func writeFakeProcess(t *testing.T, root string, pid, uid int, executable string) {
	t.Helper()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	id := strconv.Itoa(uid)
	status := "Name:\tworker\nPPid:\t1\nUid:\t" + id + "\t" + id + "\t" + id + "\t" + id + "\n"
	stat := strconv.Itoa(pid) + " (worker) S 1 " + strings.Repeat("0 ", 17) + "7777\n"
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

func TestOutOfContractSecurityEventsCannotPoisonDurableQueue(t *testing.T) {
	publicKey, privateKey, err := NewIdentity()
	if err != nil {
		t.Fatal(err)
	}
	var accepted atomic.Int32
	server := httptest.NewServer(controllerEventContract(t, publicKey, &accepted))
	defer server.Close()
	queue, err := NewQueue(filepath.Join(t.TempDir(), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	client := authenticatedTestClient(t, server.URL, "device-token")
	runner := &Runner{state: State{DeviceID: "dev_test", IdentityPublicKey: publicKey, IdentityPrivateKey: privateKey}, client: client, queue: queue}

	// Any local user can run and then delete a binary whose path is legal but
	// long and full of '&': JSON escapes every '&' as \u0026, so the procfs
	// event payload grows past the Controller's 4 KiB per-event limit.
	root := t.TempDir()
	writeFakeProcess(t, root, 4242, 1000, "/tmp/"+strings.Repeat("&", 900)+" (deleted)")
	processes := processWatcher{hostRoot: root, statePath: filepath.Join(t.TempDir(), "process.json")}
	processEvents, err := processes.Poll(context.Background())
	if err != nil || len(processEvents) != 1 {
		t.Fatalf("process events=%d err=%v", len(processEvents), err)
	}

	// Any local user can also write an authpriv syslog line (logger -p
	// authpriv.info ...). The auth.log fallback captures an unbounded run of
	// hex digits, dots and colons as the "source IP".
	logPath := filepath.Join(t.TempDir(), "auth.log")
	if err = os.WriteFile(logPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	authLog := &authLogWatcher{path: logPath, statePath: filepath.Join(t.TempDir(), "auth.offset")}
	if _, checkpoint, pollErr := authLog.Poll(context.Background()); pollErr != nil {
		t.Fatal(pollErr)
	} else if err = authLog.Commit(checkpoint); err != nil {
		t.Fatal(err)
	}
	line := "Oct 30 10:00:00 host app: Failed password for root from " + strings.Repeat("1", 80) + " port 22 ssh2\n"
	if err = os.WriteFile(logPath, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	authEvents, _, err := authLog.Poll(context.Background())
	if err != nil || len(authEvents) != 1 {
		t.Fatalf("auth events=%d err=%v", len(authEvents), err)
	}

	if err = runner.queueSecurityEvents(processEvents); err != nil {
		t.Fatal(err)
	}
	if err = runner.queueSecurityEvents(authEvents); err != nil {
		t.Fatal(err)
	}
	if err = queue.Enqueue("report", "", map[string]any{"id": "rpt_after_events"}); err != nil {
		t.Fatal(err)
	}
	if err = queue.Flush(context.Background(), client); err != nil {
		t.Fatalf("an agent-generated event batch was rejected and blocks the durable FIFO: %v", err)
	}
	files, _, err := queue.files()
	if err != nil || len(files) != 0 || accepted.Load() != 2 {
		t.Fatalf("queue not drained: files=%v accepted=%d err=%v", files, accepted.Load(), err)
	}
}

func TestRedeliveredCommandWithQueuedResultIsNotExecutedAgain(t *testing.T) {
	// Uploads are refused (for example the per-device work budget during an
	// event storm), so the first result stays in the FIFO past the Controller's
	// 2-minute claim lease and the same command is offered again.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"agent_work_rate_limited","message":"device work budget exceeded"}}`)
	}))
	defer server.Close()
	queue, err := NewQueue(filepath.Join(t.TempDir(), "queue"))
	if err != nil {
		t.Fatal(err)
	}
	var scans atomic.Int32
	scan := scanner.New(scanner.CheckFunc{NameValue: "count", Fn: func(context.Context, scanner.Host) ([]domain.Finding, error) {
		scans.Add(1)
		return nil, nil
	}})
	runner := &Runner{state: State{DeviceID: "dev_test"}, client: authenticatedTestClient(t, server.URL, "device-token"), queue: queue, scanner: scan, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	command := domain.DeviceCommand{ID: "cmd_scan_redelivered", Type: domain.CommandScan, CreatedAt: time.Now()}
	_ = runner.handleCommand(context.Background(), command)
	_ = runner.handleCommand(context.Background(), command)
	results := 0
	files, _, err := queue.files()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range files {
		data, readErr := os.ReadFile(filepath.Join(queue.dir, name))
		if readErr != nil {
			t.Fatal(readErr)
		}
		var item queuedRequest
		if json.Unmarshal(data, &item) == nil && item.Kind == "command_result" && item.CommandID == command.ID {
			results++
		}
	}
	// Two scans produce two different report IDs; the Controller accepts the
	// first completion and answers the second with 409, which the FIFO retries
	// forever ahead of every newer report, event and action result.
	if scans.Load() != 1 || results != 1 {
		t.Fatalf("redelivered command ran %d scans and queued %d results; want 1 and 1", scans.Load(), results)
	}
}

func TestHelperProcessObservationSurvivesLongMultibyteAndInvalidPaths(t *testing.T) {
	root := t.TempDir()
	// Both paths are legal Linux names. The first is a long UTF-8 (Chinese)
	// path, the second contains bytes that are not UTF-8 at all.
	writeFakeProcess(t, root, 501, 1000, "/tmp/"+strings.Repeat("构建", 200)+" (deleted)")
	writeFakeProcess(t, root, 502, 1000, "/tmp/"+strings.Repeat("\xff", 400)+" (deleted)")
	snapshot, err := observation.SuspiciousProcesses(context.Background(), root)
	if err != nil || len(snapshot.Processes) != 2 {
		t.Fatalf("snapshot=%d err=%v", len(snapshot.Processes), err)
	}
	dir, err := os.MkdirTemp("/tmp", "witshield-helper-utf8-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "helper.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer connection.Close()
		if _, readErr := bufio.NewReader(connection).ReadBytes('\n'); readErr != nil {
			return
		}
		// Same encoding as cmd/witshield-helper writeResponse.
		encoded, _ := json.Marshal(map[string]any{"ok": true, "processes": snapshot.Processes, "processObserved": snapshot.Observed})
		_, _ = connection.Write(append(encoded, '\n'))
	}()
	client := HelperClient{Socket: socket, Token: strings.Repeat("a", 64)}
	got, err := client.ObserveSuspiciousProcesses(context.Background())
	if err != nil || len(got.Processes) != 2 {
		t.Fatalf("one long path blinded the whole process sensor: processes=%d err=%v", len(got.Processes), err)
	}
}
