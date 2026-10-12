package storage

import (
	"bytes"
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRealS3Advanced(t *testing.T) {
	endpoint := os.Getenv("EULER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("real isolated S3 endpoint required")
	}
	m, _, _ := testModule(t)
	t.Setenv("EULER_STORAGE_S3_ENDPOINT", endpoint)
	t.Setenv("EULER_STORAGE_S3_PUBLIC_ENDPOINT", endpoint)
	t.Setenv("EULER_STORAGE_S3_ACCESS_KEY", os.Getenv("EULER_TEST_S3_ACCESS_KEY"))
	t.Setenv("EULER_STORAGE_S3_SECRET_KEY", os.Getenv("EULER_TEST_S3_SECRET_KEY"))
	t.Setenv("EULER_STORAGE_S3_SECURE", os.Getenv("EULER_TEST_S3_SECURE"))
	t.Setenv("EULER_STORAGE_S3_CORS_MODE", os.Getenv("EULER_TEST_S3_CORS_MODE"))
	plane, e := newS3()
	if e != nil {
		t.Fatal(e)
	}
	m.plane = plane
	p := plane.(AdvancedPlane)
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID
	ctx := context.Background()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := plane.DeleteBucket(ctx, b.Name, true); e != nil && !absent(e) {
			t.Error(e)
		}
	})
	decode[map[string]string](t, request(t, m, s, "PUT", base+"/versioning", map[string]string{"status": "Enabled"}), 200)
	if w := request(t, m, s, "POST", base+"/objects/presigned-upload", map[string]any{"key": "replay.txt", "size": 1}); w.Code != 409 {
		t.Fatal("versioned bucket exposed POST replay", w.Code)
	}
	content := bytes.Repeat([]byte("a"), int(multipartPartSize)+17)
	session := decode[multipartSession](t, request(t, m, s, "POST", base+"/multipart", map[string]any{"key": "reports/large.txt", "size": len(content), "contentType": "text/plain"}), 201)
	for n := 1; n <= 2; n++ {
		part := content[int64(n-1)*multipartPartSize : min(int64(n)*multipartPartSize, int64(len(content)))]
		decode[UploadPart](t, partRequest(t, m, s, base+"/multipart/"+session.ID+"/parts/"+string(rune('0'+n)), part, partSHA(part)), 200)
	}
	resumed := decode[multipartSession](t, request(t, m, s, "GET", base+"/multipart/"+session.ID, nil), 200)
	if len(resumed.Parts) != 2 {
		t.Fatal("resume state missing")
	}
	decode[Object](t, request(t, m, s, "POST", base+"/multipart/"+session.ID+"/complete", nil), 200)
	// Model a database commit/acknowledgement lost after S3 publication. The
	// upload marker recovers without retaining the staging object or copying.
	if _, e = m.rt.DB.Exec("UPDATE storage_uploads SET state='pending' WHERE id=?", session.ID); e != nil {
		t.Fatal(e)
	}
	// Retrying completion must not produce another version.
	decode[Object](t, request(t, m, s, "POST", base+"/multipart/"+session.ID+"/complete", nil), 200)
	versions, _, e := p.Versions(ctx, b.Name, "files/reports/large.txt", "", 100)
	if e != nil || len(versions) != 1 {
		t.Fatalf("completion was not idempotent: %+v %v", versions, e)
	}
	first := versions[0]
	body, _, e := plane.Get(ctx, b.Name, "files/reports/large.txt")
	if e != nil {
		t.Fatal(e)
	}
	got, e := io.ReadAll(body)
	body.Close()
	if e != nil || !bytes.Equal(got, content) {
		t.Fatal("multipart bytes mismatch", e)
	}
	// Create another real version, then delete the current object to create a
	// delete marker. Historical bytes continue to count towards project quota.
	if e = plane.Put(ctx, b.Name, "files/reports/large.txt", strings.NewReader("new"), 3, "text/plain"); e != nil {
		t.Fatal(e)
	}
	if e = plane.Delete(ctx, b.Name, "files/reports/large.txt"); e != nil {
		t.Fatal(e)
	}
	versions, _, e = p.Versions(ctx, b.Name, "files/reports/large.txt", "", 100)
	if e != nil || len(versions) != 3 || !versions[0].DeleteMarker {
		t.Fatalf("version listing %+v %v", versions, e)
	}
	decode[map[string]bool](t, request(t, m, s, "POST", base+"/versions/restore", map[string]string{"key": "reports/large.txt", "versionId": first.VersionID}), 200)
	obj, e := plane.Stat(ctx, b.Name, "files/reports/large.txt")
	if e != nil || obj.Size != int64(len(content)) {
		t.Fatal("restore failed", obj, e)
	}
	size, count, e := p.MeasureVersions(ctx, b.Name)
	if e != nil || size != int64(len(content))*2+3 || count != 1 {
		t.Fatalf("all-version quota %d %d %v", size, count, e)
	}
	decode[map[string]bool](t, request(t, m, s, "DELETE", base+"/versions?key=reports%2Flarge.txt&versionId="+first.VersionID, nil), 200)
	size, _, e = p.MeasureVersions(ctx, b.Name)
	if e != nil || size != int64(len(content))+3 {
		t.Fatalf("permanent version delete quota %d %v", size, e)
	}
	rules := []LifecycleRule{{ID: "logs", Prefix: "logs/", Enabled: true, ExpirationDays: 30, NoncurrentDays: 7}}
	saved := decode[struct {
		Items []LifecycleRule `json:"items"`
	}](t, request(t, m, s, "PUT", base+"/lifecycle", map[string]any{"items": rules}), 200)
	if len(saved.Items) != 1 || saved.Items[0].ExpirationDays != 30 || saved.Items[0].NoncurrentDays != 7 {
		t.Fatal(saved)
	}
	cleared := decode[struct {
		Items []LifecycleRule `json:"items"`
	}](t, request(t, m, s, "PUT", base+"/lifecycle", map[string]any{"items": []LifecycleRule{}}), 200)
	if len(cleared.Items) != 0 {
		t.Fatal("lifecycle rule deletion did not persist")
	}
	decode[map[string]any](t, request(t, m, s, "POST", base+"/objects/transfer", map[string]any{"source": "reports/large.txt", "target": "archive/large.txt"}), 200)
	if w := request(t, m, s, "POST", base+"/objects/transfer", map[string]any{"source": "reports/large.txt", "target": "archive/large.txt", "move": true}); w.Code != 409 {
		t.Fatal("copy silently overwrote", w.Code)
	}
	aborted := decode[multipartSession](t, request(t, m, s, "POST", base+"/multipart", map[string]any{"key": "abort.txt", "size": 5}), 201)
	decode[UploadPart](t, partRequest(t, m, s, base+"/multipart/"+aborted.ID+"/parts/1", []byte("hello"), partSHA([]byte("hello"))), 200)
	decode[map[string]bool](t, request(t, m, s, "DELETE", base+"/multipart/"+aborted.ID, nil), 200)
	// Versioned staging cleanup removes bytes and markers, not just latest data.
	staged, _, e := p.Versions(ctx, b.Name, "staging/", "", 100)
	if e != nil || len(staged) != 0 {
		t.Fatalf("staging versions leaked %+v %v", staged, e)
	}
}
