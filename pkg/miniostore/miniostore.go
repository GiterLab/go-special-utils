// Package miniostore implements the tuscore Bucket interface with minio-go.
package miniostore

import (
	"context"
	"io"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/GiterLab/go-special-utils/pkg/tuscore"
)

// Config describes the MinIO target.
type Config struct {
	Endpoint  string
	Region    string
	AccessKey string
	SecretKey string
	Bucket    string
	Prefix    string
	Secure    bool
}

// New returns a tusd DataStore backed by MinIO.
func New(cfg Config) (*tuscore.Store, error) {
	client, err := minio.New(stripScheme(cfg.Endpoint), &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.Secure,
		Region: cfg.Region,
	})
	if err != nil {
		return nil, err
	}
	return tuscore.New(&bucket{
		client: client,
		core:   minio.Core{Client: client},
		name:   cfg.Bucket,
	}, cfg.Prefix), nil
}

type bucket struct {
	client *minio.Client
	core   minio.Core
	name   string
}

func (b *bucket) NewMultipart(ctx context.Context, key, contentType string) (string, error) {
	return b.core.NewMultipartUpload(ctx, b.name, key, minio.PutObjectOptions{ContentType: contentType})
}

func (b *bucket) UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (string, error) {
	part, err := b.core.PutObjectPart(ctx, b.name, key, uploadID, int(number), body, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (b *bucket) ListParts(ctx context.Context, key, uploadID string) ([]tuscore.Part, error) {
	var parts []tuscore.Part
	marker := 0
	for {
		result, err := b.core.ListObjectParts(ctx, b.name, key, uploadID, marker, 1000)
		if err != nil {
			return nil, err
		}
		for _, part := range result.ObjectParts {
			parts = append(parts, tuscore.Part{
				Number: int32(part.PartNumber),
				Size:   part.Size,
				ETag:   part.ETag,
			})
		}
		if !result.IsTruncated {
			return parts, nil
		}
		marker = result.NextPartNumberMarker
	}
}

func (b *bucket) CompleteMultipart(ctx context.Context, key, uploadID string, parts []tuscore.Part) error {
	complete := make([]minio.CompletePart, 0, len(parts))
	for _, part := range parts {
		complete = append(complete, minio.CompletePart{PartNumber: int(part.Number), ETag: part.ETag})
	}
	_, err := b.core.CompleteMultipartUpload(ctx, b.name, key, uploadID, complete, minio.PutObjectOptions{})
	return err
}

func (b *bucket) AbortMultipart(ctx context.Context, key, uploadID string) error {
	return b.core.AbortMultipartUpload(ctx, b.name, key, uploadID)
}

func (b *bucket) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	options := minio.PutObjectOptions{}
	if contentType != "" {
		options.ContentType = contentType
	}
	_, err := b.client.PutObject(ctx, b.name, key, body, size, options)
	return err
}

func (b *bucket) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	object, err := b.client.GetObject(ctx, b.name, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	if _, err := object.Stat(); err != nil {
		_ = object.Close()
		return nil, err
	}
	return object, nil
}

func (b *bucket) StatObject(ctx context.Context, key string) (int64, error) {
	info, err := b.client.StatObject(ctx, b.name, key, minio.StatObjectOptions{})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (b *bucket) DeleteObject(ctx context.Context, key string) error {
	return b.client.RemoveObject(ctx, b.name, key, minio.RemoveObjectOptions{})
}

func (b *bucket) IsNotFound(err error) bool {
	switch minio.ToErrorResponse(err).Code {
	case "NoSuchKey", "NoSuchObject", "NotFound", "NoSuchUpload":
		return true
	default:
		return false
	}
}

func (b *bucket) IsNoSuchUpload(err error) bool {
	return minio.ToErrorResponse(err).Code == "NoSuchUpload"
}

// stripScheme keeps minio-go happy when the endpoint comes from an ini value
// that includes the protocol.
func stripScheme(endpoint string) string {
	value := strings.TrimSpace(endpoint)
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "http://") {
		return value[len("http://"):]
	}
	if strings.HasPrefix(lower, "https://") {
		return value[len("https://"):]
	}
	return value
}
