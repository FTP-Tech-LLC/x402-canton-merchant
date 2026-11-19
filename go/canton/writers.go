package canton

import (
	"bytes"
	"net/http"
)

// bufferWriter is a ResponseWriter that keeps status, headers and body in
// memory. The first final status wins, as with net/http.
type bufferWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func newBufferWriter() *bufferWriter {
	return &bufferWriter{header: make(http.Header)}
}

func (w *bufferWriter) Header() http.Header { return w.header }

func (w *bufferWriter) WriteHeader(code int) {
	// 1xx answers are informational and not final; a status is set once.
	if w.status != 0 || (code >= http.StatusContinue && code < http.StatusOK) {
		return
	}
	w.status = code
}

func (w *bufferWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

// finalStatus is the status the response will carry: 200 when nothing was written.
func (w *bufferWriter) finalStatus() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func copyHeader(dst, src http.Header) {
	for name, values := range src {
		dst[name] = append([]string(nil), values...)
	}
}
