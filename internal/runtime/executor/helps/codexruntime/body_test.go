package codexruntime

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

func TestSSEReaderPreservesSplitUTF8AndMultilineData(t *testing.T) {
	wire := ": comment\r\nevent: response.future\r\nid: arbitrary\r\ndata: {\r\ndata: \"type\":\"response.future\",\"text\":\"你好\"}\r\n\r\n: partial final line"
	r := NewSSEReader(iotest.OneByteReader(bytes.NewBufferString(wire)))
	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if string(first.Data) != "{\n\"type\":\"response.future\",\"text\":\"你好\"}" {
		t.Fatalf("joined event = %q", first.Data)
	}
	last, err := r.Next()
	if err != nil || string(last.Wire) != ": partial final line" || len(last.Data) != 0 {
		t.Fatalf("final frame = %#v, %v", last, err)
	}
	if string(append(first.Wire, last.Wire...)) != wire {
		t.Fatal("wire framing changed")
	}
	if _, err := r.Next(); err != io.EOF {
		t.Fatalf("EOF = %v", err)
	}
}
