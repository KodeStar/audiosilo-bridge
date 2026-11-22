// Package progress wraps an io.Reader to report byte progress to a callback,
// throttled so a large copy doesn't flood the caller (a job's progress
// updates). The Audible download reads through Reader; the file transfer
// copies in EmitEvery chunks, so both report at the same cadence.
package progress

import "io"

// EmitEvery caps callbacks to at most one per this many bytes (plus a guaranteed
// final callback), so a multi-hundred-MB copy reports a few hundred times instead of
// once per ~32 KiB chunk.
const EmitEvery = 1 << 20

// Reader counts bytes read from R and reports (copied, total) to CB, throttled. Total
// is the expected size, or -1 when unknown; a final callback still fires at EOF (or on
// a read error). CB must be non-nil.
type Reader struct {
	R        io.Reader
	Total    int64
	CB       func(copied, total int64)
	read     int64
	lastEmit int64
}

func (r *Reader) Read(b []byte) (int, error) {
	n, err := r.R.Read(b)
	r.read += int64(n)
	if r.read-r.lastEmit >= EmitEvery || err != nil {
		r.lastEmit = r.read
		r.CB(r.read, r.Total)
	}
	return n, err
}
