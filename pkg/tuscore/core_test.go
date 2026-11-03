package tuscore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

var (
	errFakeNotFound     = errors.New("fake: not found")
	errFakeNoSuchUpload = errors.New("fake: no such upload")
)

// fakeBucket is an in-memory multipart store used to exercise the tus logic.
type fakeBucket struct {
	mu             sync.Mutex
	objects        map[string][]byte
	uploads        map[string]*fakeUpload
	nextID         int
	listPartsCalls atomic.Int32
	activeUploads  atomic.Int32
	maxActive      atomic.Int32
	uploadDelay    time.Duration
}

type fakeUpload struct {
	key   string
	parts map[int32][]byte
}

func newFake() *fakeBucket {
	return &fakeBucket{objects: map[string][]byte{}, uploads: map[string]*fakeUpload{}}
}

func (b *fakeBucket) Name() string { return "fake" }

func (b *fakeBucket) NewMultipart(ctx context.Context, key, contentType string, metadata map[string]string) (string, error) {
	_ = ctx
	_ = contentType
	_ = metadata
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID++
	id := fmt.Sprintf("mp%d", b.nextID)
	b.uploads[id] = &fakeUpload{key: key, parts: map[int32][]byte{}}
	return id, nil
}

func (b *fakeBucket) UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (string, error) {
	_ = ctx
	active := b.activeUploads.Add(1)
	defer b.activeUploads.Add(-1)
	for {
		max := b.maxActive.Load()
		if active <= max || b.maxActive.CompareAndSwap(max, active) {
			break
		}
	}
	if b.uploadDelay > 0 {
		time.Sleep(b.uploadDelay)
	}

	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != size {
		return "", fmt.Errorf("part size mismatch")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return "", errFakeNoSuchUpload
	}
	upload.parts[number] = data
	return fmt.Sprintf("etag-%d", number), nil
}

func (b *fakeBucket) UploadPartCopy(ctx context.Context, key, uploadID string, number int32, sourceKey string, size int64) (string, error) {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	source, ok := b.objects[sourceKey]
	if !ok {
		return "", errFakeNotFound
	}
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return "", errFakeNoSuchUpload
	}
	if size >= 0 && int64(len(source)) != size {
		return "", fmt.Errorf("copy size mismatch: %d != %d", len(source), size)
	}
	copied := make([]byte, len(source))
	copy(copied, source)
	upload.parts[number] = copied
	return fmt.Sprintf("etag-%d", number), nil
}

func (b *fakeBucket) ListParts(ctx context.Context, key, uploadID string, marker int32) ([]Part, bool, int32, error) {
	_ = ctx
	_ = marker
	b.listPartsCalls.Add(1)
	b.mu.Lock()
	defer b.mu.Unlock()
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return nil, false, 0, errFakeNoSuchUpload
	}
	parts := make([]Part, 0, len(upload.parts))
	for number, data := range upload.parts {
		parts = append(parts, Part{Number: number, Size: int64(len(data)), ETag: fmt.Sprintf("etag-%d", number)})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, false, 0, nil
}

func (b *fakeBucket) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return errFakeNoSuchUpload
	}
	var data []byte
	for _, part := range parts {
		chunk, ok := upload.parts[part.Number]
		if !ok {
			return fmt.Errorf("missing part %d", part.Number)
		}
		data = append(data, chunk...)
	}
	b.objects[key] = data
	delete(b.uploads, uploadID)
	return nil
}

func (b *fakeBucket) AbortMultipart(ctx context.Context, key, uploadID string) error {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.uploads, uploadID)
	return nil
}

func (b *fakeBucket) PutObject(ctx context.Context, key string, body io.Reader, size int64, contentType string) error {
	_ = ctx
	_ = contentType
	data, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if int64(len(data)) != size {
		return fmt.Errorf("object size mismatch: got %d want %d", len(data), size)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.objects[key] = data
	return nil
}

func (b *fakeBucket) GetObject(ctx context.Context, key string, opts GetOptions) (*Object, error) {
	_ = ctx
	b.mu.Lock()
	data, ok := b.objects[key]
	b.mu.Unlock()
	if !ok {
		return nil, errFakeNotFound
	}
	start, end := int64(0), int64(len(data))-1
	contentRange := ""
	if opts.Range != "" {
		parsedStart, parsedEnd, parsedOK := parseTestRange(opts.Range, int64(len(data)))
		if !parsedOK {
			return nil, &fakeStatusError{status: 416}
		}
		start, end = parsedStart, parsedEnd
		contentRange = fmt.Sprintf("bytes %d-%d/%d", start, end, len(data))
	}
	return &Object{
		Body:         io.NopCloser(bytes.NewReader(data[start : end+1])),
		Size:         int64(len(data)),
		ContentRange: contentRange,
		ETag:         "etag",
	}, nil
}

func (b *fakeBucket) HeadObject(ctx context.Context, key string) (int64, error) {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	data, ok := b.objects[key]
	if !ok {
		return 0, errFakeNotFound
	}
	return int64(len(data)), nil
}

func (b *fakeBucket) DeleteObject(ctx context.Context, key string) error {
	_ = ctx
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.objects, key)
	return nil
}

// StatObjectForTest lets the tests assert on companion objects.
func (b *fakeBucket) StatObjectForTest(ctx context.Context, key string) (int64, error) {
	return b.HeadObject(ctx, key)
}

func (b *fakeBucket) IsNotFound(err error) bool     { return errors.Is(err, errFakeNotFound) }
func (b *fakeBucket) IsNoSuchUpload(err error) bool { return errors.Is(err, errFakeNoSuchUpload) }
func (b *fakeBucket) StatusCode(err error) int {
	var status *fakeStatusError
	if errors.As(err, &status) {
		return status.status
	}
	return 0
}

type fakeStatusError struct{ status int }

func (e *fakeStatusError) Error() string { return strconv.Itoa(e.status) }

func parseTestRange(header string, size int64) (int64, int64, bool) {
	spec := strings.TrimPrefix(header, "bytes=")
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, false
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 || start >= size {
		return 0, 0, false
	}
	end := size - 1
	if last != "" {
		parsed, err := strconv.ParseInt(last, 10, 64)
		if err != nil || parsed < start {
			return 0, 0, false
		}
		if parsed < size {
			end = parsed
		}
	}
	return start, end, true
}

func newStore(t *testing.T) (*Store, *fakeBucket) {
	t.Helper()
	bucket := newFake()
	store := New(bucket, "iotda/upload/tus/")
	store.MinPartSize = 5 << 20
	store.PreferredPartSize = 5 << 20
	return store, bucket
}

func upload(t *testing.T, store *Store, size int64) handler.Upload {
	t.Helper()
	handle, err := store.NewUpload(context.Background(), handler.FileInfo{
		Size:     size,
		MetaData: handler.MetaData{"filename": "a.bin", "filetype": "application/octet-stream"},
	})
	if err != nil {
		t.Fatalf("NewUpload: %v", err)
	}
	return handle
}

func objectIDOf(t *testing.T, handle handler.Upload) string {
	t.Helper()
	info, err := handle.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	return strings.SplitN(info.ID, "+", 2)[0]
}

func TestSmallUpload(t *testing.T) {
	store, bucket := newStore(t)
	handle := upload(t, store, 11)

	payload := []byte("hello world")
	if written, err := handle.WriteChunk(context.Background(), 0, bytes.NewReader(payload)); err != nil || written != 11 {
		t.Fatalf("WriteChunk = %d, %v", written, err)
	}
	info, err := handle.GetInfo(context.Background())
	if err != nil || info.Offset != 11 {
		t.Fatalf("GetInfo offset=%d err=%v", info.Offset, err)
	}
	if err := handle.FinishUpload(context.Background()); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	objectID := strings.SplitN(info.ID, "+", 2)[0]
	if got := string(bucket.objects["iotda/upload/tus/"+objectID]); got != "hello world" {
		t.Fatalf("object = %q", got)
	}
	if _, err := bucket.StatObjectForTest(context.Background(), "iotda/upload/tus/"+objectID+".part"); !errors.Is(err, errFakeNotFound) {
		t.Fatalf("tail should be removed, got %v", err)
	}
}

func TestMultipartUpload(t *testing.T) {
	store, bucket := newStore(t)
	size := int64(12 << 20)
	handle := upload(t, store, size)

	payload := make([]byte, size)
	if _, err := rand.Read(payload); err != nil {
		t.Fatal(err)
	}
	for offset := int64(0); offset < size; offset += 3 << 20 {
		end := offset + 3<<20
		if end > size {
			end = size
		}
		if _, err := handle.WriteChunk(context.Background(), offset, bytes.NewReader(payload[offset:end])); err != nil {
			t.Fatalf("WriteChunk@%d: %v", offset, err)
		}
	}
	info, err := handle.GetInfo(context.Background())
	if err != nil || info.Offset != size {
		t.Fatalf("GetInfo offset=%d err=%v", info.Offset, err)
	}
	if err := handle.FinishUpload(context.Background()); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	objectID := strings.SplitN(info.ID, "+", 2)[0]
	if got := bucket.objects["iotda/upload/tus/"+objectID]; !bytes.Equal(got, payload) {
		t.Fatalf("object content mismatch: got %d bytes", len(got))
	}
}

func TestResumeFromTail(t *testing.T) {
	store, bucket := newStore(t)
	first := make([]byte, 3<<20)
	second := make([]byte, 3<<20)
	if _, err := rand.Read(first); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(second); err != nil {
		t.Fatal(err)
	}

	handle := upload(t, store, int64(len(first)+len(second)))
	if _, err := handle.WriteChunk(context.Background(), 0, bytes.NewReader(first)); err != nil {
		t.Fatalf("first chunk: %v", err)
	}
	info, _ := handle.GetInfo(context.Background())
	id := info.ID

	// Simulate a restart: reload the upload from storage.
	reopened, err := store.GetUpload(context.Background(), id)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	reopenedInfo, err := reopened.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo after restart: %v", err)
	}
	if reopenedInfo.Offset != int64(len(first)) {
		t.Fatalf("resume offset=%d, want %d", reopenedInfo.Offset, len(first))
	}
	if _, err := reopened.WriteChunk(context.Background(), int64(len(first)), bytes.NewReader(second)); err != nil {
		t.Fatalf("second chunk: %v", err)
	}
	if err := reopened.FinishUpload(context.Background()); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	objectID := strings.SplitN(id, "+", 2)[0]
	if got := bucket.objects["iotda/upload/tus/"+objectID]; !bytes.Equal(got, append(first, second...)) {
		t.Fatalf("resumed object mismatch: %d bytes", len(got))
	}
}

func TestZeroByteUpload(t *testing.T) {
	store, bucket := newStore(t)
	handle := upload(t, store, 0)
	if err := handle.FinishUpload(context.Background()); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	objectID := objectIDOf(t, handle)
	if data, ok := bucket.objects["iotda/upload/tus/"+objectID]; !ok || len(data) != 0 {
		t.Fatalf("zero byte object missing")
	}
}

func TestOffsetMismatch(t *testing.T) {
	store, _ := newStore(t)
	handle := upload(t, store, 10)
	if _, err := handle.WriteChunk(context.Background(), 5, bytes.NewReader([]byte("12345"))); err == nil {
		t.Fatal("expected an offset error")
	}
}

func TestTerminateRemovesCompanions(t *testing.T) {
	store, bucket := newStore(t)
	handle := upload(t, store, 20)
	if _, err := handle.WriteChunk(context.Background(), 0, bytes.NewReader([]byte("0123456789"))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	objectID := objectIDOf(t, handle)
	if err := store.AsTerminatableUpload(handle).Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	for _, key := range []string{
		"iotda/upload/tus/" + objectID,
		"iotda/upload/tus/" + objectID + ".info",
		"iotda/upload/tus/" + objectID + ".part",
	} {
		if _, err := bucket.StatObjectForTest(context.Background(), key); !errors.Is(err, errFakeNotFound) {
			t.Fatalf("%s should be gone, got %v", key, err)
		}
	}
	if len(bucket.uploads) != 0 {
		t.Fatalf("multipart uploads left: %d", len(bucket.uploads))
	}
}

// tusd calls GetInfo before WriteChunk and again while finishing. All of that
// happens on one upload handle, so the state must be fetched only once.
func TestStateFetchedOncePerHandle(t *testing.T) {
	ctx := context.Background()
	store, bucket := newStore(t)
	handle := upload(t, store, 12<<20)
	if _, err := handle.WriteChunk(ctx, 0, bytes.NewReader(make([]byte, 7<<20))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if got := bucket.listPartsCalls.Load(); got != 0 {
		t.Fatalf("the creating handle must not list parts, got %d", got)
	}

	info, err := handle.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	reopened, err := store.GetUpload(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if _, err := reopened.GetInfo(ctx); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if _, err := reopened.GetInfo(ctx); err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if _, err := reopened.WriteChunk(ctx, 7<<20, bytes.NewReader(make([]byte, 5<<20))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if err := reopened.FinishUpload(ctx); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	if got := bucket.listPartsCalls.Load(); got != 1 {
		t.Fatalf("ListParts calls = %d, want 1", got)
	}
}

// A completion retried after the tail was promoted leaves a stale .part behind;
// it must be dropped instead of uploaded a second time.
func TestFinishDropsStaleTail(t *testing.T) {
	ctx := context.Background()
	store, bucket := newStore(t)
	handle := upload(t, store, 5<<20)
	if _, err := handle.WriteChunk(ctx, 0, bytes.NewReader(make([]byte, 5<<20))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	info, err := handle.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	objectID := strings.SplitN(info.ID, "+", 2)[0]
	bucket.objects["iotda/upload/tus/"+objectID+".part"] = make([]byte, 2<<20)

	// The retry arrives as a new request, so it must be a fresh handle.
	reopened, err := store.GetUpload(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if err := reopened.FinishUpload(ctx); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
	if size := len(bucket.objects["iotda/upload/tus/"+objectID]); size != 5<<20 {
		t.Fatalf("object size = %d, want %d", size, 5<<20)
	}
	if _, ok := bucket.objects["iotda/upload/tus/"+objectID+".part"]; ok {
		t.Fatal("stale .part must be removed")
	}
}

func TestFinishRejectsMismatchedTail(t *testing.T) {
	ctx := context.Background()
	store, bucket := newStore(t)
	handle := upload(t, store, 10<<20)
	if _, err := handle.WriteChunk(ctx, 0, bytes.NewReader(make([]byte, 5<<20))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	info, err := handle.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	objectID := strings.SplitN(info.ID, "+", 2)[0]
	bucket.objects["iotda/upload/tus/"+objectID+".part"] = make([]byte, 2<<20)

	reopened, err := store.GetUpload(ctx, info.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	if err := reopened.FinishUpload(ctx); err == nil {
		t.Fatal("FinishUpload must reject parts + tail != declared size")
	}
}

func TestPartsUploadedConcurrently(t *testing.T) {
	ctx := context.Background()
	bucket := newFake()
	bucket.uploadDelay = 5 * time.Millisecond
	store := New(bucket, "iotda/upload/tus/")
	store.MinPartSize = 1
	store.PreferredPartSize = 1 << 20
	store.SetConcurrentPartUploads(4)

	handle := upload(t, store, 8<<20)
	if _, err := handle.WriteChunk(ctx, 0, bytes.NewReader(make([]byte, 8<<20))); err != nil {
		t.Fatalf("WriteChunk: %v", err)
	}
	if got := bucket.maxActive.Load(); got < 2 {
		t.Fatalf("max concurrent part uploads = %d, want >= 2", got)
	}
	if err := handle.FinishUpload(ctx); err != nil {
		t.Fatalf("FinishUpload: %v", err)
	}
}

func TestConcatUploads(t *testing.T) {
	ctx := context.Background()
	store, bucket := newStore(t)
	part := make([]byte, 5<<20)
	if _, err := rand.Read(part); err != nil {
		t.Fatal(err)
	}

	first := upload(t, store, 5<<20)
	if _, err := first.WriteChunk(ctx, 0, bytes.NewReader(part)); err != nil {
		t.Fatalf("first WriteChunk: %v", err)
	}
	if err := first.FinishUpload(ctx); err != nil {
		t.Fatalf("first FinishUpload: %v", err)
	}
	second := upload(t, store, 5<<20)
	if _, err := second.WriteChunk(ctx, 0, bytes.NewReader(part)); err != nil {
		t.Fatalf("second WriteChunk: %v", err)
	}
	if err := second.FinishUpload(ctx); err != nil {
		t.Fatalf("second FinishUpload: %v", err)
	}

	target := upload(t, store, 10<<20)
	if err := store.AsConcatableUpload(target).ConcatUploads(ctx, []handler.Upload{first, second}); err != nil {
		t.Fatalf("ConcatUploads: %v", err)
	}
	objectID := objectIDOf(t, target)
	if size := len(bucket.objects["iotda/upload/tus/"+objectID]); size != 10<<20 {
		t.Fatalf("concatenated size = %d, want %d", size, 10<<20)
	}
}

func TestDeclareLength(t *testing.T) {
	ctx := context.Background()
	store, _ := newStore(t)
	handle := upload(t, store, 0)
	if err := store.AsLengthDeclarableUpload(handle).DeclareLength(ctx, 1024); err != nil {
		t.Fatalf("DeclareLength: %v", err)
	}
	info, err := handle.GetInfo(ctx)
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if info.Size != 1024 || info.SizeIsDeferred {
		t.Fatalf("size=%d deferred=%v", info.Size, info.SizeIsDeferred)
	}
}

func TestInfoSurvivesRestart(t *testing.T) {
	store, _ := newStore(t)
	handle := upload(t, store, 5)
	info, _ := handle.GetInfo(context.Background())
	reloaded, err := store.GetUpload(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("GetUpload: %v", err)
	}
	reloadedInfo, err := reloaded.GetInfo(context.Background())
	if err != nil {
		t.Fatalf("GetInfo: %v", err)
	}
	if reloadedInfo.Size != 5 || reloadedInfo.MetaData["filename"] != "a.bin" {
		t.Fatalf("metadata lost: %#v", reloadedInfo)
	}
}
