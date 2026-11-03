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
//
// Two operational properties matter for long-running resumable uploads:
//
//   - the offset, the committed parts and the persisted tail are fetched once
//     per upload handle and then cached, so one PATCH performs a single
//     ListParts instead of re-listing for every snapshot;
//   - chunk data is staged in a temporary file instead of a preallocated
//     part-size (50 MiB) heap buffer, so memory usage follows the bytes that
//     are actually in flight rather than the configured part size.
//
// The offset is still derived from the object store (parts plus the persisted
// tail), never from the .info object alone.
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
	"os"
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
	maxInfoObjectSize          = 1 << 20
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
	// TempDir stages chunk data on disk. Empty uses os.TempDir().
	TempDir string
	Logger  Logger

	uploadLocks   map[string]*sync.Mutex
	uploadLocksMu sync.Mutex
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

func (s *Store) tempDir() string {
	if dir := strings.TrimSpace(s.TempDir); dir != "" {
		return dir
	}
	return os.TempDir()
}

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
	if err := s.writeInfo(ctx, objectID, info); err != nil {
		_ = s.Bucket.AbortMultipart(ctx, key, uploadID)
		return nil, fmt.Errorf("tuscore: write info: %w", err)
	}
	return &Upload{
		store:       s,
		objectID:    objectID,
		multipartID: uploadID,
		key:         key,
		mu:          s.lockFor(objectID),
		info:        info,
		loaded:      true,
	}, nil
}

// GetUpload returns a lazy handle. The first GetInfo/WriteChunk/FinishUpload
// call fetches the .info object, the parts and the tail size once.
func (s *Store) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	objectID, multipartID, ok := splitID(id)
	if !ok {
		return nil, handler.ErrNotFound
	}
	return &Upload{
		store:       s,
		objectID:    objectID,
		multipartID: multipartID,
		key:         s.key(objectID),
		mu:          s.lockFor(objectID),
	}, nil
}

// AsTerminatableUpload implements handler.TerminaterDataStore.
func (s *Store) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	return upload.(*Upload)
}

func (s *Store) writeInfo(ctx context.Context, objectID string, info handler.FileInfo) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return s.Bucket.PutObject(ctx, s.infoKey(objectID), bytes.NewReader(data), int64(len(data)), "application/json")
}

// Upload is the per-upload handle. Everything below key is the per-request
// cache; a new request always starts from GetUpload and reloads from storage.
type Upload struct {
	store       *Store
	objectID    string
	multipartID string
	key         string
	mu          *sync.Mutex

	info      handler.FileInfo
	parts     []Part
	tailSize  int64
	loaded    bool
	completed bool
}

// WriteChunk streams src into parts, staging bytes on disk. Full parts are
// uploaded as they fill up; the remainder is persisted as the .part object.
func (u *Upload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	u.mu.Lock()
	defer u.mu.Unlock()

	info, err := u.load(ctx)
	if err != nil {
		return 0, err
	}
	if u.completed {
		return 0, fmt.Errorf("tuscore: upload %s is already completed", u.objectID)
	}
	if offset != info.Offset {
		return 0, fmt.Errorf("tuscore: invalid offset %d, expected %d", offset, info.Offset)
	}

	staging, err := os.CreateTemp(u.store.tempDir(), "tus-chunk-")
	if err != nil {
		return 0, err
	}
	defer func() {
		_ = staging.Close()
		_ = os.Remove(staging.Name())
	}()

	tailKey := u.store.partKey(u.objectID)
	var tailSize int64
	if u.tailSize > 0 {
		reader, err := u.store.Bucket.GetObject(ctx, tailKey)
		if err != nil {
			if !u.store.Bucket.IsNotFound(err) {
				return 0, err
			}
		} else {
			n, copyErr := io.Copy(staging, reader)
			_ = reader.Close()
			if copyErr != nil {
				return 0, copyErr
			}
			tailSize = n
			// The persisted tail is now local; removing it first keeps the
			// offset from double counting if this request dies mid-write.
			if err := u.store.Bucket.DeleteObject(ctx, tailKey); err != nil && !u.store.Bucket.IsNotFound(err) {
				return 0, err
			}
			u.tailSize = 0
		}
	}

	written, err := u.streamChunks(ctx, staging, &tailSize, src)
	if err != nil {
		// Parts may have been committed before the failure; reload next time.
		u.loaded = false
		return written, err
	}

	if tailSize > 0 {
		if err := u.store.Bucket.PutObject(ctx, tailKey, io.NewSectionReader(staging, 0, tailSize), tailSize, ""); err != nil {
			u.loaded = false
			return written, err
		}
	}
	u.tailSize = tailSize
	u.info.Offset += written
	return written, nil
}

// streamChunks appends src to the staging file and commits every full part.
func (u *Upload) streamChunks(ctx context.Context, staging *os.File, tailSize *int64, src io.Reader) (int64, error) {
	partSize := u.store.partSize(u.info.Size)
	buffer := make([]byte, readBufferSize)
	var written int64
	for {
		n, readErr := src.Read(buffer)
		if n > 0 {
			if _, err := staging.WriteAt(buffer[:n], *tailSize); err != nil {
				return written, err
			}
			*tailSize += int64(n)
			written += int64(n)
			for *tailSize >= partSize {
				if err := u.commitPart(ctx, staging, partSize, tailSize); err != nil {
					return written, err
				}
			}
		}
		if readErr == io.EOF {
			return written, nil
		}
		if readErr != nil {
			return written, readErr
		}
	}
}

// commitPart uploads the leading partSize bytes of the staging file and shifts
// whatever is left to the front. The remainder is bounded by readBufferSize,
// so the shift never copies a full part.
func (u *Upload) commitPart(ctx context.Context, staging *os.File, partSize int64, tailSize *int64) error {
	number := int32(len(u.parts) + 1)
	body := io.NewSectionReader(staging, 0, partSize)
	etag, err := u.store.Bucket.UploadPart(ctx, u.key, u.multipartID, number, body, partSize)
	if err != nil {
		return fmt.Errorf("tuscore: upload part %d: %w", number, err)
	}
	u.parts = append(u.parts, Part{Number: number, Size: partSize, ETag: etag})

	remainder := *tailSize - partSize
	if remainder > 0 {
		source := io.NewSectionReader(staging, partSize, remainder)
		if _, err := io.Copy(io.NewOffsetWriter(staging, 0), source); err != nil {
			return err
		}
	}
	*tailSize = remainder
	return nil
}

// GetInfo reports the committed bytes plus the persisted tail.
func (u *Upload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.load(ctx)
}

// GetReader opens the completed object.
func (u *Upload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	return u.store.Bucket.GetObject(ctx, u.key)
}

// FinishUpload promotes the tail to the last part and completes the upload.
func (u *Upload) FinishUpload(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	info, err := u.load(ctx)
	if err != nil {
		return err
	}
	tailKey := u.store.partKey(u.objectID)
	if u.completed {
		return u.finalizeInfo(ctx, info)
	}

	if u.tailSize > 0 {
		staging, err := os.CreateTemp(u.store.tempDir(), "tus-finish-")
		if err != nil {
			return err
		}
		defer func() {
			_ = staging.Close()
			_ = os.Remove(staging.Name())
		}()
		reader, err := u.store.Bucket.GetObject(ctx, tailKey)
		if err != nil {
			if !u.store.Bucket.IsNotFound(err) {
				return err
			}
		} else {
			n, copyErr := io.Copy(staging, reader)
			_ = reader.Close()
			if copyErr != nil {
				return copyErr
			}
			if n > 0 {
				number := int32(len(u.parts) + 1)
				etag, err := u.store.Bucket.UploadPart(ctx, u.key, u.multipartID, number, io.NewSectionReader(staging, 0, n), n)
				if err != nil {
					return fmt.Errorf("tuscore: upload final part %d: %w", number, err)
				}
				u.parts = append(u.parts, Part{Number: number, Size: n, ETag: etag})
			}
			u.tailSize = 0
		}
	}

	if len(u.parts) == 0 {
		// Zero-byte upload: a multipart completion without parts is invalid.
		_ = u.store.Bucket.AbortMultipart(ctx, u.key, u.multipartID)
		if err := u.store.Bucket.PutObject(ctx, u.key, bytes.NewReader(nil), 0, info.MetaData["filetype"]); err != nil {
			return err
		}
	} else if err := u.store.Bucket.CompleteMultipart(ctx, u.key, u.multipartID, u.parts); err != nil {
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

// load fetches .info, the parts and the tail size once per handle.
func (u *Upload) load(ctx context.Context) (handler.FileInfo, error) {
	if u.loaded {
		return u.info, nil
	}

	var (
		wg       sync.WaitGroup
		info     handler.FileInfo
		parts    []Part
		tailSize int64
		infoErr  error
		partsErr error
		tailErr  error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		info, infoErr = u.readInfo(ctx)
	}()
	go func() {
		defer wg.Done()
		parts, partsErr = u.store.Bucket.ListParts(ctx, u.key, u.multipartID)
	}()
	go func() {
		defer wg.Done()
		tailSize, tailErr = u.store.Bucket.StatObject(ctx, u.store.partKey(u.objectID))
	}()
	wg.Wait()

	if infoErr != nil {
		return handler.FileInfo{}, infoErr
	}
	if partsErr != nil {
		// The multipart session is gone: the upload either completed already
		// or was aborted elsewhere. Fall back to the stored object length.
		if u.store.Bucket.IsNoSuchUpload(partsErr) || u.store.Bucket.IsNotFound(partsErr) {
			size, statErr := u.store.Bucket.StatObject(ctx, u.key)
			if statErr != nil {
				if u.store.Bucket.IsNotFound(statErr) {
					return handler.FileInfo{}, handler.ErrNotFound
				}
				return handler.FileInfo{}, statErr
			}
			info.Offset = size
			u.info, u.parts, u.tailSize = info, nil, 0
			u.loaded, u.completed = true, true
			return u.info, nil
		}
		return handler.FileInfo{}, partsErr
	}
	if tailErr != nil {
		if !u.store.Bucket.IsNotFound(tailErr) {
			return handler.FileInfo{}, tailErr
		}
		tailSize = 0
	}

	var offset int64
	for _, part := range parts {
		offset += part.Size
	}
	info.Offset = offset + tailSize
	u.info, u.parts, u.tailSize = info, parts, tailSize
	u.loaded = true
	return u.info, nil
}

func (u *Upload) readInfo(ctx context.Context) (handler.FileInfo, error) {
	var info handler.FileInfo
	reader, err := u.store.Bucket.GetObject(ctx, u.store.infoKey(u.objectID))
	if err != nil {
		if u.store.Bucket.IsNotFound(err) {
			return info, handler.ErrNotFound
		}
		return info, err
	}
	defer reader.Close()
	if err := json.NewDecoder(io.LimitReader(reader, maxInfoObjectSize)).Decode(&info); err != nil {
		return info, fmt.Errorf("tuscore: decode info: %w", err)
	}
	if info.ID == "" {
		info.ID = u.objectID + "+" + u.multipartID
	}
	return info, nil
}

func (u *Upload) finalizeInfo(ctx context.Context, info handler.FileInfo) error {
	info.Offset = info.Size
	u.info = info
	u.parts = nil
	u.tailSize = 0
	u.completed = true
	return u.store.writeInfo(ctx, u.objectID, info)
}

// partSize picks the multipart part size for one upload.
func (s *Store) partSize(size int64) int64 {
	minimum := s.MinPartSize
	if minimum <= 0 {
		minimum = DefaultMinPartSize
	}
	preferred := s.PreferredPartSize
	if preferred < minimum {
		preferred = minimum
	}
	maxParts := s.MaxParts
	if maxParts <= 0 {
		maxParts = DefaultMaxParts
	}
	if size > 0 {
		if needed := (size + int64(maxParts) - 1) / int64(maxParts); needed > preferred {
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
