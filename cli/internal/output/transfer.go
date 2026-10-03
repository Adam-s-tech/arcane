package output

import (
	"context"
	"io"
)

// Copy copies a transfer while reporting bytes, or activity for unknown totals.
func Copy(ctx context.Context, dst io.Writer, src io.Reader, total int64, label string, enabled bool) (int64, error) {
	loading := StartTracker(ctx, Stderr(), enabled, label)
	loading.NextStep()
	defer loading.Stop()
	reader := &transferReader{ctx: ctx, source: src, loading: loading, total: total, label: label}
	loading.SetProgress("", label, 0, total, true)
	written, err := io.Copy(dst, reader)
	loading.Finish("", err)
	return written, err
}

type transferReader struct {
	ctx            context.Context
	source         io.Reader
	loading        *tracker
	current, total int64
	label          string
}

func (r *transferReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.source.Read(p)
	r.current += int64(n)
	r.loading.SetProgress("", r.label, r.current, r.total, true)
	return n, err
}
