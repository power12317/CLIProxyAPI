package codexruntime

import (
	"bufio"
	"bytes"
	"io"
)

// BodyReader exposes the bridge's unparsed HTTP chunks as a normal response body.
type BodyReader struct {
	Client  *Client
	pending []byte
}

func (r *BodyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	for len(r.pending) == 0 {
		frame, err := r.Client.Next()
		if err != nil {
			return 0, err
		}
		r.pending = frame.Body
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}

// SSEFrame retains wire framing while exposing joined data fields to CPA's
// existing Responses processing and translators.
type SSEFrame struct {
	Wire []byte
	Data []byte
}

type SSEReader struct{ reader *bufio.Reader }

func NewSSEReader(body io.Reader) *SSEReader {
	return &SSEReader{reader: bufio.NewReader(body)}
}

func (r *SSEReader) Next() (SSEFrame, error) {
	var frame SSEFrame
	var data [][]byte
	for {
		line, err := r.reader.ReadBytes('\n')
		frame.Wire = append(frame.Wire, line...)
		line = bytes.TrimSuffix(line, []byte("\n"))
		line = bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(line, []byte("data:")) {
			data = append(data, bytes.TrimPrefix(line[5:], []byte(" ")))
		}
		if len(line) == 0 || err != nil {
			frame.Data = bytes.Join(data, []byte("\n"))
			if err == io.EOF && len(frame.Wire) != 0 {
				return frame, nil
			}
			return frame, err
		}
	}
}
