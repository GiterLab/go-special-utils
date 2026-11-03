package tuscore

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// ServeContent implements handler.ServableUpload. It mirrors upstream s3store:
// the provider performs the object read (including Range and conditional
// headers) and the response headers are copied from the provider response.
func (u *Upload) ServeContent(ctx context.Context, w http.ResponseWriter, r *http.Request) error {
	opts := GetOptions{
		Range:       r.Header.Get("Range"),
		IfMatch:     r.Header.Get("If-Match"),
		IfNoneMatch: r.Header.Get("If-None-Match"),
	}
	if value := r.Header.Get("If-Modified-Since"); value != "" {
		if parsed, err := http.ParseTime(value); err == nil {
			opts.IfModifiedSince = parsed
		}
	}
	if value := r.Header.Get("If-Unmodified-Since"); value != "" {
		if parsed, err := http.ParseTime(value); err == nil {
			opts.IfUnmodifiedSince = parsed
		}
	}

	object, err := u.store.Bucket.GetObject(ctx, u.key(), opts)
	if err != nil {
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Disposition")
		switch u.store.Bucket.StatusCode(err) {
		case http.StatusNotFound, http.StatusForbidden:
			// The object is missing, so the upload has not been finished yet.
			return errIncompleteUpload
		case http.StatusNotModified:
			w.WriteHeader(http.StatusNotModified)
			return nil
		case http.StatusRequestedRangeNotSatisfiable:
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return nil
		}
		return err
	}
	defer object.Body.Close()

	acceptRanges := object.AcceptRanges
	if acceptRanges == "" {
		acceptRanges = "bytes"
	}
	w.Header().Set("Accept-Ranges", acceptRanges)
	setHeader(w, "Content-Disposition", object.ContentDisposition)
	setHeader(w, "Content-Encoding", object.ContentEncoding)
	setHeader(w, "Content-Language", object.ContentLanguage)
	setHeader(w, "Content-Type", object.ContentType)
	setHeader(w, "Cache-Control", object.CacheControl)
	setHeader(w, "ETag", object.ETag)
	setHeader(w, "Expires", object.Expires)
	if !object.LastModified.IsZero() {
		w.Header().Set("Last-Modified", object.LastModified.Format(http.TimeFormat))
	}

	status := http.StatusOK
	contentLength := object.Size
	if opts.Range != "" && object.Size >= 0 {
		start, end, contentRange, ok := parseRange(opts.Range, object.Size)
		if !ok {
			w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
			return nil
		}
		if object.ContentRange == "" {
			object.ContentRange = contentRange
		}
		w.Header().Set("Content-Range", object.ContentRange)
		contentLength = end - start + 1
		status = http.StatusPartialContent
	} else if object.Size == 0 {
		status = http.StatusNoContent
	}
	if contentLength >= 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(contentLength, 10))
	}

	w.WriteHeader(status)
	if status == http.StatusNoContent {
		return nil
	}
	_, err = io.Copy(w, object.Body)
	return err
}

func setHeader(w http.ResponseWriter, key, value string) {
	if value != "" {
		w.Header().Set(key, value)
	}
}

// parseRange understands the single-range form of the Range header.
func parseRange(header string, size int64) (start, end int64, contentRange string, ok bool) {
	value := strings.TrimSpace(header)
	if !strings.HasPrefix(value, "bytes=") {
		return 0, 0, "", false
	}
	spec := strings.TrimSpace(strings.TrimPrefix(value, "bytes="))
	if strings.Contains(spec, ",") {
		return 0, 0, "", false
	}
	first, last, found := strings.Cut(spec, "-")
	if !found {
		return 0, 0, "", false
	}
	switch {
	case strings.TrimSpace(first) == "":
		suffix, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
		if err != nil || suffix <= 0 {
			return 0, 0, "", false
		}
		if suffix > size {
			suffix = size
		}
		start, end = size-suffix, size-1
	case strings.TrimSpace(last) == "":
		parsed, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
		if err != nil || parsed < 0 || parsed >= size {
			return 0, 0, "", false
		}
		start, end = parsed, size-1
	default:
		parsedStart, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
		if err != nil || parsedStart < 0 || parsedStart >= size {
			return 0, 0, "", false
		}
		parsedEnd, err := strconv.ParseInt(strings.TrimSpace(last), 10, 64)
		if err != nil || parsedEnd < parsedStart {
			return 0, 0, "", false
		}
		if parsedEnd >= size {
			parsedEnd = size - 1
		}
		start, end = parsedStart, parsedEnd
	}
	return start, end, "bytes " + strconv.FormatInt(start, 10) + "-" + strconv.FormatInt(end, 10) + "/" + strconv.FormatInt(size, 10), true
}
