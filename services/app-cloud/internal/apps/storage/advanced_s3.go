package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/lifecycle"
)

// AdvancedPlane is optional so installations can explicitly report an older
// data-plane adapter. Euler remains a REST control plane, not a SigV4 proxy.
type AdvancedPlane interface {
	BeginMultipart(context.Context, string, string, string) (string, error)
	UploadPart(context.Context, string, string, string, int, io.Reader, int64, string) (string, error)
	CompleteMultipart(context.Context, string, string, string, []UploadPart) error
	AbortMultipart(context.Context, string, string, string) error
	PurgeKey(context.Context, string, string) error
	Versioning(context.Context, string) (string, error)
	SetVersioning(context.Context, string, string) error
	Versions(context.Context, string, string, string, int) ([]ObjectVersion, string, error)
	StatVersion(context.Context, string, string, string) (Object, error)
	RestoreVersion(context.Context, string, string, string, string) error
	DeleteVersion(context.Context, string, string, string) error
	GetLifecycle(context.Context, string) ([]LifecycleRule, error)
	SetLifecycle(context.Context, string, []LifecycleRule) error
	MeasureVersions(context.Context, string) (int64, int64, error)
}

type UploadPart struct {
	Number int    `json:"number"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag"`
	SHA256 string `json:"sha256"`
}
type ObjectVersion struct {
	Object
	VersionID    string `json:"versionId"`
	IsLatest     bool   `json:"isLatest"`
	DeleteMarker bool   `json:"deleteMarker"`
}
type LifecycleRule struct {
	ID             string `json:"id"`
	Prefix         string `json:"prefix"`
	Enabled        bool   `json:"enabled"`
	ExpirationDays int    `json:"expirationDays"`
	NoncurrentDays int    `json:"noncurrentDays"`
}

func (p *s3Plane) BeginMultipart(ctx context.Context, b, k, contentType string) (string, error) {
	return (minio.Core{Client: p.client}).NewMultipartUpload(ctx, b, k, minio.PutObjectOptions{ContentType: contentType})
}
func (p *s3Plane) UploadPart(ctx context.Context, b, k, id string, n int, r io.Reader, size int64, sha string) (string, error) {
	v, e := (minio.Core{Client: p.client}).PutObjectPart(ctx, b, k, id, n, r, size, minio.PutObjectPartOptions{Sha256Hex: sha})
	return v.ETag, e
}
func (p *s3Plane) CompleteMultipart(ctx context.Context, b, k, id string, parts []UploadPart) error {
	all := make([]minio.CompletePart, 0, len(parts))
	for _, v := range parts {
		all = append(all, minio.CompletePart{PartNumber: v.Number, ETag: v.ETag})
	}
	_, e := (minio.Core{Client: p.client}).CompleteMultipartUpload(ctx, b, k, id, all, minio.PutObjectOptions{})
	return e
}
func (p *s3Plane) AbortMultipart(ctx context.Context, b, k, id string) error {
	e := (minio.Core{Client: p.client}).AbortMultipartUpload(ctx, b, k, id)
	if minio.ToErrorResponse(e).Code == "NoSuchUpload" {
		return nil
	}
	return e
}

// Staging keys and forced bucket deletion must remove every version; a normal
// versioned DeleteObject only creates a delete marker and would leak capacity.
func (p *s3Plane) PurgeKey(ctx context.Context, b, k string) error {
	for v := range p.client.ListIncompleteUploads(ctx, b, k, true) {
		if v.Err != nil {
			return v.Err
		}
		if v.Key != k {
			continue
		}
		if e := p.AbortMultipart(ctx, b, k, v.UploadID); e != nil {
			return e
		}
	}
	for v := range p.client.ListObjects(ctx, b, minio.ListObjectsOptions{Prefix: k, Recursive: true, WithVersions: true}) {
		if v.Err != nil {
			return v.Err
		}
		if v.Key != k {
			continue
		}
		if e := p.client.RemoveObject(ctx, b, k, minio.RemoveObjectOptions{VersionID: v.VersionID}); e != nil {
			return e
		}
	}
	return nil
}
func (p *s3Plane) Versioning(ctx context.Context, b string) (string, error) {
	v, e := p.client.GetBucketVersioning(ctx, b)
	if e != nil {
		return "", e
	}
	if v.Status == "" {
		return "Disabled", nil
	}
	return v.Status, nil
}
func (p *s3Plane) SetVersioning(ctx context.Context, b, status string) error {
	return p.client.SetBucketVersioning(ctx, b, minio.BucketVersioningConfiguration{Status: status})
}
func (p *s3Plane) Versions(ctx context.Context, b, prefix, cursor string, limit int) ([]ObjectVersion, string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := []ObjectVersion{}
	next := ""
	passed := cursor == ""
	var after [2]string
	if cursor != "" {
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(raw, &after) != nil {
			return nil, "", errors.New("invalid cursor")
		}
	}
	for v := range p.client.ListObjects(ctx, b, minio.ListObjectsOptions{Prefix: prefix, Recursive: true, WithVersions: true}) {
		if v.Err != nil {
			return nil, "", v.Err
		}
		if !passed {
			if v.Key == after[0] && v.VersionID == after[1] {
				passed = true
			}
			continue
		}
		if len(out) == limit {
			return out, next, nil
		}
		out = append(out, ObjectVersion{Object: fromS3(v), VersionID: v.VersionID, IsLatest: v.IsLatest, DeleteMarker: v.IsDeleteMarker})
		raw, _ := json.Marshal([2]string{v.Key, v.VersionID})
		next = base64.RawURLEncoding.EncodeToString(raw)
	}
	return out, "", nil
}
func (p *s3Plane) StatVersion(ctx context.Context, b, k, id string) (Object, error) {
	v, e := p.client.StatObject(ctx, b, k, minio.StatObjectOptions{VersionID: id})
	return fromS3(v), e
}
func (p *s3Plane) RestoreVersion(ctx context.Context, b, k, id, etag string) error {
	_, e := p.client.ComposeObject(ctx, minio.CopyDestOptions{Bucket: b, Object: k}, minio.CopySrcOptions{Bucket: b, Object: k, VersionID: id, MatchETag: etag})
	return e
}
func (p *s3Plane) DeleteVersion(ctx context.Context, b, k, id string) error {
	return p.client.RemoveObject(ctx, b, k, minio.RemoveObjectOptions{VersionID: id})
}
func (p *s3Plane) GetLifecycle(ctx context.Context, b string) ([]LifecycleRule, error) {
	c, e := p.client.GetBucketLifecycle(ctx, b)
	if minio.ToErrorResponse(e).Code == "NoSuchLifecycleConfiguration" {
		return []LifecycleRule{}, nil
	}
	if e != nil {
		return nil, e
	}
	out := []LifecycleRule{}
	for _, r := range c.Rules {
		if !strings.HasPrefix(r.ID, "euler-files-") {
			continue
		}
		out = append(out, LifecycleRule{ID: strings.TrimPrefix(r.ID, "euler-files-"), Prefix: strings.TrimPrefix(r.RuleFilter.Prefix, "files/"), Enabled: r.Status == "Enabled", ExpirationDays: int(r.Expiration.Days), NoncurrentDays: int(r.NoncurrentVersionExpiration.NoncurrentDays)})
	}
	return out, nil
}
func (p *s3Plane) SetLifecycle(ctx context.Context, b string, rules []LifecycleRule) error {
	// Preserve rules installed by an operator; Euler only owns its ID prefix.
	c, e := p.client.GetBucketLifecycle(ctx, b)
	if e != nil && minio.ToErrorResponse(e).Code != "NoSuchLifecycleConfiguration" {
		return e
	}
	if c == nil {
		c = lifecycle.NewConfiguration()
	}
	kept := make([]lifecycle.Rule, 0, len(c.Rules)+len(rules))
	for _, r := range c.Rules {
		if !strings.HasPrefix(r.ID, "euler-files-") {
			kept = append(kept, r)
		}
	}
	for _, r := range rules {
		status := "Disabled"
		if r.Enabled {
			status = "Enabled"
		}
		kept = append(kept, lifecycle.Rule{ID: "euler-files-" + r.ID, Status: status, RuleFilter: lifecycle.Filter{Prefix: "files/" + r.Prefix}, Expiration: lifecycle.Expiration{Days: lifecycle.ExpirationDays(r.ExpirationDays)}, NoncurrentVersionExpiration: lifecycle.NoncurrentVersionExpiration{NoncurrentDays: lifecycle.ExpirationDays(r.NoncurrentDays)}})
	}
	c.Rules = kept
	return p.client.SetBucketLifecycle(ctx, b, c)
}
func (p *s3Plane) MeasureVersions(ctx context.Context, b string) (int64, int64, error) {
	var size, count int64
	for v := range p.client.ListObjects(ctx, b, minio.ListObjectsOptions{Prefix: "files/", Recursive: true, WithVersions: true}) {
		if v.Err != nil {
			return 0, 0, v.Err
		}
		if !v.IsDeleteMarker {
			size += v.Size
		}
		if v.IsLatest && !v.IsDeleteMarker && !strings.HasSuffix(v.Key, "/") {
			count++
		}
	}
	return size, count, nil
}

// Recover an acknowledgement lost after S3 copied the object. The durable
// upload marker avoids creating another version on retry.
func (p *s3Plane) Published(ctx context.Context, b, k, id string) (bool, error) {
	v, e := p.client.StatObject(ctx, b, k, minio.StatObjectOptions{})
	if absent(e) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	for key, value := range v.UserMetadata {
		if strings.EqualFold(key, "euler-upload-id") && value == id {
			return true, nil
		}
	}
	return false, nil
}
func (p *s3Plane) Publish(ctx context.Context, b, source, target, etag, id, ct string) error {
	_, e := p.client.ComposeObject(ctx, minio.CopyDestOptions{Bucket: b, Object: target, ReplaceMetadata: true, ContentType: ct, UserMetadata: map[string]string{"euler-upload-id": id}}, minio.CopySrcOptions{Bucket: b, Object: source, MatchETag: etag})
	return e
}
func (m *Module) DependencyStatus(ctx context.Context) map[string]string {
	if m.plane == nil {
		return map[string]string{"s3": "not_configured"}
	}
	p, ok := m.plane.(*s3Plane)
	if !ok {
		return map[string]string{"s3": "configured_unverified"}
	}
	_, e := p.client.ListBuckets(ctx)
	status := "up"
	if e != nil {
		status = "down"
	}
	return map[string]string{"s3": status}
}
