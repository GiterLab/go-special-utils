package tuscore

import (
	"context"
	"fmt"
	"io"
	"time"
)

// StatusError reports a provider HTTP status without a response body. Some
// SDKs return plain errors for 3xx responses, so providers use this type to
// keep the status observable by the store.
type StatusError struct{ Status int }

func (e *StatusError) Error() string {
	return fmt.Sprintf("tuscore: provider returned status %d", e.Status)
}

// Part is one committed multipart part.
type Part struct {
	Number int32
	Size   int64
	ETag   string
}

// GetOptions carries the HTTP semantics a provider must forward when an object
// is streamed back to a client.
type GetOptions struct {
	Range             string
	IfMatch           string
	IfNoneMatch       string
	IfModifiedSince   time.Time
	IfUnmodifiedSince time.Time
}

// Object is the response of a GetObject call. Only the fields the provider can
// supply are populated.
type Object struct {
	Body               io.ReadCloser
	Size               int64
	ContentType        string
	ContentDisposition string
	ContentEncoding    string
	ContentLanguage    string
	CacheControl       string
	Expires            string
	ETag               string
	LastModified       time.Time
	AcceptRanges       string
	ContentRange       string
}

// Bucket is the provider surface used by the store. It mirrors the subset of
// the S3 API the tus semantics need, so minio-go and the Aliyun OSS SDK can
// implement it without leaking their own types.
type Bucket interface {
	// Name reports the bucket name used in the upload's Storage metadata.
	Name() string

	NewMultipart(ctx context.Context, key, contentType string, metadata map[string]string) (uploadID string, err error)
	UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (etag string, err error)
	UploadPartCopy(ctx context.Context, key, uploadID string, number int32, sourceKey string, sourceSize int64) (etag string, err error)
	// ListParts returns one page. marker is 0 for the first page.
	ListParts(ctx context.Context, key, uploadID string, marker int32) (parts []Part, truncated bool, nextMarker int32, err error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error

	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string, opts GetOptions) (*Object, error)
	HeadObject(ctx context.Context, key string) (size int64, err error)
	DeleteObject(ctx context.Context, key string) error

	IsNotFound(err error) bool
	IsNoSuchUpload(err error) bool
	// StatusCode reports the HTTP status carried by a provider error, or 0.
	StatusCode(err error) int
}

// Logger receives diagnostics; nil is fine.
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}
