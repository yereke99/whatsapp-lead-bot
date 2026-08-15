package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

type stat struct {
	Keyword string `json:"keyword"`
}

// A list endpoint with nothing to return must answer [], not null. The
// frontend iterates these directly, so null is a broken page rather than an
// empty one.
func TestJSONEncodesEmptyListsAsArrays(t *testing.T) {
	tests := []struct {
		name string
		data any
		want string
	}{
		{"nil slice", []stat(nil), `{"data":[]}`},
		{"empty slice", []stat{}, `{"data":[]}`},
		{"populated slice", []stat{{Keyword: "АЙРАН"}}, `{"data":[{"keyword":"АЙРАН"}]}`},
		{"nil map", map[string]int(nil), `{"data":{}}`},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			JSON(rec, http.StatusOK, tc.data)

			got := trimNewline(rec.Body.String())
			if got != tc.want {
				t.Errorf("JSON body\n got: %s\nwant: %s", got, tc.want)
			}
		})
	}
}

func TestPagedEncodesEmptyListsAsArrays(t *testing.T) {
	rec := httptest.NewRecorder()
	Paged(rec, []stat(nil), 0, 25, 0)

	var body struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(body.Data) != "[]" {
		t.Errorf("data: got %s, want []", body.Data)
	}
}

// A nil pointer still means "absent", and null is the correct encoding for it.
func TestJSONLeavesNilPointersAsNull(t *testing.T) {
	rec := httptest.NewRecorder()
	JSON(rec, http.StatusOK, (*stat)(nil))

	if got := trimNewline(rec.Body.String()); got != `{"data":null}` {
		t.Errorf("got %s, want {\"data\":null}", got)
	}
}

func trimNewline(s string) string {
	if len(s) > 0 && s[len(s)-1] == '\n' {
		return s[:len(s)-1]
	}
	return s
}
