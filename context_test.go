package pdf

import (
	"bytes"
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// buildMinimalPDF constructs a minimal valid single-page PDF as a byte slice.
func buildMinimalPDF() []byte {
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /MediaBox [0 0 100 100] /Parent 2 0 R /Resources << >> >>",
	}

	var body bytes.Buffer
	body.WriteString("%PDF-1.0\r\n")
	offsets := make([]int, len(objs)+1) // 1-indexed
	for i, content := range objs {
		offsets[i+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", i+1, content)
	}

	xrefOffset := body.Len()
	n := len(objs) + 1
	fmt.Fprintf(&body, "xref\n0 %d\n", n)
	fmt.Fprintf(&body, "%010d 65535 f \r\n", 0)
	for i := 1; i < n; i++ {
		fmt.Fprintf(&body, "%010d 00000 n \r\n", offsets[i])
	}
	fmt.Fprintf(&body, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", n, xrefOffset)
	return body.Bytes()
}

// buildMultiPagePDF constructs a minimal valid n-page PDF as a byte slice.
func buildMultiPagePDF(n int) []byte {
	// Object layout:
	//   1: Catalog → 2
	//   2: Pages, Count=n, Kids=[3..n+2]
	//   3..n+2: individual Page objects
	kids := ""
	for i := 0; i < n; i++ {
		kids += fmt.Sprintf("%d 0 R ", i+3)
	}
	objs := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", kids, n),
	}
	for i := 0; i < n; i++ {
		objs = append(objs, "<< /Type /Page /MediaBox [0 0 100 100] /Parent 2 0 R /Resources << >> >>")
	}

	var body bytes.Buffer
	body.WriteString("%PDF-1.0\r\n")
	offsets := make([]int, len(objs)+1)
	for i, content := range objs {
		offsets[i+1] = body.Len()
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", i+1, content)
	}

	xrefOffset := body.Len()
	total := len(objs) + 1
	fmt.Fprintf(&body, "xref\n0 %d\n", total)
	fmt.Fprintf(&body, "%010d 65535 f \r\n", 0)
	for i := 1; i < total; i++ {
		fmt.Fprintf(&body, "%010d 00000 n \r\n", offsets[i])
	}
	fmt.Fprintf(&body, "trailer << /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", total, xrefOffset)
	return body.Bytes()
}

// triggerCtx is a context.Context whose Done() channel returns open for the
// first limit calls and a closed channel thereafter, allowing deterministic
// mid-loop cancellation tests.
type triggerCtx struct {
	mu     sync.Mutex
	limit  int
	polls  int
	closed chan struct{}
	once   sync.Once
}

func newTriggerCtx(limit int) *triggerCtx {
	return &triggerCtx{limit: limit, closed: make(chan struct{})}
}

func (c *triggerCtx) Done() <-chan struct{} {
	c.mu.Lock()
	c.polls++
	fired := c.polls > c.limit
	c.mu.Unlock()
	if fired {
		c.once.Do(func() { close(c.closed) })
		return c.closed
	}
	return make(chan struct{})
}

func (c *triggerCtx) Err() error {
	select {
	case <-c.closed:
		return context.Canceled
	default:
		return nil
	}
}

func (c *triggerCtx) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *triggerCtx) Value(_ any) any              { return nil }

func TestGetPlainText(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		data    []byte
		wantErr error
	}{
		{name: "pre_cancelled", ctx: cancelledCtx, data: buildMinimalPDF(), wantErr: context.Canceled},
		{name: "background", ctx: context.Background(), data: buildMinimalPDF()},
		{name: "midway_cancelled", ctx: newTriggerCtx(1), data: buildMultiPagePDF(3), wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			result, err := r.GetPlainText(tc.ctx)
			if err != tc.wantErr {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
			if tc.wantErr == nil && result == nil {
				t.Error("got nil reader")
			}
		})
	}
}

func TestGetStyledTexts(t *testing.T) {
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		data    []byte
		wantErr error
	}{
		{name: "pre_cancelled", ctx: cancelledCtx, data: buildMinimalPDF(), wantErr: context.Canceled},
		{name: "background", ctx: context.Background(), data: buildMinimalPDF()},
		{name: "midway_cancelled", ctx: newTriggerCtx(1), data: buildMultiPagePDF(3), wantErr: context.Canceled},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := NewReader(bytes.NewReader(tc.data), int64(len(tc.data)))
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			_, err = r.GetStyledTexts(tc.ctx)
			if err != tc.wantErr {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}
