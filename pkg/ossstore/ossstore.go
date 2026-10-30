// Package ossstore implements the tuscore Bucket interface with the Aliyun
// OSS SDK.
package ossstore

import (
	"context"
	"io"
	"strconv"
	"strings"

	"github.com/aliyun/aliyun-oss-go-sdk/oss"

	"github.com/GiterLab/go-special-utils/pkg/tuscore"
)

// Config describes the OSS target.
type Config struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	Prefix    string
	Secure    bool
}

// New returns a tusd DataStore backed by Aliyun OSS.
func New(cfg Config) (*tuscore.Store, error) {
	client, err := oss.New(withScheme(cfg.Endpoint, cfg.Secure), cfg.AccessKey, cfg.SecretKey)
	if err != nil {
		return nil, err
	}
	target, err := client.Bucket(cfg.Bucket)
	if err != nil {
		return nil, err
	}
	return tuscore.New(&bucket{name: cfg.Bucket, bucket: target}, cfg.Prefix), nil
}

type bucket struct {
	name   string
	bucket *oss.Bucket
}

func (b *bucket) result(key, uploadID string) oss.InitiateMultipartUploadResult {
	return oss.InitiateMultipartUploadResult{Bucket: b.name, Key: key, UploadID: uploadID}
}

func (b *bucket) NewMultipart(ctx context.Context, key, contentType string) (string, error) {
	_ = ctx
	options := []oss.Option{}
	if contentType != "" {
		options = append(options, oss.ContentType(contentType))
	}
	result, err := b.bucket.InitiateMultipartUpload(key, options...)
	if err != nil {
		return "", err
	}
	return result.UploadID, nil
}

func (b *bucket) UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (string, error) {
	_ = ctx
	part, err := b.bucket.UploadPart(b.result(key, uploadID), body, size, int(number))
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (b *bucket) ListParts(ctx context.Context, key, uploadID string) ([]tuscore.Part, error) {
	_ = ctx
	var parts []tuscore.Part
	marker := 0
	for {
		options := []oss.Option{oss.MaxParts(1000)}
		if marker > 0 {
			options = append(options, oss.PartNumberMarker(marker))
		}
		result, err := b.bucket.ListUploadedParts(b.result(key, uploadID), options...)
		if err != nil {
			return nil, err
		}
		for _, part := range result.UploadedParts {
			parts = append(parts, tuscore.Part{
				Number: int32(part.PartNumber),
				Size:   int64(part.Size),
				ETag:   part.ETag,
			})
		}
		if !result.IsTruncated {
			return parts, nil
		}
		marker, _ = strconv.Atoi(result.NextPartNumberMarker)
	}
}

func (b *bucket) CompleteMultipart(ctx context.Context, key, uploadID string, parts []tuscore.Part) error {
	_ = ctx
	uploadParts := make([]oss.UploadPart, 0, len(parts))
	for _, part := range parts {
		uploadParts = append(uploadParts, oss.UploadPart{PartNumber: int(part.Number), ETag: part.ETag})
	}
	_, err := b.bucket.CompleteMultipartUpload(b.result(key, uploadID), uploadParts)
	return err
}

func (b *bucket) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_ = ctx
	return b.bucket.AbortMultipartUpload(b.result(key, uploadID))
}

func (b *bucket) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_ = ctx
	options := []oss.Option{oss.ContentLength(size)}
	if contentType != "" {
		options = append(options, oss.ContentType(contentType))
	}
	return b.bucket.PutObject(key, body, options...)
}

func (b *bucket) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	_ = ctx
	return b.bucket.GetObject(key)
}

func (b *bucket) StatObject(ctx context.Context, key string) (int64, error) {
	_ = ctx
	meta, err := b.bucket.GetObjectMeta(key)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(meta.Get("Content-Length"), 10, 64)
}

func (b *bucket) DeleteObject(ctx context.Context, key string) error {
	_ = ctx
	return b.bucket.DeleteObject(key)
}

func (b *bucket) IsNotFound(err error) bool {
	switch serviceCode(err) {
	case "NoSuchKey", "NoSuchObject", "NotFound", "NoSuchUpload":
		return true
	default:
		return false
	}
}

func (b *bucket) IsNoSuchUpload(err error) bool {
	return serviceCode(err) == "NoSuchUpload"
}

func serviceCode(err error) string {
	if serviceError, ok := err.(oss.ServiceError); ok {
		return serviceError.Code
	}
	return ""
}

func withScheme(endpoint string, secure bool) string {
	lower := strings.ToLower(endpoint)
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return endpoint
	}
	if secure {
		return "https://" + endpoint
	}
	return "http://" + endpoint
}
