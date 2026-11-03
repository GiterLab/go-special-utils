// Package ossstore implements the tuscore Bucket interface with the Aliyun
// OSS SDK.
package ossstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

func (b *bucket) Name() string { return b.name }

func (b *bucket) result(key, uploadID string) oss.InitiateMultipartUploadResult {
	return oss.InitiateMultipartUploadResult{Bucket: b.name, Key: key, UploadID: uploadID}
}

func (b *bucket) NewMultipart(ctx context.Context, key, contentType string, metadata map[string]string) (string, error) {
	_ = ctx
	options := []oss.Option{}
	if contentType != "" {
		options = append(options, oss.ContentType(contentType))
	}
	for name, value := range metadata {
		options = append(options, oss.Meta(name, value))
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

func (b *bucket) UploadPartCopy(ctx context.Context, key, uploadID string, number int32, sourceKey string, sourceSize int64) (string, error) {
	_ = ctx
	part, err := b.bucket.UploadPartCopy(b.result(key, uploadID), b.name, sourceKey, 0, sourceSize, int(number))
	if err != nil {
		return "", err
	}
	return part.ETag, nil
}

func (b *bucket) ListParts(ctx context.Context, key, uploadID string, marker int32) ([]tuscore.Part, bool, int32, error) {
	_ = ctx
	options := []oss.Option{oss.MaxParts(1000)}
	if marker > 0 {
		options = append(options, oss.PartNumberMarker(int(marker)))
	}
	result, err := b.bucket.ListUploadedParts(b.result(key, uploadID), options...)
	if err != nil {
		return nil, false, 0, err
	}
	parts := make([]tuscore.Part, 0, len(result.UploadedParts))
	for _, part := range result.UploadedParts {
		parts = append(parts, tuscore.Part{
			Number: int32(part.PartNumber),
			Size:   int64(part.Size),
			ETag:   part.ETag,
		})
	}
	if !result.IsTruncated {
		return parts, false, 0, nil
	}
	next, _ := strconv.Atoi(result.NextPartNumberMarker)
	return parts, true, int32(next), nil
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

func (b *bucket) GetObject(ctx context.Context, key string, opts tuscore.GetOptions) (*tuscore.Object, error) {
	_ = ctx
	conditions := make([]oss.Option, 0, 4)
	if opts.IfMatch != "" {
		conditions = append(conditions, oss.IfMatch(opts.IfMatch))
	}
	if opts.IfNoneMatch != "" {
		conditions = append(conditions, oss.IfNoneMatch(opts.IfNoneMatch))
	}
	if !opts.IfModifiedSince.IsZero() {
		conditions = append(conditions, oss.IfModifiedSince(opts.IfModifiedSince))
	}
	if !opts.IfUnmodifiedSince.IsZero() {
		conditions = append(conditions, oss.IfUnmodifiedSince(opts.IfUnmodifiedSince))
	}

	// The SDK reports 3xx responses (including 304) as plain errors without the
	// status code, so conditional requests are evaluated from a HEAD first and
	// the GET itself is sent without the conditional headers.
	if len(conditions) > 0 {
		headers, err := b.bucket.GetObjectMeta(key)
		if err != nil {
			return nil, err
		}
		if status := evaluateConditions(opts, headers); status != 0 {
			return nil, &tuscore.StatusError{Status: status}
		}
	}

	options := []oss.Option{}
	if opts.Range != "" {
		options = append(options, oss.NormalizedRange(strings.TrimPrefix(opts.Range, "bytes=")))
	}

	// DoGetObject exposes the response headers, unlike GetObject, so the ETag,
	// content type and full size come back without an extra HEAD request.
	result, err := b.bucket.DoGetObject(&oss.GetObjectRequest{ObjectKey: key}, options)
	if err != nil {
		return nil, err
	}
	headers := result.Response.Headers
	size, _ := strconv.ParseInt(headers.Get("Content-Length"), 10, 64)
	contentRange := headers.Get("Content-Range")
	if index := strings.LastIndex(contentRange, "/"); index >= 0 {
		if total, parseErr := strconv.ParseInt(strings.TrimSpace(contentRange[index+1:]), 10, 64); parseErr == nil {
			size = total
		}
	}
	return &tuscore.Object{
		Body:         result.Response,
		Size:         size,
		ContentType:  headers.Get("Content-Type"),
		ETag:         headers.Get("ETag"),
		LastModified: parseHTTPTime(headers.Get("Last-Modified")),
		AcceptRanges: "bytes",
		ContentRange: contentRange,
	}, nil
}

func (b *bucket) HeadObject(ctx context.Context, key string) (int64, error) {
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

func (b *bucket) StatusCode(err error) int {
	var statusError *tuscore.StatusError
	if errors.As(err, &statusError) {
		return statusError.Status
	}
	var serviceError oss.ServiceError
	if errors.As(err, &serviceError) {
		return serviceError.StatusCode
	}
	return 0
}

// evaluateConditions returns the HTTP status a conditional request should be
// answered with, or 0 when the request may proceed.
func evaluateConditions(opts tuscore.GetOptions, headers http.Header) int {
	etag := strings.Trim(headers.Get("ETag"), `"`)
	if opts.IfMatch != "" && !etagMatches(opts.IfMatch, etag) {
		return http.StatusPreconditionFailed
	}
	if opts.IfNoneMatch != "" && etagMatches(opts.IfNoneMatch, etag) {
		return http.StatusNotModified
	}
	lastModified := parseHTTPTime(headers.Get("Last-Modified"))
	if !opts.IfUnmodifiedSince.IsZero() && !lastModified.IsZero() && lastModified.After(opts.IfUnmodifiedSince) {
		return http.StatusPreconditionFailed
	}
	if !opts.IfModifiedSince.IsZero() && !lastModified.IsZero() && !lastModified.After(opts.IfModifiedSince) {
		return http.StatusNotModified
	}
	return 0
}

func etagMatches(header, etag string) bool {
	if etag == "" {
		return false
	}
	if strings.TrimSpace(header) == "*" {
		return true
	}
	for _, candidate := range strings.Split(header, ",") {
		if strings.Trim(strings.TrimSpace(candidate), `"`) == etag {
			return true
		}
	}
	return false
}

func serviceCode(err error) string {
	var serviceError oss.ServiceError
	if errors.As(err, &serviceError) {
		return serviceError.Code
	}
	return ""
}

func parseHTTPTime(value string) time.Time {
	if value == "" {
		return time.Time{}
	}
	parsed, err := http.ParseTime(value)
	if err != nil {
		return time.Time{}
	}
	return parsed
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
