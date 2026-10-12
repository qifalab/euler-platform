package trust

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/qifalab/euler-platform/services/app-cloud/internal/appkit"
)

func TestRealS3PrivateMaterialCompatibilityAndIsolation(t *testing.T) {
	endpoint := os.Getenv("EULER_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("requires isolated real S3 service")
	}
	f := newFixture(t)
	scheme := schemeFixture(t, f)
	alice := person("alice", "read", "write")
	ctx := context.Background()
	// Existing inline materials remain readable after enabling the optional S3 backend.
	t.Setenv("EULER_TRUST_MATERIAL_S3_BUCKET", "")
	legacy := upload(t, f, alice, scheme)
	bucket := "trust-test-" + strings.ToLower(appkit.NewID(""))
	bucket = strings.ReplaceAll(bucket, "_", "")
	secure := os.Getenv("EULER_TEST_S3_SECURE") != "false"
	client, e := minio.New(endpoint, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("EULER_TEST_S3_ACCESS_KEY"), os.Getenv("EULER_TEST_S3_SECRET_KEY"), ""), Secure: secure})
	if e != nil {
		t.Fatal(e)
	}
	if e = client.MakeBucket(ctx, bucket, minio.MakeBucketOptions{}); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		for v := range client.ListObjects(context.Background(), bucket, minio.ListObjectsOptions{Recursive: true, WithVersions: true}) {
			if v.Err == nil {
				_ = client.RemoveObject(context.Background(), bucket, v.Key, minio.RemoveObjectOptions{VersionID: v.VersionID})
			}
		}
		_ = client.RemoveBucket(context.Background(), bucket)
	})
	t.Setenv("EULER_TRUST_MATERIAL_S3_BUCKET", bucket)
	t.Setenv("EULER_TRUST_MATERIAL_S3_ENDPOINT", endpoint)
	t.Setenv("EULER_TRUST_MATERIAL_S3_ACCESS_KEY", os.Getenv("EULER_TEST_S3_ACCESS_KEY"))
	t.Setenv("EULER_TRUST_MATERIAL_S3_SECRET_KEY", os.Getenv("EULER_TEST_S3_SECRET_KEY"))
	t.Setenv("EULER_TRUST_MATERIAL_S3_SECURE", os.Getenv("EULER_TEST_S3_SECURE"))
	expect(t, request(t, f.m, alice, "GET", "/materials/"+legacy.ID, nil), 200)
	material := upload(t, f, alice, scheme)
	var cipher []byte
	if e = f.rt.DB.QueryRow("SELECT body FROM trust_materials WHERE id=?", material.ID).Scan(&cipher); e != nil {
		t.Fatal(e)
	}
	plain, e := f.rt.Decrypt(cipher, "trust:material-body:"+material.ID)
	if e != nil || !strings.HasPrefix(plain, materialObjectPrefix) {
		t.Fatal("encrypted reference missing", e)
	}
	var ref materialObjectRef
	if e = json.Unmarshal([]byte(strings.TrimPrefix(plain, materialObjectPrefix)), &ref); e != nil {
		t.Fatal(e)
	}
	obj, e := client.GetObject(ctx, bucket, ref.Key, minio.GetObjectOptions{})
	if e != nil {
		t.Fatal(e)
	}
	raw, e := io.ReadAll(obj)
	obj.Close()
	if e != nil || bytes.Contains(raw, []byte("secret evidence material")) {
		t.Fatal("S3 stored cleartext", e)
	}
	download := request(t, f.m, alice, "GET", "/materials/"+material.ID, nil)
	expect(t, download, 200)
	if download.Body.String() != "secret evidence material" {
		t.Fatal("wrong material payload")
	}
	expect(t, request(t, f.m, person("bob", "read", "write"), "GET", "/materials/"+material.ID, nil), 404)
	expect(t, request(t, f.m, person("storage-admin", "read", "manage", "secrets", "admin"), "GET", "/materials/"+material.ID, nil), 404)
	expect(t, request(t, f.m, person("storage-admin", "read", "manage", "secrets", "admin"), "GET", "/review/materials/"+material.ID, nil), 403)
	schemeURL := "https"
	if !secure {
		schemeURL = "http"
	}
	response, e := http.Get(schemeURL + "://" + endpoint + "/" + bucket + "/" + ref.Key)
	if e != nil {
		t.Fatal(e)
	}
	io.Copy(io.Discard, response.Body)
	response.Body.Close()
	if response.StatusCode < 400 {
		t.Fatal("anonymous S3 access allowed")
	}
	expect(t, request(t, f.m, alice, "DELETE", "/materials/"+material.ID, nil), 204)
	if _, e = client.StatObject(ctx, bucket, ref.Key, minio.StatObjectOptions{}); minio.ToErrorResponse(e).StatusCode != 404 {
		t.Fatal("deleted material ciphertext survived", e)
	}
	// A scheme deletion bypasses individual material endpoints. Its encrypted
	// orphan is still collected from the durable ledger by maintenance.
	orphan := upload(t, f, alice, scheme)
	if e = f.rt.DB.QueryRow("SELECT body FROM trust_materials WHERE id=?", orphan.ID).Scan(&cipher); e != nil {
		t.Fatal(e)
	}
	plain, _ = f.rt.Decrypt(cipher, "trust:material-body:"+orphan.ID)
	_ = json.Unmarshal([]byte(strings.TrimPrefix(plain, materialObjectPrefix)), &ref)
	expect(t, request(t, f.m, person("admin", "admin"), "DELETE", "/schemes/"+scheme.ID, nil), 204)
	_, _ = f.rt.DB.Exec("UPDATE trust_material_objects SET created_at=?", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339Nano))
	if e = f.m.Maintain(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = client.StatObject(ctx, bucket, ref.Key, minio.StatObjectOptions{}); minio.ToErrorResponse(e).StatusCode != 404 {
		t.Fatal("orphan ciphertext survived", e)
	}
	// Fail closed if a deployment accidentally points Trust at a user-managed bucket.
	_, _ = f.rt.DB.Exec("CREATE TABLE storage_buckets(name TEXT)")
	_, _ = f.rt.DB.Exec("INSERT INTO storage_buckets VALUES(?)", bucket)
	if _, e = f.m.materialS3(ctx, bucket); e == nil {
		t.Fatal("project bucket accepted for private materials")
	}
}
