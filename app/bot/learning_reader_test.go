package bot

import (
	"errors"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/require"
)

type learningReadCloser struct {
	io.Reader
	closed   bool
	closeErr error
}

func (r *learningReadCloser) Close() error { r.closed = true; return r.closeErr }

func TestLearningBufferReaderFailureClosesConnection(t *testing.T) {
	failure := errors.New("reader failed")
	for _, closeFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "read error", true: "close error"}[closeFailure], func(t *testing.T) {
			r := &learningReadCloser{Reader: iotest.ErrReader(failure)}
			if closeFailure {
				r.Reader = strings.NewReader("sample")
				r.closeErr = failure
			}
			buffer, err := bufferSampleReader(r, nil)
			require.ErrorIs(t, err, failure)
			require.Nil(t, buffer)
			require.True(t, r.closed)
		})
	}
}
