package storage

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// listingS3 is a loopback S3 listing endpoint. It implements ListObjectsV2
// (prefix, delimiter roll-up, start-after, continuation token, max-keys) and
// ListObjectVersions the way S3 orders them, so the real minio-go adapter and
// its page decoding are exercised instead of a sorted in-memory fake.
type listingS3 struct{ keys []string }

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (f *listingS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if r.Method != http.MethodGet {
		http.Error(w, "unsupported", http.StatusNotImplemented)
		return
	}
	if _, ok := q["versions"]; ok {
		f.versions(w, q)
		return
	}
	if q.Get("list-type") != "2" {
		http.Error(w, "unsupported", http.StatusNotImplemented)
		return
	}
	prefix, delimiter := q.Get("prefix"), q.Get("delimiter")
	marker := q.Get("start-after")
	if token := q.Get("continuation-token"); token > marker {
		marker = token
	}
	maxKeys, _ := strconv.Atoi(q.Get("max-keys"))
	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	type entry struct {
		name   string
		folder bool
	}
	entries := []entry{}
	seen := map[string]bool{}
	for _, k := range f.keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name, folder := k, false
		if delimiter != "" {
			if i := strings.Index(k[len(prefix):], delimiter); i >= 0 {
				name, folder = k[:len(prefix)+i+len(delimiter)], true
			}
		}
		// A key rolled into a common prefix at or before the marker was
		// already returned as that prefix.
		if name <= marker || seen[name] {
			continue
		}
		seen[name] = true
		entries = append(entries, entry{name, folder})
	}
	truncated := len(entries) > maxKeys
	if truncated {
		entries = entries[:maxKeys]
	}
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, "<Name>bucket</Name><Prefix>%s</Prefix><Delimiter>%s</Delimiter><MaxKeys>%d</MaxKeys><KeyCount>%d</KeyCount><IsTruncated>%t</IsTruncated>", xmlText(prefix), xmlText(delimiter), maxKeys, len(entries), truncated)
	if truncated {
		fmt.Fprintf(&b, "<NextContinuationToken>%s</NextContinuationToken>", xmlText(entries[len(entries)-1].name))
	}
	// Like S3, one page carries <Contents> and <CommonPrefixes> separately.
	for _, e := range entries {
		if !e.folder {
			fmt.Fprintf(&b, `<Contents><Key>%s</Key><LastModified>2026-10-30T00:00:00.000Z</LastModified><ETag>"e-%s"</ETag><Size>1</Size><StorageClass>STANDARD</StorageClass></Contents>`, xmlText(e.name), xmlText(e.name))
		}
	}
	for _, e := range entries {
		if e.folder {
			fmt.Fprintf(&b, "<CommonPrefixes><Prefix>%s</Prefix></CommonPrefixes>", xmlText(e.name))
		}
	}
	b.WriteString("</ListBucketResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}
func (f *listingS3) versions(w http.ResponseWriter, q url.Values) {
	prefix, marker := q.Get("prefix"), q.Get("key-marker")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><ListVersionsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	fmt.Fprintf(&b, "<Name>bucket</Name><Prefix>%s</Prefix><MaxKeys>1000</MaxKeys><IsTruncated>false</IsTruncated>", xmlText(prefix))
	for _, k := range f.keys {
		if strings.HasPrefix(k, prefix) && k > marker {
			fmt.Fprintf(&b, `<Version><Key>%s</Key><VersionId>null</VersionId><IsLatest>true</IsLatest><LastModified>2026-10-30T00:00:00.000Z</LastModified><ETag>"e"</ETag><Size>1</Size><StorageClass>STANDARD</StorageClass></Version>`, xmlText(k))
		}
	}
	b.WriteString("</ListVersionsResult>")
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, b.String())
}
func listingPlane(t *testing.T, keys []string) *s3Plane {
	t.Helper()
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	server := httptest.NewServer(&listingS3{keys: sorted})
	t.Cleanup(server.Close)
	host := strings.TrimPrefix(server.URL, "http://")
	client, e := minio.New(host, &minio.Options{Creds: credentials.NewStaticV4("test-access", "test-secret", ""), Secure: false, Region: "us-east-1"})
	if e != nil {
		t.Fatal(e)
	}
	return &s3Plane{client: client, public: client, region: "us-east-1", corsMode: "external"}
}

// The console and machine API resume a folder listing from "next". A page that
// holds both files and folders must hand back a cursor past every entry it
// returned, or the following page repeats them.
func TestNonRecursiveListingCursorAdvancesPastFilesAndFolders(t *testing.T) {
	m, _, _ := testModule(t)
	keys := []string{"files/a/x.txt", "files/b.txt", "files/c/y.txt", "files/d.txt", "files/e.txt", "files/f/z.txt", "files/g.txt"}
	m.plane = listingPlane(t, keys)
	s := scope()
	if _, e := m.rt.DB.Exec("INSERT INTO storage_buckets VALUES(?,?,?,?,?,?,?,?,?,0,0,?,?,?)", "bucket_list", s.TenantID, s.ProjectID, s.InstallationID, "eu-listing", "Listing", "", "private", "active", "[]", "2026-10-30T00:00:00Z", "2026-10-30T00:00:00Z"); e != nil {
		t.Fatal(e)
	}
	type page struct {
		Items   []Object `json:"items"`
		HasMore bool     `json:"hasMore"`
		Next    string   `json:"next"`
	}
	seen := map[string]int{}
	after := ""
	for i := 0; i < 10; i++ {
		v := decode[page](t, request(t, m, s, "GET", "/buckets/bucket_list/objects?limit=2&after="+url.QueryEscape(after), nil), 200)
		for _, item := range v.Items {
			seen[item.Key]++
		}
		if !v.HasMore {
			break
		}
		if v.Next <= after {
			t.Fatalf("cursor did not advance: %q after %q", v.Next, after)
		}
		after = v.Next
	}
	want := []string{"a/", "b.txt", "c/", "d.txt", "e.txt", "f/", "g.txt"}
	for _, key := range want {
		if seen[key] != 1 {
			t.Fatalf("entry %q listed %d times across pages: %v", key, seen[key], seen)
		}
	}
	if len(seen) != len(want) {
		t.Fatalf("unexpected entries: %v", seen)
	}
}

// Returning a page early must not strand minio-go's listing producer: each
// stranded goroutine and its buffered entry live until the process exits.
func TestListingPagesDoNotLeakGoroutines(t *testing.T) {
	keys := []string{}
	for i := 0; i < 120; i++ {
		keys = append(keys, fmt.Sprintf("files/k%03d", i))
	}
	p := listingPlane(t, keys)
	ctx := context.Background()
	run := func() {
		if items, more, e := p.List(ctx, "bucket", "files/", true, "", 2); e != nil || !more || len(items) != 2 {
			t.Fatalf("list page: %d %v %v", len(items), more, e)
		}
		if items, next, e := p.Versions(ctx, "bucket", "files/", "", 100); e != nil || next == "" || len(items) != 100 {
			t.Fatalf("version page: %d %q %v", len(items), next, e)
		}
	}
	// Poll with a deadline: goroutines that are merely finishing (connection
	// handlers, a just-returned request) may need a moment; a stranded producer
	// never exits, so a leak still fails once the deadline passes.
	settle := func(done func(int) bool) int {
		deadline := time.Now().Add(3 * time.Second)
		last := runtime.NumGoroutine()
		for !done(last) && time.Now().Before(deadline) {
			time.Sleep(25 * time.Millisecond)
			last = runtime.NumGoroutine()
		}
		return last
	}
	run()
	previous := -1
	baseline := settle(func(n int) bool { stable := n == previous; previous = n; return stable })
	for i := 0; i < 30; i++ {
		run()
	}
	if n := settle(func(n int) bool { return n <= baseline+4 }); n > baseline+4 {
		t.Fatalf("listing leaked goroutines: %d before, %d after 30 early-returning pages", baseline, n)
	}
}
