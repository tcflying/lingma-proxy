package httpapi

import (
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestB4Encode1005UnencodablePayloadIsExplicitError: writeJSON used to commit
// the status header first and then ignore the encoder's error, so a payload
// with a non-finite number answered 200 with an empty body -- a success the
// client cannot distinguish from a real one. Serialization must happen before
// the status is committed, and a failure must be an explicit error response.
func TestB4Encode1005UnencodablePayloadIsExplicitError(t *testing.T) {
	rec := httptest.NewRecorder()
	writeJSON(rec, 200, map[string]any{"delta": map[string]any{"partial_json": math.NaN()}})
	body := rec.Body.String()
	if rec.Code == 200 && rec.Body.Len() == 0 {
		t.Fatalf("committed 200 with empty body for an unencodable payload")
	}
	if rec.Code == 200 {
		t.Fatalf("unencodable payload must not be a 200, got %d body=%q", rec.Code, body)
	}
	if rec.Body.Len() == 0 || !strings.Contains(body, "error") {
		t.Fatalf("failure must carry an explicit error body, code=%d body=%q", rec.Code, body)
	}

	ok := httptest.NewRecorder()
	writeJSON(ok, 200, map[string]any{"delta": map[string]any{"partial_json": "1.5"}})
	if ok.Code != 200 || ok.Body.Len() == 0 {
		t.Fatalf("encodable payload must still encode, code=%d body=%q", ok.Code, ok.Body.String())
	}
}
