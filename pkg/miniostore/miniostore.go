// Package miniostore implements the tuscore Bucket interface with minio-go.
package miniostore

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
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

func (b *bucket) Name() string { return b.name }

func (b *bucket) NewMultipart(ctx context.Context, key, contentType string, metadata map[string]string) (string, error) {
	options := minio.PutObjectOptions{ContentType: contentType}
	if len(metadata) > 0 {
		options.UserMetadata = metadata
	}
	return b.core.NewMultipartUpload(ctx, b.name, key, options)
}

func (b *bucket) UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (string, error) {
	part, err := b.core.PutObjectPart(ctx, b.name, key, uploadID, int(number), body, size, minio.PutObjectPartOptions{})
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (b *bucket) UploadPartCopy(ctx context.Context, key, uploadID string, number int32, sourceKey string, sourceSize int64) (string, error) {
	part, err := b.core.CopyObjectPart(ctx, b.name, sourceKey, b.name, key, uploadID, int(number), 0, sourceSize, nil)
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (b *bucket) ListParts(ctx context.Context, key, uploadID string, marker int32) ([]tuscore.Part, bool, int32, error) {
	result, err := b.core.ListObjectParts(ctx, b.name, key, uploadID, int(marker), 1000)
	if err != nil {
		return nil, false, 0, err
	}
	parts := make([]tuscore.Part, 0, len(result.ObjectParts))
	for _, part := range result.ObjectParts {
		parts = append(parts, tuscore.Part{
			Number: int32(part.PartNumber),
			Size:   part.Size,
			ETag:   part.ETag,
		})
	}
	if !result.IsTruncated {
		return parts, false, 0, nil
	}
	return parts, true, int32(result.NextPartNumberMarker), nil
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

func (b *bucket) GetObject(ctx context.Context, key string, opts tuscore.GetOptions) (*tuscore.Object, error) {
	options := minio.GetObjectOptions{}
	if opts.IfMatch != "" {
		options.Set("If-Match", opts.IfMatch)
	}
	if opts.IfNoneMatch != "" {
		options.Set("If-None-Match", opts.IfNoneMatch)
	}
	if !opts.IfModifiedSince.IsZero() {
		options.Set("If-Modified-Since", opts.IfModifiedSince.UTC().Format(http.TimeFormat))
	}
	if !opts.IfUnmodifiedSince.IsZero() {
		options.Set("If-Unmodified-Since", opts.IfUnmodifiedSince.UTC().Format(http.TimeFormat))
	}

	// A ranged response reports the range length, so the full object size has
	// to come from a HEAD when the caller needs Content-Range.
	var head *minio.ObjectInfo
	if opts.Range != "" {
		info, err := b.client.StatObject(ctx, b.name, key, minio.StatObjectOptions{})
		if err != nil {
			return nil, err
		}
		head = &info
		start, end, ok := parseRangeHeader(opts.Range, info.Size)
		if !ok {
			return nil, fmt.Errorf("miniostore: unsupported Range header %q", opts.Range)
		}
		if err := options.SetRange(start, end); err != nil {
			return nil, err
		}
	}

	object, err := b.client.GetObject(ctx, b.name, key, options)
	if err != nil {
		return nil, err
	}
	info, err := object.Stat()
	if err != nil {
		_ = object.Close()
		return nil, err
	}
	result := &tuscore.Object{
		Body:         object,
		Size:         info.Size,
		ContentType:  info.ContentType,
		ETag:         info.ETag,
		LastModified: info.LastModified,
		AcceptRanges: "bytes",
	}
	if head != nil {
		result.Size = head.Size
		if result.ContentType == "" {
			result.ContentType = head.ContentType
		}
		if result.ETag == "" {
			result.ETag = head.ETag
		}
		if result.LastModified.IsZero() {
			result.LastModified = head.LastModified
		}
	}
	return result, nil
}

func (b *bucket) HeadObject(ctx context.Context, key string) (int64, error) {
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

func (b *bucket) StatusCode(err error) int {
	return minio.ToErrorResponse(err).StatusCode
}

// parseRangeHeader converts a single-range header into the inclusive bounds
// minio-go expects.
func parseRangeHeader(header string, size int64) (start, end int64, ok bool) {
	value := strings.TrimSpace(header)
	if !strings.HasPrefix(value, "bytes=") || size <= 0 {
		return 0, 0, false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	switch {
	case strings.TrimSpace(first) == "":
		suffix, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, false
		}
		if suffix > size {
			suffix = size
		}
		return size - suffix, size - 1, true
	case strings.TrimSpace(last) == "":
		parsed, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
		if err != nil || parsed < 0 || parsed >= size {
			return 0, 0, false
		}
		return parsed, size - 1, true
	default:
		parsedStart, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
		if err != nil || parsedStart < 0 || parsedStart >= size {
			return 0, 0, false
		}
		parsedEnd, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
		if err != nil || parsedEnd < parsedStart {
			return 0, 0, false
		}
		if parsedEnd >= size {
			parsedEnd = size - 1
		}
		return parsedStart, parsedEnd, true
	}
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
