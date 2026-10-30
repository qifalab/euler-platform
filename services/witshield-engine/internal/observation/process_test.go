// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package observation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func writeProcess(t *testing.T, root string, pid int, executable string) {
	t.Helper()
	dir := filepath.Join(root, "proc", strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "status"), []byte("Name:\tworker\nPPid:\t1\nUid:\t1000\t1000\t1000\t1000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(strconv.Itoa(pid)+" (worker) S 1 "+strings.Repeat("0 ", 17)+"99\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(executable, filepath.Join(dir, "exe")); err != nil {
		t.Fatal(err)
	}
}

func TestProcessFieldsAreValidUTF8WithinTheirBoundsAfterJSON(t *testing.T) {
	root := t.TempDir()
	writeProcess(t, root, 31, "/tmp/"+strings.Repeat("构建", 200)+" (deleted)")
	writeProcess(t, root, 32, "/tmp/"+strings.Repeat("\xff", 400)+" (deleted)")
	writeProcess(t, root, 33, "/tmp/short (deleted)")
	snapshot, err := SuspiciousProcesses(context.Background(), root)
	if err != nil || len(snapshot.Processes) != 3 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	// The Helper sends the snapshot with encoding/json, which rewrites every
	// byte of a split or invalid UTF-8 sequence as U+FFFD (3 bytes). The Agent
	// rejects the whole observation when a decoded field exceeds its bound
	// (internal/agent/helper.go: executable 1024+len("…"), name 128+len("…")).
	encoded, err := json.Marshal(snapshot.Processes)
	if err != nil {
		t.Fatal(err)
	}
	var decoded []Process
	if err = json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	for i, process := range decoded {
		if !utf8.ValidString(snapshot.Processes[i].Executable) || len(process.Executable) > 1024+len("…") || len(process.Name) > 128+len("…") {
			t.Errorf("pid %d: executable %d bytes (valid UTF-8 before JSON: %v)", process.PID, len(process.Executable), utf8.ValidString(snapshot.Processes[i].Executable))
		}
	}
	for _, process := range decoded {
		if process.PID == 33 && process.Executable != "/tmp/short (deleted)" {
			t.Fatalf("short executable changed: %q", process.Executable)
		}
	}
}

func TestTruncateFieldCutsOnRuneBoundary(t *testing.T) {
	if got := truncateField("ab构建", 4); got != "ab…" {
		t.Fatalf("got %q", got)
	}
	if got := truncateField("abc", 3); got != "abc" {
		t.Fatalf("got %q", got)
	}
	if got := truncateField("a\x01b\x7fc", 10); got != "abc" {
		t.Fatalf("control characters kept: %q", got)
	}
}
