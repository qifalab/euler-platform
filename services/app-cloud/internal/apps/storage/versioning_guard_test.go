package storage

import (
	"errors"
	"testing"
)

func TestFailedUploadSignatureDoesNotBlockVersioning(t *testing.T) {
	m, f, _ := testModule(t)
	m.plane = multipartFake(f)
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID
	f.signErr = errors.New("signer unavailable")
	if w := request(t, m, s, "POST", base+"/objects/presigned-upload", map[string]any{"key": "direct.txt", "size": 1}); w.Code != 503 {
		t.Fatalf("signing failure returned %d", w.Code)
	}
	// No permit reached the client, so there is nothing the guard must wait for.
	decode[map[string]string](t, request(t, m, s, "PUT", base+"/versioning", map[string]string{"status": "Enabled"}), 200)
}
