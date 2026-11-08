package app

import (
	"io"
	"strings"
	"testing"
)

func TestLimitedBufferCopyCannotBypassLimit(t *testing.T) {
	for _, size := range []int{65536, 65537, 1000000} {
		var output limitedBuffer
		// Hide the source's WriterTo so io.Copy would select a destination
		// ReaderFrom if the buffer accidentally exposed that fast path again.
		source := struct{ io.Reader }{strings.NewReader(strings.Repeat("x", size))}
		_, err := io.Copy(&output, source)
		if (err != nil) != (size > 65536) || output.Len() > 65536 {
			t.Fatalf("size=%d retained=%d error=%v", size, output.Len(), err)
		}
		if size == 65536 && len(output.Bytes()) != size {
			t.Fatal("truncated output within limit")
		}
	}
}
