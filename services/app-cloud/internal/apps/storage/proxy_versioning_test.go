package storage

import (
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
)

// failingStaging rejects the proxy's staging write, as an unreachable S3 would.
type failingStaging struct{ *fakeMultipart }

func (f failingStaging) Put(ctx context.Context, b, k string, r io.Reader, size int64, ct string) error {
	if strings.HasPrefix(k, "staging/") {
		return errors.New("s3 unavailable")
	}
	return f.fakeMultipart.Put(ctx, b, k, r, size, ct)
}

// Machine and public PUTs are proxied: Euler writes the staging object itself
// while holding the project lock and never hands out a POST policy. Nothing is
// left that a client could replay, so the bucket's versioning policy can change
// right away, and a failed proxy write must not keep its reservation.
func TestProxyUploadDoesNotBlockVersioningOrHoldReservation(t *testing.T) {
	m, f, _ := testModule(t)
	advanced := multipartFake(f)
	m.plane = advanced
	s := scope()
	b := makeBucket(t, m, s, "private")
	k := decode[APIKey](t, request(t, m, s, "POST", "/keys", map[string]any{"name": "uploader", "scopes": []string{"write"}}), 201)
	put := func(key, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("PUT", "/s3/buckets/"+b.ID+"/objects/"+key, strings.NewReader(body))
		r.Header.Set("X-Access-Key", k.AccessKey)
		r.Header.Set("X-Secret-Key", k.SecretKey)
		r.Header.Set("Content-Type", "text/plain")
		w := httptest.NewRecorder()
		m.PublicHandler().ServeHTTP(w, r)
		return w
	}
	if w := put("report.txt", "hello"); w.Code != 201 {
		t.Fatalf("machine upload %d %s", w.Code, w.Body)
	}
	decode[map[string]string](t, request(t, m, s, "PUT", "/buckets/"+b.ID+"/versioning", map[string]string{"status": "Enabled"}), 200)

	m.plane = failingStaging{advanced}
	if w := put("second.txt", "world"); w.Code != 503 {
		t.Fatalf("failed staging write returned %d %s", w.Code, w.Body)
	}
	v := decode[struct {
		ReservedBytes int64 `json:"reservedBytes"`
	}](t, request(t, m, s, "GET", "/", nil), 200)
	if v.ReservedBytes != 0 {
		t.Fatalf("failed proxy upload still reserves %d bytes", v.ReservedBytes)
	}
	decode[map[string]string](t, request(t, m, s, "PUT", "/buckets/"+b.ID+"/versioning", map[string]string{"status": "Suspended"}), 200)
}
