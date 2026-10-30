// Euler derivative of WitShield (Apache-2.0); imports and integration may be modified. See module NOTICE.

package agent

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/qifalab/euler-platform/services/witshield-engine/internal/domain"
)

// The Controller stores heartbeat sensor health only when every item's error
// is at most 500 bytes after decoding (store.PutSensorHealth); otherwise the
// whole heartbeat is answered with HTTP 400 and the device looks offline.
func TestSensorErrorStaysWithinControllerBoundAfterJSON(t *testing.T) {
	for _, raw := range []string{
		strings.Repeat("a", 499) + "构建失败",
		"open /etc/cron.d/" + strings.Repeat("\xff", 300) + ": permission denied",
	} {
		encoded, err := json.Marshal(domain.SensorHealth{Error: safeSensorError(raw)})
		if err != nil {
			t.Fatal(err)
		}
		var decoded domain.SensorHealth
		if err = json.Unmarshal(encoded, &decoded); err != nil {
			t.Fatal(err)
		}
		if len(strings.TrimSpace(decoded.Error)) > 500 {
			t.Fatalf("sensor error is %d bytes after JSON; the Controller rejects the heartbeat", len(decoded.Error))
		}
	}
}
