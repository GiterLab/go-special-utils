package tuscore

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
)

const tempDirUseMemory = "_memory"

// fileChunk is one staged part: a file (or memory buffer) of exactly size bytes.
type fileChunk struct {
	reader io.ReadSeeker
	close  func() error
	size   int64
}

// partProducer converts the request body into a stream of staged parts, so the
// reader and the uploads to the object store can overlap.
type partProducer struct {
	tmpDir string
	files  chan fileChunk
	err    error
	r      io.Reader
}

func newPartProducer(source io.Reader, backlog int64, tmpDir string) (*partProducer, <-chan fileChunk) {
	if backlog <= 0 {
		backlog = 1
	}
	if os.Getenv("TUSCORE_TEMP_MEMORY") == "1" {
		tmpDir = tempDirUseMemory
	}
	fileChan := make(chan fileChunk, backlog)
	return &partProducer{tmpDir: tmpDir, files: fileChan, r: source}, fileChan
}

// closeUnreadFiles drains the channel so no staged file descriptor leaks when
// the consumer returns early.
func (p *partProducer) closeUnreadFiles() {
	for chunk := range p.files {
		_ = chunk.close()
	}
}

func (p *partProducer) produce(ctx context.Context, partSize int64) {
outer:
	for {
		chunk, ok, err := p.nextPart(partSize)
		if err != nil {
			p.err = err
			break
		}
		if !ok {
			break
		}
		select {
		case p.files <- chunk:
		case <-ctx.Done():
			_ = chunk.close()
			break outer
		}
	}
	close(p.files)
}

func (p *partProducer) nextPart(size int64) (fileChunk, bool, error) {
	if p.tmpDir != tempDirUseMemory {
		file, err := os.CreateTemp(p.tmpDir, "tus-part-")
		if err != nil {
			return fileChunk{}, false, err
		}
		n, err := io.Copy(file, io.LimitReader(p.r, size))
		if err != nil {
			cleanUpTempFile(file)
			return fileChunk{}, false, err
		}
		if n == 0 {
			cleanUpTempFile(file)
			return fileChunk{}, false, nil
		}
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			cleanUpTempFile(file)
			return fileChunk{}, false, err
		}
		return fileChunk{
			reader: file,
			close: func() error {
				if err := file.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
					return err
				}
				return os.Remove(file.Name())
			},
			size: n,
		}, true, nil
	}

	buffer := new(bytes.Buffer)
	n, err := io.Copy(buffer, io.LimitReader(p.r, size))
	if err != nil {
		return fileChunk{}, false, err
	}
	if n == 0 {
		return fileChunk{}, false, nil
	}
	return fileChunk{
		reader: bytes.NewReader(buffer.Bytes()),
		close:  func() error { return nil },
		size:   n,
	}, true, nil
}
