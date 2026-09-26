package storage

import (
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/cors"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

type Object struct {
	Key         string    `json:"key"`
	Size        int64     `json:"size"`
	ContentType string    `json:"contentType"`
	ETag        string    `json:"etag"`
	ModifiedAt  time.Time `json:"modifiedAt"`
	Folder      bool      `json:"folder"`
}
type Signature struct {
	URL       string            `json:"url"`
	Method    string            `json:"method"`
	Fields    map[string]string `json:"fields,omitempty"`
	ExpiresAt string            `json:"expiresAt"`
}

// DataPlane keeps authorization in this module, while real file bytes travel to
// the configured S3 service. Fakes are confined to tests.
type DataPlane interface {
	CreateBucket(context.Context, string) error
	DeleteBucket(context.Context, string, bool) error
	SetAccess(context.Context, string, string) error
	SetCORS(context.Context, string, []string) error
	List(context.Context, string, string, bool, string, int) ([]Object, bool, error)
	Stat(context.Context, string, string) (Object, error)
	Put(context.Context, string, string, io.Reader, int64, string) error
	Get(context.Context, string, string) (io.ReadCloser, Object, error)
	Delete(context.Context, string, string) error
	Copy(context.Context, string, string, string, string) error
	UploadSignature(context.Context, string, string, int64, string, time.Duration) (Signature, error)
	DownloadSignature(context.Context, string, string, time.Duration) (Signature, error)
}
type s3Plane struct {
	client, public *minio.Client
	region         string
	corsMode       string
}

func newS3() (DataPlane, error) {
	endpoint := os.Getenv("EULER_STORAGE_S3_ENDPOINT")
	if endpoint == "" {
		return nil, nil
	}
	secure := os.Getenv("EULER_STORAGE_S3_SECURE") != "false"
	corsMode := os.Getenv("EULER_STORAGE_S3_CORS_MODE")
	if corsMode == "" {
		corsMode = "bucket"
	}
	if corsMode != "bucket" && corsMode != "external" {
		return nil, errors.New("EULER_STORAGE_S3_CORS_MODE must be bucket or external")
	}
	key, secret := os.Getenv("EULER_STORAGE_S3_ACCESS_KEY"), os.Getenv("EULER_STORAGE_S3_SECRET_KEY")
	if key == "" || secret == "" {
		return nil, errors.New("S3 access key and secret are required when endpoint is configured")
	}
	opts := &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: secure, Region: os.Getenv("EULER_STORAGE_S3_REGION")}
	client, e := minio.New(endpoint, opts)
	if e != nil {
		return nil, errors.New("invalid EULER_STORAGE_S3_ENDPOINT")
	}
	public := client
	if external := os.Getenv("EULER_STORAGE_S3_PUBLIC_ENDPOINT"); external != "" {
		public, e = minio.New(external, opts)
		if e != nil {
			return nil, errors.New("invalid EULER_STORAGE_S3_PUBLIC_ENDPOINT")
		}
	}
	return &s3Plane{client: client, public: public, region: opts.Region, corsMode: corsMode}, nil
}
func (p *s3Plane) CORSMode() string { return p.corsMode }
func (p *s3Plane) CreateBucket(ctx context.Context, b string) error {
	return p.client.MakeBucket(ctx, b, minio.MakeBucketOptions{Region: p.region})
}
func (p *s3Plane) DeleteBucket(ctx context.Context, b string, force bool) error {
	if force {
		for obj := range p.client.ListObjects(ctx, b, minio.ListObjectsOptions{Recursive: true}) {
			if obj.Err != nil {
				return obj.Err
			}
			if e := p.client.RemoveObject(ctx, b, obj.Key, minio.RemoveObjectOptions{}); e != nil {
				return e
			}
		}
	}
	return p.client.RemoveBucket(ctx, b)
}
func (p *s3Plane) SetAccess(ctx context.Context, b, acl string) error {
	// Public access is enforced at Euler's public data entry. A permanent S3
	// anonymous policy would bypass installation disablement and ACL changes.
	// Temporary signed reads and size-limited POST policies remain direct to S3.
	return p.client.SetBucketPolicy(ctx, b, "")
}
func (p *s3Plane) SetCORS(ctx context.Context, b string, origins []string) error {
	if p.corsMode == "external" {
		return errors.New("CORS is managed by the data-plane operator")
	}
	return p.client.SetBucketCors(ctx, b, &cors.Config{CORSRules: []cors.Rule{{AllowedOrigin: origins, AllowedMethod: []string{"GET", "HEAD", "POST", "PUT"}, AllowedHeader: []string{"*"}, ExposeHeader: []string{"ETag", "Content-Length"}, MaxAgeSeconds: 3600}}})
}
func fromS3(v minio.ObjectInfo) Object {
	return Object{Key: v.Key, Size: v.Size, ContentType: v.ContentType, ETag: v.ETag, ModifiedAt: v.LastModified, Folder: strings.HasSuffix(v.Key, "/")}
}
func (p *s3Plane) List(ctx context.Context, b, prefix string, recursive bool, after string, limit int) ([]Object, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := []Object{}
	for obj := range p.client.ListObjects(ctx, b, minio.ListObjectsOptions{Prefix: prefix, Recursive: recursive, StartAfter: after, MaxKeys: limit}) {
		if obj.Err != nil {
			return nil, false, obj.Err
		}
		if len(out) >= limit {
			return out, true, nil
		}
		out = append(out, fromS3(obj))
	}
	return out, false, nil
}
func (p *s3Plane) Stat(ctx context.Context, b, k string) (Object, error) {
	v, e := p.client.StatObject(ctx, b, k, minio.StatObjectOptions{})
	return fromS3(v), e
}
func (p *s3Plane) Put(ctx context.Context, b, k string, r io.Reader, size int64, contentType string) error {
	_, e := p.client.PutObject(ctx, b, k, r, size, minio.PutObjectOptions{ContentType: contentType})
	return e
}
func (p *s3Plane) Get(ctx context.Context, b, k string) (io.ReadCloser, Object, error) {
	info, e := p.Stat(ctx, b, k)
	if e != nil {
		return nil, info, e
	}
	r, e := p.client.GetObject(ctx, b, k, minio.GetObjectOptions{})
	return r, info, e
}
func (p *s3Plane) Delete(ctx context.Context, b, k string) error {
	return p.client.RemoveObject(ctx, b, k, minio.RemoveObjectOptions{})
}
func (p *s3Plane) Copy(ctx context.Context, b, source, target, etag string) error {
	_, e := p.client.CopyObject(ctx, minio.CopyDestOptions{Bucket: b, Object: target}, minio.CopySrcOptions{Bucket: b, Object: source, MatchETag: etag})
	return e
}
func (p *s3Plane) UploadSignature(ctx context.Context, b, k string, size int64, contentType string, ttl time.Duration) (Signature, error) {
	policy := minio.NewPostPolicy()
	expiry := time.Now().UTC().Add(ttl)
	// The SDK omits a (0,0) range entirely. Bound empty-file policies to one
	// byte; publish still requires exactly zero, so they cannot become unbounded.
	for _, e := range []error{policy.SetBucket(b), policy.SetKey(k), policy.SetExpires(expiry), policy.SetContentLengthRange(size, max(size, 1))} {
		if e != nil {
			return Signature{}, e
		}
	}
	if contentType != "" {
		if e := policy.SetContentType(contentType); e != nil {
			return Signature{}, e
		}
	}
	u, fields, e := p.public.PresignedPostPolicy(ctx, policy)
	if e != nil {
		return Signature{}, e
	}
	return Signature{URL: u.String(), Method: "POST", Fields: fields, ExpiresAt: expiry.Format(time.RFC3339Nano)}, nil
}
func (p *s3Plane) DownloadSignature(ctx context.Context, b, k string, ttl time.Duration) (Signature, error) {
	q := url.Values{}
	q.Set("response-content-disposition", "attachment")
	u, e := p.public.PresignedGetObject(ctx, b, k, ttl, q)
	if e != nil {
		return Signature{}, e
	}
	return Signature{URL: u.String(), Method: "GET", ExpiresAt: time.Now().UTC().Add(ttl).Format(time.RFC3339Nano)}, nil
}
func absent(err error) bool {
	v := minio.ToErrorResponse(err)
	return v.Code == "NoSuchKey" || v.Code == "NoSuchBucket" || v.Code == "NotFound" || v.StatusCode == 404 || errors.Is(err, os.ErrNotExist)
}
