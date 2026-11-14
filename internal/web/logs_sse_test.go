package web

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kingzion24/ozymandis/internal/orchestrator"
)

// A lone \r ends a line in the event-stream format, so a log line carrying one
// must not be able to start a field of its own.
func TestWriteLogEventCarriageReturn(t *testing.T) {
	var buf bytes.Buffer
	writeLogEvent(&buf, orchestrator.LogLine{Text: "progress\rid: 1\r\revent: forged"})

	for _, line := range strings.FieldsFunc(buf.String(), func(r rune) bool {
		return r == '\n' || r == '\r'
	}) {
		if !strings.HasPrefix(line, "data: ") && !strings.HasPrefix(line, "id: ") {
			t.Errorf("line %q is not a data or id field", line)
		}
	}
	if strings.Contains(buf.String(), "\r") {
		t.Errorf("a carriage return reached the stream: %q", buf.String())
	}
	if strings.Count("\n"+buf.String(), "\nid: ") != 1 {
		t.Errorf("want exactly one id field, got %q", buf.String())
	}
}
