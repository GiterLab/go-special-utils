// Package tuscore implements a tusd DataStore on top of a small object-store
// interface (Bucket), so the tus semantics can run on minio-go and the Aliyun
// OSS SDK without depending on the upstream s3store or the AWS SDK.
//
// The behaviour mirrors github.com/tus/tusd/v2/pkg/s3store, including the
// request-scoped state cache, the on-disk part staging, concurrent part
// uploads, concatenation, deferred length and content serving. Only the
// transport (the Bucket implementation) differs.
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
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/tus/tusd/v2/pkg/handler"
)

// Defaults mirror the upstream s3store configuration.
const (
	DefaultMinPartSize           int64 = 5 << 20
	DefaultPreferredPartSize     int64 = 50 << 20
	DefaultMaxPartSize           int64 = 5 << 30
	DefaultMaxMultipartParts     int64 = 10000
	DefaultMaxObjectSize         int64 = 5 << 40
	DefaultMaxBufferedParts      int64 = 20
	DefaultConcurrentPartUploads       = 10
)

// nonPrintableRegexp matches every character which is not valid in a header
// value according to RFC 2616. Upstream replaces them in object metadata.
var nonPrintableRegexp = regexp.MustCompile(`[^\x09\x20-\x7E]`)

// errIncompleteUpload is used when a client attempts to download an upload
// that has not been finished yet.
var errIncompleteUpload = handler.NewError("ERR_INCOMPLETE_UPLOAD", "cannot stream non-finished upload", http.StatusBadRequest)

// Store implements handler.DataStore, handler.TerminaterDataStore,
// handler.ConcaterDataStore, handler.LengthDeferrerDataStore and
// handler.ContentServerDataStore.
type Store struct {
	Bucket Bucket
	// Prefix is prepended to the upload id (and to the .info/.part objects).
	Prefix string
	// MetadataPrefix is prepended to the .info/.part objects. Empty means
	// Prefix.
	MetadataPrefix string

	MinPartSize       int64
	PreferredPartSize int64
	MaxPartSize       int64
	MaxMultipartParts int64
	MaxObjectSize     int64
	MaxBufferedParts  int64
	// TemporaryDirectory stages chunk data. Empty uses os.TempDir().
	TemporaryDirectory string
	Logger             Logger

	sem     chan struct{}
	locks   map[string]*sync.Mutex
	locksMu sync.Mutex
}

// New builds a store. prefix should end with "/" (normalised here).
func New(bucket Bucket, prefix string) *Store {
	prefix = strings.Trim(strings.TrimSpace(prefix), "/")
	if prefix != "" {
		prefix += "/"
	}
	store := &Store{
		Bucket:            bucket,
		Prefix:            prefix,
		MinPartSize:       DefaultMinPartSize,
		PreferredPartSize: DefaultPreferredPartSize,
		MaxPartSize:       DefaultMaxPartSize,
		MaxMultipartParts: DefaultMaxMultipartParts,
		MaxObjectSize:     DefaultMaxObjectSize,
		MaxBufferedParts:  DefaultMaxBufferedParts,
		locks:             make(map[string]*sync.Mutex),
	}
	store.SetConcurrentPartUploads(DefaultConcurrentPartUploads)
	return store
}

// SetConcurrentPartUploads limits how many parts are uploaded in parallel.
func (s *Store) SetConcurrentPartUploads(limit int) {
	if limit <= 0 {
		limit = 1
	}
	s.sem = make(chan struct{}, limit)
}

func (s *Store) acquire() { s.sem <- struct{}{} }
func (s *Store) release() { <-s.sem }

// UseIn registers every extension the upstream s3store provides.
func (s *Store) UseIn(composer *handler.StoreComposer) {
	composer.UseCore(s)
	composer.UseTerminater(s)
	composer.UseConcater(s)
	composer.UseLengthDeferrer(s)
	composer.UseContentServer(s)
}

func (s *Store) AsTerminatableUpload(upload handler.Upload) handler.TerminatableUpload {
	return upload.(*Upload)
}

func (s *Store) AsLengthDeclarableUpload(upload handler.Upload) handler.LengthDeclarableUpload {
	return upload.(*Upload)
}

func (s *Store) AsConcatableUpload(upload handler.Upload) handler.ConcatableUpload {
	return upload.(*Upload)
}

func (s *Store) AsServableUpload(upload handler.Upload) handler.ServableUpload {
	return upload.(*Upload)
}

func (s *Store) key(objectID string) string { return s.Prefix + objectID }

func (s *Store) metadataKey(objectID string) string {
	prefix := strings.Trim(strings.TrimSpace(s.MetadataPrefix), "/")
	if prefix == "" {
		return s.key(objectID)
	}
	return prefix + "/" + objectID
}

func (s *Store) infoKey(objectID string) string { return s.metadataKey(objectID) + ".info" }
func (s *Store) partKey(objectID string) string { return s.metadataKey(objectID) + ".part" }

func (s *Store) lockFor(objectID string) *sync.Mutex {
	s.locksMu.Lock()
	defer s.locksMu.Unlock()
	lock, ok := s.locks[objectID]
	if !ok {
		lock = &sync.Mutex{}
		s.locks[objectID] = lock
	}
	return lock
}

// NewUpload creates the multipart upload and the .info object.
func (s *Store) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	if info.Size > s.MaxObjectSize {
		return nil, fmt.Errorf("tuscore: upload size of %v bytes exceeds MaxObjectSize of %v bytes", info.Size, s.MaxObjectSize)
	}

	objectID := strings.TrimSpace(info.ID)
	if objectID == "" {
		generated, err := randomID()
		if err != nil {
			return nil, err
		}
		objectID = generated
	}

	metadata := make(map[string]string, len(info.MetaData))
	for key, value := range info.MetaData {
		metadata[key] = nonPrintableRegexp.ReplaceAllString(value, "?")
	}

	contentType := ""
	if fileType, found := info.MetaData["filetype"]; found {
		contentType = fileType
	}
	uploadID, err := s.Bucket.NewMultipart(ctx, s.key(objectID), contentType, metadata)
	if err != nil {
		return nil, fmt.Errorf("tuscore: unable to create multipart upload: %w", err)
	}

	info.ID = objectID + "+" + uploadID
	info.Storage = map[string]string{
		"Type":   "tuscore",
		"Bucket": s.Bucket.Name(),
		"Key":    s.key(objectID),
	}
	upload := &Upload{
		objectID:    objectID,
		multipartID: uploadID,
		store:       s,
		parts:       []*Part{},
	}
	if err := upload.writeInfo(ctx, info); err != nil {
		return nil, fmt.Errorf("tuscore: unable to create info file: %w", err)
	}
	return upload, nil
}

// GetUpload returns a lazy handle; nothing is fetched until it is needed.
func (s *Store) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	objectID, multipartID, ok := splitIDs(id)
	if !ok {
		return nil, handler.ErrNotFound
	}
	return &Upload{
		objectID:    objectID,
		multipartID: multipartID,
		store:       s,
		parts:       []*Part{},
	}, nil
}

// Upload is the per-upload handle. The info/parts/incompletePartSize fields are
// the request-scoped cache, exactly like upstream s3store.
type Upload struct {
	objectID    string
	multipartID string
	store       *Store

	info               *handler.FileInfo
	parts              []*Part
	incompletePartSize int64
	cacheMu            sync.Mutex
}

func (u *Upload) key() string { return u.store.key(u.objectID) }

func (u *Upload) writeInfo(ctx context.Context, info handler.FileInfo) error {
	u.info = &info
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	return u.store.Bucket.PutObject(ctx, u.store.infoKey(u.objectID), bytes.NewReader(data), int64(len(data)), "application/json")
}

func (u *Upload) getInternalInfo(ctx context.Context) (handler.FileInfo, []*Part, int64, error) {
	if u.info != nil {
		return *u.info, u.parts, u.incompletePartSize, nil
	}
	info, parts, incompletePartSize, err := u.fetchInfo(ctx)
	if err != nil {
		return info, parts, incompletePartSize, err
	}
	u.info = &info
	u.parts = parts
	u.incompletePartSize = incompletePartSize
	return info, parts, incompletePartSize, nil
}

// GetInfo reports the committed bytes plus the persisted tail.
func (u *Upload) GetInfo(ctx context.Context) (handler.FileInfo, error) {
	info, _, _, err := u.getInternalInfo(ctx)
	return info, err
}

// GetReader opens the completed object.
func (u *Upload) GetReader(ctx context.Context) (io.ReadCloser, error) {
	object, err := u.store.Bucket.GetObject(ctx, u.key(), GetOptions{})
	if err == nil {
		return object.Body, nil
	}
	if !u.store.Bucket.IsNotFound(err) {
		return nil, err
	}

	// The object is missing, but the multipart upload may still exist, which
	// means the upload simply has not been finished yet.
	_, _, _, listErr := u.store.Bucket.ListParts(ctx, u.key(), u.multipartID, 0)
	if listErr == nil {
		return nil, errIncompleteUpload
	}
	if u.store.Bucket.IsNoSuchUpload(listErr) || u.store.Bucket.IsNotFound(listErr) {
		return nil, handler.ErrNotFound
	}
	return nil, listErr
}

// fetchInfo loads .info, the parts and the tail size concurrently.
func (u *Upload) fetchInfo(ctx context.Context) (info handler.FileInfo, parts []*Part, incompletePartSize int64, err error) {
	var (
		wg                    sync.WaitGroup
		infoErr               error
		partsErr              error
		incompletePartSizeErr error
	)
	wg.Add(3)
	go func() {
		defer wg.Done()
		object, getErr := u.store.Bucket.GetObject(ctx, u.store.infoKey(u.objectID), GetOptions{})
		if getErr != nil {
			infoErr = getErr
			return
		}
		defer object.Body.Close()
		infoErr = json.NewDecoder(io.LimitReader(object.Body, 1<<20)).Decode(&info)
	}()
	go func() {
		defer wg.Done()
		parts, partsErr = u.listAllParts(ctx, u.objectID, u.multipartID)
	}()
	go func() {
		defer wg.Done()
		incompletePartSize, incompletePartSizeErr = u.store.headIncompletePart(ctx, u.objectID)
	}()
	wg.Wait()

	if infoErr != nil {
		if u.store.Bucket.IsNotFound(infoErr) {
			return info, nil, 0, handler.ErrNotFound
		}
		return info, nil, 0, infoErr
	}
	if partsErr != nil {
		if u.store.Bucket.IsNoSuchUpload(partsErr) || u.store.Bucket.IsNotFound(partsErr) {
			// The multipart upload is gone: it was either completed or aborted.
			// Because the .info object exists we treat it as completed.
			info.Offset = info.Size
			return info, nil, 0, nil
		}
		return info, nil, 0, partsErr
	}
	if incompletePartSizeErr != nil {
		return info, nil, 0, incompletePartSizeErr
	}

	offset := incompletePartSize
	for _, part := range parts {
		offset += part.Size
	}
	info.Offset = offset
	return info, parts, incompletePartSize, nil
}

// WriteChunk streams src into parts, staging bytes on disk.
func (u *Upload) WriteChunk(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	info, _, incompletePartSize, err := u.getInternalInfo(ctx)
	if err != nil {
		return 0, err
	}
	if offset != info.Offset {
		return 0, fmt.Errorf("tuscore: invalid offset %d, expected %d", offset, info.Offset)
	}

	if incompletePartSize > 0 {
		partFile, err := u.store.downloadIncompletePart(ctx, u.objectID)
		if err != nil {
			return 0, err
		}
		if partFile == nil {
			return 0, errors.New("tuscore: expected an incomplete part file but did not get any")
		}
		defer cleanUpTempFile(partFile)

		if err := u.store.deleteIncompletePart(ctx, u.objectID); err != nil {
			return 0, err
		}
		u.cacheMu.Lock()
		u.incompletePartSize = 0
		u.cacheMu.Unlock()

		src = io.MultiReader(partFile, src)
		offset -= incompletePartSize
	}

	bytesUploaded, err := u.uploadParts(ctx, offset, src)
	bytesUploaded = max(bytesUploaded-incompletePartSize, 0)
	if u.info != nil {
		u.info.Offset += bytesUploaded
	}
	return bytesUploaded, err
}

// uploadParts converts the request body into parts, uploading them
// concurrently and parking a sub-MinPartSize tail in the .part object.
func (u *Upload) uploadParts(ctx context.Context, offset int64, src io.Reader) (int64, error) {
	store := u.store
	info, parts, _, err := u.getInternalInfo(ctx)
	if err != nil {
		return 0, err
	}

	size := info.Size
	bytesUploaded := int64(0)
	optimalPartSize, err := store.calcOptimalPartSize(size)
	if err != nil {
		return 0, err
	}

	nextPartNum := int32(len(parts) + 1)
	producer, fileChan := newPartProducer(src, store.MaxBufferedParts, store.temporaryDirectory())

	producerCtx, cancelProducer := context.WithCancel(ctx)
	defer func() {
		cancelProducer()
		producer.closeUnreadFiles()
	}()
	go producer.produce(producerCtx, optimalPartSize)

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	setErr := func(err error) {
		errMu.Lock()
		if firstErr == nil {
			firstErr = err
		}
		errMu.Unlock()
	}

	for {
		store.acquire()
		chunk, more := <-fileChan
		if !more {
			store.release()
			break
		}

		isFinalChunk := !info.SizeIsDeferred && size == offset+bytesUploaded+chunk.size
		if chunk.size >= store.MinPartSize || isFinalChunk {
			part := &Part{Number: nextPartNum, Size: chunk.size}
			u.parts = append(u.parts, part)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer store.release()
				etag, uploadErr := store.Bucket.UploadPart(ctx, u.key(), u.multipartID, part.Number, chunk.reader, part.Size)
				if uploadErr != nil {
					setErr(uploadErr)
				} else {
					part.ETag = etag
				}
				if closeErr := chunk.close(); closeErr != nil && uploadErr == nil {
					setErr(closeErr)
				}
			}()
		} else {
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer store.release()
				putErr := store.putIncompletePart(ctx, u.objectID, chunk.reader, chunk.size)
				if putErr != nil {
					setErr(putErr)
				} else {
					u.cacheMu.Lock()
					u.incompletePartSize = chunk.size
					u.cacheMu.Unlock()
				}
				if closeErr := chunk.close(); closeErr != nil && putErr == nil {
					setErr(closeErr)
				}
			}()
		}

		bytesUploaded += chunk.size
		nextPartNum++
	}

	wg.Wait()
	if firstErr != nil {
		return 0, firstErr
	}
	return bytesUploaded, producer.err
}

// FinishUpload promotes the tail to the last part and completes the upload,
// following the upstream branch order.
func (u *Upload) FinishUpload(ctx context.Context) error {
	store := u.store
	info, parts, incompletePartSize, err := u.getInternalInfo(ctx)
	if err != nil {
		return err
	}

	if incompletePartSize > 0 {
		var partsSize int64
		for _, part := range parts {
			partsSize += part.Size
		}

		switch {
		case partsSize == info.Size:
			// The parts already cover the declared size, so the leftover .part is
			// stale (a completion retried after the part was promoted). Drop it.
			if err := store.deleteIncompletePart(ctx, u.objectID); err != nil {
				return err
			}
			u.incompletePartSize = 0
		case partsSize+incompletePartSize == info.Size:
			partFile, err := store.downloadIncompletePart(ctx, u.objectID)
			if err != nil {
				return err
			}
			if partFile == nil {
				return fmt.Errorf("tuscore: expected an incomplete part file for upload %s but did not get any", u.objectID)
			}
			defer cleanUpTempFile(partFile)

			partNumber := int32(1)
			if len(parts) > 0 {
				partNumber = parts[len(parts)-1].Number + 1
			}
			etag, err := store.Bucket.UploadPart(ctx, u.key(), u.multipartID, partNumber, partFile, incompletePartSize)
			if err != nil {
				return err
			}
			parts = append(parts, &Part{Number: partNumber, Size: incompletePartSize, ETag: etag})
			u.parts = parts

			if err := store.deleteIncompletePart(ctx, u.objectID); err != nil {
				return err
			}
			u.incompletePartSize = 0
		default:
			return fmt.Errorf("tuscore: cannot finish upload %s: multipart parts (%d bytes) plus incomplete part (%d bytes) do not match the declared size (%d bytes)",
				u.objectID, partsSize, incompletePartSize, info.Size)
		}
	}

	if len(parts) == 0 {
		// S3 requires at least one part for a multipart completion.
		etag, err := store.Bucket.UploadPart(ctx, u.key(), u.multipartID, 1, bytes.NewReader(nil), 0)
		if err != nil {
			return err
		}
		parts = []*Part{{Number: 1, Size: 0, ETag: etag}}
		u.parts = parts
	}

	completed := make([]Part, 0, len(parts))
	for _, part := range parts {
		completed = append(completed, *part)
	}
	if err := store.Bucket.CompleteMultipart(ctx, u.key(), u.multipartID, completed); err != nil {
		return err
	}

	// The object now exists; mark the upload as finished in the cached info and
	// persist the final offset.
	finished := info
	finished.Offset = finished.Size
	u.parts = parts
	return u.writeInfo(ctx, finished)
}

// Terminate aborts the multipart upload and removes all companion objects.
func (u *Upload) Terminate(ctx context.Context) error {
	store := u.store
	var wg sync.WaitGroup
	errCh := make(chan error, 4)

	wg.Add(1)
	go func() {
		defer wg.Done()
		err := store.Bucket.AbortMultipart(ctx, u.key(), u.multipartID)
		if err != nil && !store.Bucket.IsNoSuchUpload(err) && !store.Bucket.IsNotFound(err) {
			errCh <- err
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, key := range []string{u.key(), store.partKey(u.objectID), store.infoKey(u.objectID)} {
			if err := store.Bucket.DeleteObject(ctx, key); err != nil && !store.Bucket.IsNotFound(err) {
				errCh <- err
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	errs := make([]error, 0, 4)
	for err := range errCh {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// DeclareLength implements handler.LengthDeclarableUpload.
func (u *Upload) DeclareLength(ctx context.Context, length int64) error {
	info, err := u.GetInfo(ctx)
	if err != nil {
		return err
	}
	info.Size = length
	info.SizeIsDeferred = false
	return u.writeInfo(ctx, info)
}

// ConcatUploads implements handler.ConcatableUpload.
func (u *Upload) ConcatUploads(ctx context.Context, partialUploads []handler.Upload) error {
	store := u.store
	hasSmallPart := false
	sizes := make([]int64, len(partialUploads))
	for index, partial := range partialUploads {
		info, err := partial.GetInfo(ctx)
		if err != nil {
			return err
		}
		sizes[index] = info.Size
		if info.Size < store.MinPartSize {
			hasSmallPart = true
		}
	}
	if hasSmallPart {
		return u.concatUsingDownload(ctx, partialUploads)
	}
	return u.concatUsingMultipart(ctx, partialUploads, sizes)
}

func (u *Upload) concatUsingDownload(ctx context.Context, partialUploads []handler.Upload) error {
	store := u.store
	file, err := os.CreateTemp(store.temporaryDirectory(), "tus-concat-")
	if err != nil {
		return err
	}
	defer cleanUpTempFile(file)

	for _, partial := range partialUploads {
		source := partial.(*Upload)
		object, err := store.Bucket.GetObject(ctx, source.key(), GetOptions{})
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(file, object.Body)
		_ = object.Body.Close()
		if copyErr != nil {
			return copyErr
		}
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	stat, err := file.Stat()
	if err != nil {
		return err
	}
	if err := store.Bucket.PutObject(ctx, u.key(), file, stat.Size(), ""); err != nil {
		return err
	}

	// The multipart upload is no longer used; abort it in the background like
	// upstream does, ignoring the outcome.
	go func() {
		_ = store.Bucket.AbortMultipart(context.Background(), u.key(), u.multipartID)
	}()
	return nil
}

func (u *Upload) concatUsingMultipart(ctx context.Context, partialUploads []handler.Upload, sizes []int64) error {
	store := u.store
	u.parts = make([]*Part, len(partialUploads))
	var wg sync.WaitGroup
	errCh := make(chan error, len(partialUploads))

	for index, partial := range partialUploads {
		partNumber := int32(index + 1)
		source := partial.(*Upload)
		size := sizes[index]
		wg.Add(1)
		go func() {
			defer wg.Done()
			etag, err := store.Bucket.UploadPartCopy(ctx, u.key(), u.multipartID, partNumber, source.key(), size)
			if err != nil {
				errCh <- err
				return
			}
			u.parts[partNumber-1] = &Part{Number: partNumber, Size: size, ETag: etag}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		return err
	}
	return u.FinishUpload(ctx)
}

// fetch helpers -------------------------------------------------------------

func (u *Upload) listAllParts(ctx context.Context, objectID, multipartID string) ([]*Part, error) {
	var parts []*Part
	var marker int32
	for {
		page, truncated, next, err := u.store.Bucket.ListParts(ctx, u.store.key(objectID), multipartID, marker)
		if err != nil {
			return nil, err
		}
		for _, part := range page {
			parts = append(parts, &Part{Number: part.Number, Size: part.Size, ETag: part.ETag})
		}
		if !truncated {
			return parts, nil
		}
		marker = next
	}
}

func (s *Store) headIncompletePart(ctx context.Context, objectID string) (int64, error) {
	size, err := s.Bucket.HeadObject(ctx, s.partKey(objectID))
	if err != nil {
		if s.Bucket.IsNotFound(err) {
			return 0, nil
		}
		return 0, err
	}
	return size, nil
}

func (s *Store) downloadIncompletePart(ctx context.Context, objectID string) (*os.File, error) {
	object, err := s.Bucket.GetObject(ctx, s.partKey(objectID), GetOptions{})
	if err != nil {
		if s.Bucket.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer object.Body.Close()

	file, err := os.CreateTemp(s.temporaryDirectory(), "tus-tail-")
	if err != nil {
		return nil, err
	}
	n, err := io.Copy(file, object.Body)
	if err != nil {
		cleanUpTempFile(file)
		return nil, err
	}
	if object.Size > 0 && n < object.Size {
		cleanUpTempFile(file)
		return nil, errors.New("tuscore: short read of incomplete upload")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanUpTempFile(file)
		return nil, err
	}
	return file, nil
}

func (s *Store) putIncompletePart(ctx context.Context, objectID string, body io.Reader, size int64) error {
	return s.Bucket.PutObject(ctx, s.partKey(objectID), body, size, "")
}

func (s *Store) deleteIncompletePart(ctx context.Context, objectID string) error {
	return s.Bucket.DeleteObject(ctx, s.partKey(objectID))
}

func (s *Store) temporaryDirectory() string {
	if dir := strings.TrimSpace(s.TemporaryDirectory); dir != "" {
		return dir
	}
	return ""
}

// calcOptimalPartSize mirrors the upstream calculation.
func (s *Store) calcOptimalPartSize(size int64) (int64, error) {
	var optimal int64
	switch {
	case size <= s.PreferredPartSize:
		optimal = s.PreferredPartSize
	case size <= s.PreferredPartSize*s.MaxMultipartParts:
		optimal = s.PreferredPartSize
	case size%s.MaxMultipartParts == 0:
		optimal = size / s.MaxMultipartParts
	default:
		optimal = size/s.MaxMultipartParts + 1
	}
	if optimal > s.MaxPartSize {
		return optimal, fmt.Errorf("tuscore: to upload %v bytes optimalPartSize %v must exceed MaxPartSize %v", size, optimal, s.MaxPartSize)
	}
	return optimal, nil
}

func cleanUpTempFile(file *os.File) {
	_ = file.Close()
	_ = os.Remove(file.Name())
}

func splitIDs(id string) (objectID, multipartID string, ok bool) {
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
