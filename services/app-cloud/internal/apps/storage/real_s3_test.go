package storage

import (
	"bytes"
	"context"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

// TestRealS3 is mandatory in the native-apps CI job. Without an explicitly
// configured isolated S3 service it skips, rather than claiming a fake is S3.
func TestRealS3(t *testing.T) {
	endpoint := os.Getenv("EULER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set EULER_TEST_S3_ENDPOINT to run real S3 data-plane acceptance")
	}
	access, secret := os.Getenv("EULER_TEST_S3_ACCESS_KEY"), os.Getenv("EULER_TEST_S3_SECRET_KEY")
	if access == "" || secret == "" {
		t.Fatal("real S3 test requires explicit test credentials")
	}
	m, _, _ := testModule(t)
	t.Setenv("EULER_STORAGE_S3_ENDPOINT", endpoint)
	t.Setenv("EULER_STORAGE_S3_PUBLIC_ENDPOINT", endpoint)
	t.Setenv("EULER_STORAGE_S3_ACCESS_KEY", access)
	t.Setenv("EULER_STORAGE_S3_SECRET_KEY", secret)
	t.Setenv("EULER_STORAGE_S3_SECURE", os.Getenv("EULER_TEST_S3_SECURE"))
	t.Setenv("EULER_STORAGE_S3_CORS_MODE", os.Getenv("EULER_TEST_S3_CORS_MODE"))
	p, e := newS3()
	if e != nil {
		t.Fatal(e)
	}
	m.plane = p
	s := scope()
	b := makeBucket(t, m, s, "private")
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e := p.DeleteBucket(ctx, b.Name, true); e != nil && !absent(e) {
			t.Errorf("test bucket cleanup: %v", e)
		}
	})
	// Community MinIO uses an operator-configured global CORS origin. Verify
	// the real browser preflight instead of pretending bucket CORS was applied.
	scheme := "https"
	if os.Getenv("EULER_TEST_S3_SECURE") == "false" {
		scheme = "http"
	}
	preflight, err := http.NewRequest(http.MethodOptions, scheme+"://"+endpoint+"/"+b.Name, nil)
	if err != nil {
		t.Fatal(err)
	}
	preflight.Header.Set("Origin", "https://euler.example.test")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	preflight.Header.Set("Access-Control-Request-Headers", "content-type")
	preflightResponse, err := http.DefaultClient.Do(preflight)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, preflightResponse.Body)
	preflightResponse.Body.Close()
	if preflightResponse.StatusCode >= 400 || (preflightResponse.Header.Get("Access-Control-Allow-Origin") != "https://euler.example.test" && preflightResponse.Header.Get("Access-Control-Allow-Origin") != "*") {
		t.Fatalf("browser preflight not enabled: status=%d headers=%v", preflightResponse.StatusCode, preflightResponse.Header)
	}
	signed := decode[struct {
		UploadID  string    `json:"uploadId"`
		Signature Signature `json:"signature"`
	}](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-upload", map[string]any{"key": "reports/hello.txt", "size": 5, "contentType": "text/plain"}), 201)
	upload := func(content string) int {
		t.Helper()
		var body bytes.Buffer
		form := multipart.NewWriter(&body)
		for key, value := range signed.Signature.Fields {
			if e := form.WriteField(key, value); e != nil {
				t.Fatal(e)
			}
		}
		part, e := form.CreateFormFile("file", "hello.txt")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = io.WriteString(part, content); e != nil {
			t.Fatal(e)
		}
		if e = form.Close(); e != nil {
			t.Fatal(e)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		r, e := http.NewRequestWithContext(ctx, signed.Signature.Method, signed.Signature.URL, &body)
		if e != nil {
			t.Fatal(e)
		}
		r.Header.Set("Content-Type", form.FormDataContentType())
		res, e := http.DefaultClient.Do(r)
		if e != nil {
			t.Fatal(e)
		}
		defer res.Body.Close()
		response, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
		if len(content) == 5 && (res.StatusCode < 200 || res.StatusCode >= 300) {
			t.Fatalf("real signed POST failed %d: %s", res.StatusCode, response)
		}
		return res.StatusCode
	}
	if status := upload("oversized"); status >= 200 && status < 300 {
		t.Fatal("S3 accepted a body outside the signed size policy")
	}
	upload("hello")
	v := decode[Object](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/confirm-upload", map[string]string{"uploadId": signed.UploadID}), 200)
	if v.Size != 5 || v.Key != "reports/hello.txt" {
		t.Fatalf("published object: %+v", v)
	}
	if _, e = p.Stat(context.Background(), b.Name, "staging/"+signed.UploadID); !absent(e) {
		t.Fatal("confirmed staging object was not removed")
	}
	r, _, e := p.Get(context.Background(), b.Name, "files/reports/hello.txt")
	if e != nil {
		t.Fatal(e)
	}
	data, e := io.ReadAll(r)
	r.Close()
	if e != nil || string(data) != "hello" {
		t.Fatalf("real readback: %q %v", data, e)
	}
	list := decode[struct {
		Items []Object `json:"items"`
	}](t, request(t, m, s, "GET", "/buckets/"+b.ID+"/objects?prefix=reports/", nil), 200)
	if len(list.Items) != 1 || list.Items[0].Key != "reports/hello.txt" {
		t.Fatalf("real list: %+v", list)
	}
	signature := decode[Signature](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/objects/presigned-download", map[string]any{"key": "reports/hello.txt", "expires": 60}), 200)
	response, e := http.Get(signature.URL)
	if e != nil {
		t.Fatal(e)
	}
	download, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(download) != "hello" {
		t.Fatalf("signed download %d %q", response.StatusCode, download)
	}
	decode[Bucket](t, request(t, m, s, "PUT", "/buckets/"+b.ID, map[string]string{"name": "public-read-write", "access": "public-read-write"}), 200)
	anonymousURL := scheme + "://" + endpoint + "/" + b.Name + "/files/reports/hello.txt"
	response, e = http.Get(anonymousURL)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		t.Fatal("anonymous direct S3 read bypassed Euler installation and ACL checks")
	}
	raw, e := http.NewRequest(http.MethodPut, scheme+"://"+endpoint+"/"+b.Name+"/files/bypass.txt", strings.NewReader("bypass"))
	if e != nil {
		t.Fatal(e)
	}
	response, e = http.DefaultClient.Do(raw)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		t.Fatal("anonymous direct S3 write bypassed the quota gateway")
	}
	decode[Bucket](t, request(t, m, s, "POST", "/buckets/"+b.ID+"/sync", nil), 200)
	decode[map[string]bool](t, request(t, m, s, "DELETE", "/buckets/"+b.ID+"/objects?key=reports%2Fhello.txt", nil), 200)
	if _, e = p.Stat(context.Background(), b.Name, "files/reports/hello.txt"); !absent(e) {
		t.Fatal("real delete left object present")
	}
}
