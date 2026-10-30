// Package tuscore implements a tusd DataStore on top of a small object-store
// interface. Two providers (minio-go and aliyun-oss-go-sdk) implement Bucket,
// so the tus semantics live in exactly one place and do not depend on the
// upstream s3store or the AWS SDK.
//
// Layout (same as tusd, so existing records and the archive pipeline keep
// working):
//
//	<prefix><objectID>        content object (created on completion)
//	<prefix><objectID>.info   upload metadata (JSON)
//	<prefix><objectID>.part   incomplete tail (< MinPartSize)
package tuscore

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"github.com/tus/tusd/v2/pkg/handler"
)

// Defaults mirror the S3 multipart constraints; providers may reject smaller
// parts for non-final chunks.
const (
	DefaultMinPartSize   int64 = 5 << 20
	DefaultPreferredSize int64 = 50 << 20
	DefaultMaxParts            = 10000
	readBufferSize             = 1 << 20
)

// Part is one committed multipart part.
type Part struct {
	Number int32
	Size   int64
	ETag   string
}

// Bucket is the provider surface used by the store. Implementations must map
// their SDK errors onto IsNotFound / IsNoSuchUpload.
type Bucket interface {
	NewMultipart(ctx context.Context, key, contentType string) (uploadID string, err error)
	UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (etag string, err error)
	ListParts(ctx context.Context, key, uploadID string) ([]Part, error)
	CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error
	AbortMultipart(ctx context.Context, key, uploadID string) error

	PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error
	GetObject(ctx context.Context, key string) (io.ReadCloser, error)
	// StatObject returns the object size; not-found errors are reported through
	// IsNotFound.
	StatObject(ctx context.Context, key string) (int64, error)
	DeleteObject(ctx context.Context, key string) error

	IsNotFound(err error) bool
	IsNoSuchUpload(err error) bool
}

// Logger receives diagnostics; nil is fine.
type Logger interface {
	Debug(msg string, args ...any)
	Warn(msg string, args ...any)
}

// Store implements handler.DataStore and handler.TerminaterDataStore.
type Store struct {
	Bucket            Bucket
	Prefix            string
	MinPartSize       int64
	PreferredPartSize int64
	MaxParts          int
	Logger            Logger
	uploadLocks       map[string]*sync.Mutex
	uploadLocksMu     sync.Mutex
}

// New builds a store. prefix should end with "/" (normalised here).
func New(bucket Bucket, prefix string) *Store {
	prefix = strings.Trim(prefix, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &Store{
		Bucket:            bucket,
		Prefix:            prefix,
		MinPartSize:       DefaultMinPartSize,
		PreferredPartSize: DefaultPreferredSize,
		MaxParts:          DefaultMaxParts,
		uploadLocks:       make(map[string]*sync.Mutex),
	}
}

// UseIn registers the store on a tusd composer.
func (s *Store) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(s)
	composer.UseTerminater(s)
}

func (s *Store) key(objectID string) string     { return s.Prefix + objectID }
func (s *Store) infoKey(objectID string) string { return s.key(objectID) + ".info" }
func (s *Store) partKey(objectID string) string { return s.key(objectID) + ".part" }

func (s *Store) lockFor(objectID string) *sync.Mutex {
	s.uploadLocksMu.Lock()
	defer s.uploadLocksMu.Unlock()
	lock, ok := s.uploadLocks[objectID]
	if !ok {
		lock = &sync.Mutex{}
		s.uploadLocks[objectID] = lock
	}
	return lock
}

// NewUpload creates the multipart upload and the .info object.
func (s *Store) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	objectID := strings.TrimSpace(info.ID)
	if objectID == "" {
		generated, err := randomID()
		if err != nil {
			return nil, err
		}
		objectID = generated
	}
	key := s.key(objectID)
	contentType := info.MetaData["filetype"]
	uploadID, err := s.Bucket.NewMultipart(ctx, key, contentType)
	if err != nil {
		return nil, fmt.Errorf("tuscore: create multipart: %w", err)
	}

	info.ID = objectID + "+" + uploadID
	info.Offset = 0
	info.Storage = map[string]string{"Type": "tuscore", "Key": key}
	upload := &Upload{
		store:       s,
		objectID:    objectID,
		multipartID: uploadID,
		key:         key,
		mu:          s.lockFor(objectID),
		info:        &info,
	}
	if err := s.writeInfo(ctx, objectID, &info); err != nil {
		_ = s.Bucket.AbortMultipart(ctx, key, uploadID)
		return nil, fmt.Errorf("tuscore: write info: %w", err)
	}
	return upload, nil
}

// GetUpload loads the metadata written by NewUpload.
func (s *Store) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	objectID, multipartID, ok := splitID(id)
	if !ok {
		return nil, handler.ErrNotFound
	}
	reader, err := s.Bucket.GetObject(ctx, s.infoKey(objectID))
	if err != nil {
		if s.Bucket.IsNotFound(err) {
			return nil, handler.ErrNotFound
		}
		return nil, err
	}
	data, readErr := io.ReadAll(reader)
	_ = reader.Close()
	if readErr != nil {
		return nil, readErr
	}
	var info handler.FileInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("tuscore: decode info: %w", err)
	}
	if info.ID == "" {
		info.ID = id
	}
	return &Upload{
		store:       s,
		objectID:    objectID,
		multipartID: multipartID,
		key:         s.key(objectID),
		mu:          s.lockFor(objectID),
		info:        &info,
	}, nil
}

// AsTerminatableUpload implements handler.TerminaterDataStore.
func (s *Store) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	return upload.(*Upload)
}

func (s *Store) writeInfo(ctx context.Context, objectID string, info *handler.FileInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.Bucket.PutObject(ctx, s.infoKey(objectID), bytes.NewReader(data), int64(len(data)), "application/json")
}

// Upload is the per-upload handle.
type Upload struct {
	store       *Store
	objectID    string
	multipartID string
	key         string
	mu          *sync.Mutex
	info        *handler.FileInfo
}

// WriteChunk streams src into parts, buffering only the incomplete tail.
func (u *Upload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	info, parts, err := u.snapshot(ctx)
	if err != nil {
		return 0, err
	}
	if offset != info.Offset {
		return 0, fmt.Errorf("tuscore: invalid offset %d, expected %d", offset, info.Offset)
	}

	tailKey := u.store.partKey(u.objectID)
	buffer := make([]byte, 0, u.store.partSize(info.Size))
	if size, statErr := u.store.Bucket.StatObject(ctx, tailKey); statErr == nil && size > 0 {
		reader, err := u.store.Bucket.GetObject(ctx, tailKey)
		if err != nil {
			return 0, err
		}
		existing, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			return 0, readErr
		}
		buffer = append(buffer, existing...)
		// Remove the persisted tail from the offset immediately; a crash before
		// the next flush costs at most the tail bytes, never a double count.
		_ = u.store.Bucket.DeleteObject(ctx, tailKey)
	} else if statErr != nil && !u.store.Bucket.IsNotFound(statErr) {
		return 0, statErr
	}

	partSize := u.store.partSize(info.Size)
	var written int64
	readBuf := make([]byte, readBufferSize)
	for {
		n, readErr := src.Read(readBuf)
		if n > 0 {
			buffer = append(buffer, readBuf[:n]...)
			written += int64(n)
			for int64(len(buffer)) >= partSize {
				if err := u.uploadPart(ctx, &parts, buffer[:partSize]); err != nil {
					return written, err
				}
				buffer = append(buffer[:0], buffer[partSize:]...)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return written, readErr
		}
	}

	if len(buffer) > 0 {
		if err := u.store.Bucket.PutObject(ctx, tailKey, bytes.NewReader(buffer), int64(len(buffer)), ""); err != nil {
			return written, err
		}
	}
	return written, nil
}

// GetInfo reports the committed bytes plus the persisted tail.
func (u *Upload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	info, _, err := u.snapshot(ctx)
	return info, err
}

// GetReader opens the completed object.
func (u *Upload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return u.store.Bucket.GetObject(ctx, u.key)
}

// FinishUpload promotes the tail to the last part and completes the upload.
func (u *Upload) FinishUpload(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	info, parts, err := u.snapshot(ctx)
	if err != nil {
		return err
	}
	if parts == nil {
		// Already completed: the multipart session is gone but the object is
		// still there (FinishUpload is allowed to be retried).
		if size, statErr := u.store.Bucket.StatObject(ctx, u.key); statErr == nil && size == info.Size {
			return u.finalizeInfo(ctx, info)
		}
	}

	tailKey := u.store.partKey(u.objectID)
	if size, statErr := u.store.Bucket.StatObject(ctx, tailKey); statErr == nil && size > 0 {
		reader, err := u.store.Bucket.GetObject(ctx, tailKey)
		if err != nil {
			return err
		}
		tail, readErr := io.ReadAll(reader)
		_ = reader.Close()
		if readErr != nil {
			return readErr
		}
		if len(tail) > 0 {
			if err := u.uploadPart(ctx, &parts, tail); err != nil {
				return err
			}
		}
	} else if statErr != nil && !u.store.Bucket.IsNotFound(statErr) {
		return statErr
	}

	if len(parts) == 0 {
		// Zero-byte upload: a multipart completion without parts is invalid.
		_ = u.store.Bucket.AbortMultipart(ctx, u.key, u.multipartID)
		if err := u.store.Bucket.PutObject(ctx, u.key, bytes.NewReader(nil), 0, info.MetaData["filetype"]); err != nil {
			return err
		}
	} else if err := u.store.Bucket.CompleteMultipart(ctx, u.key, u.multipartID, parts); err != nil {
		return err
	}
	_ = u.store.Bucket.DeleteObject(ctx, tailKey)
	return u.finalizeInfo(ctx, info)
}

// Terminate aborts the multipart upload and removes all companion objects.
func (u *Upload) Terminate(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if err := u.store.Bucket.AbortMultipart(ctx, u.key, u.multipartID); err != nil &&
		!u.store.Bucket.IsNotFound(err) && !u.store.Bucket.IsNoSuchUpload(err) {
		return err
	}
	for _, key := range []string{u.store.partKey(u.objectID), u.store.infoKey(u.objectID), u.key} {
		if err := u.store.Bucket.DeleteObject(ctx, key); err != nil && !u.store.Bucket.IsNotFound(err) {
			return err
		}
	}
	return nil
}

func (u *Upload) uploadPart(ctx context.Context, parts *[]Part, data []byte) error {
	number := int32(len(*parts) + 1)
	etag, err := u.store.Bucket.UploadPart(ctx, u.key, u.multipartID, number, bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return fmt.Errorf("tuscore: upload part %d: %w", number, err)
	}
	*parts = append(*parts, Part{Number: number, Size: int64(len(data)), ETag: etag})
	return nil
}

// snapshot returns the metadata with an offset derived from the object store.
// A nil parts slice means the multipart session is gone (already completed).
func (u *Upload) snapshot(ctx context.Context) (handler.FileInfo, []Part, error) {
	info := *u.info
	parts, err := u.store.Bucket.ListParts(ctx, u.key, u.multipartID)
	if err != nil {
		if u.store.Bucket.IsNoSuchUpload(err) {
			size, statErr := u.store.Bucket.StatObject(ctx, u.key)
			if statErr == nil {
				info.Offset = size
				return info, nil, nil
			}
			if u.store.Bucket.IsNotFound(statErr) {
				return info, nil, nil
			}
			return info, nil, statErr
		}
		return info, nil, fmt.Errorf("tuscore: list parts: %w", err)
	}

	var offset int64
	for _, part := range parts {
		offset += part.Size
	}
	if size, statErr := u.store.Bucket.StatObject(ctx, u.store.partKey(u.objectID)); statErr == nil {
		offset += size
	} else if !u.store.Bucket.IsNotFound(statErr) {
		return info, parts, statErr
	}
	info.Offset = offset
	return info, parts, nil
}

func (u *Upload) finalizeInfo(ctx context.Context, info handler.FileInfo) error {
	info.Offset = info.Size
	u.info = &info
	if err := u.store.writeInfo(ctx, u.objectID, u.info); err != nil {
		return err
	}
	return nil
}

func (s *Store) partSize(size int64) int64 {
	minimum := s.MinPartSize
	if minimum <= 0 {
		minimum = DefaultMinPartSize
	}
	preferred := s.PreferredPartSize
	if preferred < minimum {
		preferred = minimum
	}
	if size > 0 && s.MaxParts > 0 {
		needed := (size + int64(s.MaxParts) - 1) / int64(s.MaxParts)
		if needed > preferred {
			preferred = needed
		}
	}
	return preferred
}

func splitID(id string) (objectID, multipartID string, ok bool) {
	index := strings.LastIndex(id, "+")
	if index <= 0 || index == len(id)-1 {
		return "", "", false
	}
	return id[:index], id[index+1:], true
}

func randomID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw), nil
}

// ErrInvalidOffset is returned when the caller skips or rewinds.
var ErrInvalidOffset = errors.New("tuscore: invalid offset")
