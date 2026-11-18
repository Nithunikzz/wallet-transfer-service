package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"
)

func TestRequestIDAttachedFromContext(t *testing.T) {
	var buf bytes.Buffer
	log := NewLogger(slog.NewJSONHandler(&buf, nil)).With("component", "svc")

	log.InfoContext(WithRequestID(context.Background(), "req-1"), "transfer processed", "amount", 100)

	var rec map[string]any
	if err := json.Unmarshal(buf.Bytes(), &rec); err != nil {
		t.Fatal(err)
	}
	if rec["request_id"] != "req-1" || rec["component"] != "svc" || rec["amount"] != float64(100) {
		t.Fatalf("unexpected record: %v", rec)
	}

	buf.Reset()
	log.InfoContext(context.Background(), "no id")
	if bytes.Contains(buf.Bytes(), []byte("request_id")) {
		t.Fatalf("request_id must be absent without one in context: %s", buf.String())
	}
}
