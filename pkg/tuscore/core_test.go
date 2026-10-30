package tuscore

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/handler"
)

var (
	errFakeNotFound     = errors.New("fake: not found")
	errFakeNoSuchUpload = errors.New("fake: no such upload")
)

// fakeBucket is an in-memory multipart store used to exercise the tus logic.
type fakeBucket struct {
	objects map[string][]byte
	uploads map[string]*fakeUpload
	nextID  int
}

type fakeUpload struct {
	key   string
	parts map[int32][]byte
}

func newFake() *fakeBucket {
	return &fakeBucket{objects: map[string][]byte{}, uploads: map[string]*fakeUpload{}}
}

func (b *fakeBucket) NewMultipart(ctx context.Context, key, contentType string) (string, error) {
	_ = ctx
	_ = contentType
	b.nextID++
	id := fmt.Sprintf("mp%d", b.nextID)
	b.uploads[id] = &fakeUpload{key: key, parts: map[int32][]byte{}}
	return id, nil
}

func (b *fakeBucket) UploadPart(ctx context.Context, key, uploadID string, number int32, body io.Reader, size int64) (string, error) {
	_ = ctx
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return "", errFakeNoSuchUpload
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if int64(len(data)) != size {
		return "", fmt.Errorf("part size mismatch")
	}
	upload.parts[number] = data
	return fmt.Sprintf("etag-%d", number), nil
}

func (b *fakeBucket) ListParts(ctx context.Context, key, uploadID string) ([]Part, error) {
	_ = ctx
	upload, ok := b.uploads[uploadID]
	if !ok || upload.key != key {
		return nil, errFakeNoSuchUpload
	}
	parts := make([]Part, 0, len(upload.parts))
	for number, data := range upload.parts {
		parts = append(parts, Part{Number: number, Size: int64(len(data)), ETag: fmt.Sprintf("etag-%d", number)})
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

func (b *fakeBucket) CompleteMultipart(ctx context.Context, key, uploadID string, parts []Part) error {
	_ = ctx
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
	b.objects[key] = data
	return nil
}

func (b *fakeBucket) GetObject(ctx context.Context, key string) (io.ReadCloser, error) {
	_ = ctx
	data, ok := b.objects[key]
	if !ok {
		return nil, errFakeNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (b *fakeBucket) StatObject(ctx context.Context, key string) (int64, error) {
	_ = ctx
	data, ok := b.objects[key]
	if !ok {
		return 0, errFakeNotFound
	}
	return int64(len(data)), nil
}

func (b *fakeBucket) DeleteObject(ctx context.Context, key string) error {
	_ = ctx
	delete(b.objects, key)
	return nil
}

func (b *fakeBucket) IsNotFound(err error) bool     { return errors.Is(err, errFakeNotFound) }
func (b *fakeBucket) IsNoSuchUpload(err error) bool { return errors.Is(err, errFakeNoSuchUpload) }

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
	if _, err := bucket.StatObject(context.Background(), "iotda/upload/tus/"+objectID+".part"); !errors.Is(err, errFakeNotFound) {
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
	info, _ := handle.GetInfo(context.Background())
	objectID := strings.SplitN(info.ID, "+", 2)[0]
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
	info, _ := handle.GetInfo(context.Background())
	objectID := strings.SplitN(info.ID, "+", 2)[0]
	if err := store.AsTerminatableUpload(handle).Terminate(context.Background()); err != nil {
		t.Fatalf("Terminate: %v", err)
	}
	for _, key := range []string{
		"iotda/upload/tus/" + objectID,
		"iotda/upload/tus/" + objectID + ".info",
		"iotda/upload/tus/" + objectID + ".part",
	} {
		if _, err := bucket.StatObject(context.Background(), key); !errors.Is(err, errFakeNotFound) {
			t.Fatalf("%s should be gone, got %v", key, err)
		}
	}
	if len(bucket.uploads) != 0 {
		t.Fatalf("multipart uploads left: %d", len(bucket.uploads))
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
