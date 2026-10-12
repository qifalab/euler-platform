package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

// This adapter tests controller boundaries only. TestRealS3Advanced separately
// exercises real S3 multipart, versioning, lifecycle and cleanup semantics.
type fakeMultipart struct {
	*fakeS3
	muParts sync.Mutex
	uploads map[string]map[int][]byte
	aborted int
	status  string
}

func multipartFake(f *fakeS3) *fakeMultipart {
	return &fakeMultipart{fakeS3: f, uploads: map[string]map[int][]byte{}}
}
func (f *fakeMultipart) BeginMultipart(_ context.Context, b, k, ct string) (string, error) {
	f.muParts.Lock()
	defer f.muParts.Unlock()
	f.uploads[k] = map[int][]byte{}
	return k, nil
}
func (f *fakeMultipart) UploadPart(_ context.Context, b, k, id string, n int, r io.Reader, size int64, sha string) (string, error) {
	data, e := io.ReadAll(r)
	if e != nil {
		return "", e
	}
	f.muParts.Lock()
	defer f.muParts.Unlock()
	if f.uploads[k] == nil {
		return "", errors.New("missing upload")
	}
	f.uploads[k][n] = data
	return fmt.Sprintf("part-%d", n), nil
}
func (f *fakeMultipart) CompleteMultipart(ctx context.Context, b, k, id string, parts []UploadPart) error {
	f.muParts.Lock()
	defer f.muParts.Unlock()
	var data []byte
	for _, part := range parts {
		data = append(data, f.uploads[k][part.Number]...)
	}
	delete(f.uploads, k)
	return f.Put(ctx, b, k, bytes.NewReader(data), int64(len(data)), "text/plain")
}
func (f *fakeMultipart) AbortMultipart(_ context.Context, b, k, id string) error {
	f.muParts.Lock()
	defer f.muParts.Unlock()
	delete(f.uploads, k)
	f.aborted++
	return nil
}
func (f *fakeMultipart) PurgeKey(ctx context.Context, b, k string) error { return f.Delete(ctx, b, k) }
func (f *fakeMultipart) Versioning(context.Context, string) (string, error) {
	if f.status != "" {
		return f.status, nil
	}
	return "Disabled", nil
}
func (f *fakeMultipart) SetVersioning(_ context.Context, _ string, status string) error {
	f.status = status
	return nil
}
func (f *fakeMultipart) Versions(context.Context, string, string, string, int) ([]ObjectVersion, string, error) {
	return nil, "", errors.New("unsupported controller fake")
}
func (f *fakeMultipart) StatVersion(context.Context, string, string, string) (Object, error) {
	return Object{}, errors.New("unsupported controller fake")
}
func (f *fakeMultipart) RestoreVersion(context.Context, string, string, string, string) error {
	return errors.New("unsupported controller fake")
}
func (f *fakeMultipart) DeleteVersion(context.Context, string, string, string) error {
	return errors.New("unsupported controller fake")
}
func (f *fakeMultipart) GetLifecycle(context.Context, string) ([]LifecycleRule, error) {
	return nil, errors.New("unsupported controller fake")
}
func (f *fakeMultipart) SetLifecycle(context.Context, string, []LifecycleRule) error {
	return errors.New("unsupported controller fake")
}
func (f *fakeMultipart) MeasureVersions(ctx context.Context, b string) (int64, int64, error) {
	items, _, e := f.List(ctx, b, "files/", true, "", 10000)
	var size, count int64
	for _, v := range items {
		size += v.Size
		if !v.Folder {
			count++
		}
	}
	return size, count, e
}
func partRequest(t *testing.T, m *Module, s appkit.Scope, path string, data []byte, digest string) *httptest.ResponseRecorder {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	_ = form.WriteField("sha256", digest)
	part, e := form.CreateFormFile("file", "part")
	if e != nil {
		t.Fatal(e)
	}
	_, _ = part.Write(data)
	_ = form.Close()
	r := httptest.NewRequest("PUT", path, &body)
	r.Header.Set("Content-Type", form.FormDataContentType())
	r = r.WithContext(appkit.WithScope(r.Context(), s))
	w := httptest.NewRecorder()
	m.Handler().ServeHTTP(w, r)
	return w
}
func partSHA(data []byte) string { d := sha256.Sum256(data); return hex.EncodeToString(d[:]) }
func TestMultipartResumeIsolationQuotaAndAbort(t *testing.T) {
	m, f, _ := testModule(t)
	advanced := multipartFake(f)
	m.plane = advanced
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID + "/multipart"
	v := decode[multipartSession](t, request(t, m, s, "POST", base, map[string]any{"key": "resume.txt", "size": 5}), 201)
	other := s
	other.ActorID = "bob"
	if w := request(t, m, other, "GET", base+"/"+v.ID, nil); w.Code != 404 {
		t.Fatalf("other actor read %d %s", w.Code, w.Body)
	}
	other = s
	other.ProjectID = "other"
	if w := request(t, m, other, "GET", base+"/"+v.ID, nil); w.Code != 404 {
		t.Fatalf("other project read %d", w.Code)
	}
	if w := partRequest(t, m, s, base+"/"+v.ID+"/parts/1", []byte("hello"), "wrong"); w.Code != 400 {
		t.Fatalf("checksum accepted %d", w.Code)
	}
	if w := partRequest(t, m, s, base+"/"+v.ID+"/parts/1", []byte("larger"), partSHA([]byte("larger"))); w.Code != 400 {
		t.Fatalf("oversized part accepted %d", w.Code)
	}
	part := decode[UploadPart](t, partRequest(t, m, s, base+"/"+v.ID+"/parts/1", []byte("hello"), partSHA([]byte("hello"))), 200)
	if part.Size != 5 {
		t.Fatal(part)
	}
	decode[UploadPart](t, partRequest(t, m, s, base+"/"+v.ID+"/parts/1", []byte("hello"), partSHA([]byte("hello"))), 200)
	if w := partRequest(t, m, s, base+"/"+v.ID+"/parts/1", []byte("other"), partSHA([]byte("other"))); w.Code != 409 {
		t.Fatalf("different retry accepted %d", w.Code)
	}
	resumed := decode[multipartSession](t, request(t, m, s, "GET", base+"/"+v.ID, nil), 200)
	if len(resumed.Parts) != 1 || resumed.Parts[0].SHA256 != partSHA([]byte("hello")) {
		t.Fatal(resumed)
	}
	decode[Object](t, request(t, m, s, "POST", base+"/"+v.ID+"/complete", nil), 200)
	decode[Object](t, request(t, m, s, "POST", base+"/"+v.ID+"/complete", nil), 200)
	if w := request(t, m, s, "POST", base, map[string]any{"key": "resume.txt", "size": 5}); w.Code != 409 {
		t.Fatalf("silent overwrite %d", w.Code)
	}
	v = decode[multipartSession](t, request(t, m, s, "POST", base, map[string]any{"key": "aborted.txt", "size": 7}), 201)
	decode[map[string]bool](t, request(t, m, s, "DELETE", base+"/"+v.ID, nil), 200)
	if advanced.aborted == 0 {
		t.Fatal("S3 abort not called")
	}
	var pending int
	_ = m.rt.DB.QueryRow("SELECT COUNT(*) FROM storage_uploads WHERE state='pending'").Scan(&pending)
	if pending != 0 {
		t.Fatal("reservation not released")
	}
}
func TestMultipartReservationsAndExpiry(t *testing.T) {
	m, f, _ := testModule(t)
	advanced := multipartFake(f)
	m.plane = advanced
	m.ledger.base.StorageBytes = 10
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID + "/multipart"
	v := decode[multipartSession](t, request(t, m, s, "POST", base, map[string]any{"key": "reserve", "size": 8}), 201)
	if w := request(t, m, s, "POST", base, map[string]any{"key": "overbook", "size": 3}); w.Code != 409 {
		t.Fatalf("overbook %d", w.Code)
	}
	_, _ = m.rt.DB.Exec("UPDATE storage_uploads SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), v.ID)
	m.cleanup(context.Background())
	if advanced.aborted == 0 {
		t.Fatal("expired multipart not aborted")
	}
	var state string
	_ = m.rt.DB.QueryRow("SELECT state FROM storage_uploads WHERE id=?", v.ID).Scan(&state)
	if state != "expired" {
		t.Fatal(state)
	}
	decode[multipartSession](t, request(t, m, s, "POST", base, map[string]any{"key": "after-expiry", "size": 10}), 201)
}
func TestMultipartCompletionRequiresAllParts(t *testing.T) {
	m, f, _ := testModule(t)
	m.plane = multipartFake(f)
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID + "/multipart"
	v := decode[multipartSession](t, request(t, m, s, "POST", base, map[string]any{"key": "missing", "size": multipartPartSize + 1}), 201)
	if w := request(t, m, s, "POST", base+"/"+v.ID+"/complete", nil); w.Code != 409 {
		t.Fatalf("completed missing parts %d", w.Code)
	}
	if w := partRequest(t, m, s, base+"/"+v.ID+"/parts/3", []byte("a"), partSHA([]byte("a"))); w.Code != 400 {
		t.Fatalf("out of range part %d", w.Code)
	}
}
func TestTransferNeverOverwritesAndKeepsScope(t *testing.T) {
	m, f, _ := testModule(t)
	s := scope()
	b := makeBucket(t, m, s, "private")
	ctx := context.Background()
	_ = f.Put(ctx, b.Name, "files/source", strings.NewReader("hello"), 5, "text/plain")
	_ = f.Put(ctx, b.Name, "files/existing", strings.NewReader("old"), 3, "text/plain")
	path := "/buckets/" + b.ID + "/objects/transfer"
	if w := request(t, m, s, "POST", path, map[string]any{"source": "source", "target": "existing", "move": true}); w.Code != 409 {
		t.Fatalf("overwrote %d", w.Code)
	}
	decode[map[string]any](t, request(t, m, s, "POST", path, map[string]any{"source": "source", "target": "copied"}), 200)
	decode[map[string]any](t, request(t, m, s, "POST", path, map[string]any{"source": "copied", "target": "moved", "move": true}), 200)
	if _, e := f.Stat(ctx, b.Name, "files/copied"); !absent(e) {
		t.Fatal("source retained")
	}
	reader := s
	reader.Permissions = []string{"read"}
	if w := request(t, m, reader, "POST", path, map[string]string{"source": "source", "target": "denied"}); w.Code != 403 {
		t.Fatal(w.Code)
	}
	if w := request(t, m, reader, "DELETE", "/buckets/"+b.ID+"/versions?key=source&versionId=null", nil); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestUploadETagGuardPreventsLostUpdates(t *testing.T) {
	m, f, _ := testModule(t)
	s := scope()
	b := makeBucket(t, m, s, "private")
	ctx := context.Background()
	_ = f.Put(ctx, b.Name, "files/file", strings.NewReader("old"), 3, "text/plain")
	old, _ := f.Stat(ctx, b.Name, "files/file")
	signed := decode[struct {
		UploadID string `json:"uploadId"`
	}](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": "file", "size": 3, "expectedETag": old.ETag}), 201)
	_ = f.Put(ctx, b.Name, "staging/"+signed.UploadID, strings.NewReader("new"), 3, "text/plain")
	_ = f.Put(ctx, b.Name, "files/file", strings.NewReader("changed"), 7, "text/plain")
	if w := request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/confirm-upload", map[string]string{"uploadId": signed.UploadID}); w.Code != 409 {
		t.Fatalf("lost update %d %s", w.Code, w.Body)
	}
}

func TestVersionPolicyRejectsReusablePOSTCapabilities(t *testing.T) {
	m, f, _ := testModule(t)
	m.plane = multipartFake(f)
	s := scope()
	b := makeBucket(t, m, s, "private")
	base := "/buckets/" + b.ID
	signed := decode[struct {
		UploadID string `json:"uploadId"`
	}](t, request(t, m, s, "POST", base+"/objects/presigned-upload", map[string]any{"key": "direct.txt", "size": 1}), 201)
	// A successfully completed upload still has a usable POST policy.
	_, _ = m.rt.DB.Exec("UPDATE storage_uploads SET state='completed' WHERE id=?", signed.UploadID)
	if w := request(t, m, s, "PUT", base+"/versioning", map[string]string{"status": "Enabled"}); w.Code != 409 {
		t.Fatalf("live signature survived version transition: %d", w.Code)
	}
	_, _ = m.rt.DB.Exec("UPDATE storage_uploads SET expires_at=? WHERE id=?", time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano), signed.UploadID)
	decode[map[string]string](t, request(t, m, s, "PUT", base+"/versioning", map[string]string{"status": "Enabled"}), 200)
	if w := request(t, m, s, "POST", base+"/objects/presigned-upload", map[string]any{"key": "direct.txt", "size": 1}); w.Code != 409 {
		t.Fatalf("versioned bucket issued reusable POST: %d", w.Code)
	}
}
