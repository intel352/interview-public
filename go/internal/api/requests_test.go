package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeContactRejectsInvalidRequests(t *testing.T) {
	for _, tt := range []struct {
		name, body, contentType string
		status                  int
	}{
		{"missing media type", `{}`, "", 415},
		{"wrong media type", `{}`, "text/plain", 415},
		{"malformed", `{`, "application/json", 400},
		{"null", `null`, "application/json", 400},
		{"array", `[]`, "application/json", 400},
		{"unknown field", `{"unexpected":true}`, "application/json", 400},
		{"server field", `{"id":"client-id"}`, "application/json", 400},
		{"trailing object", `{} {}`, "application/json", 400},
		{"trailing garbage", `{} trailing`, "application/json", 400},
		{"long name", fmt.Sprintf(`{"first_name":%q}`, strings.Repeat("a", 257)), "application/json", 400},
		{"control character", `{"first_name":"bad\nname"}`, "application/json", 400},
		{"invalid email", `{"email":"not-an-address"}`, "application/json", 400},
		{"display address", `{"email":"Alice <alice@example.com>"}`, "application/json", 400},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", tt.contentType)
			w := httptest.NewRecorder()
			if _, ok := decodeContact(w, r); ok || w.Code != tt.status {
				t.Fatalf("accepted=%v status=%d want %d body=%s", ok, w.Code, tt.status, w.Body.String())
			}
		})
	}
}

func TestDecodeContactRequiresExactUniqueStringFields(t *testing.T) {
	for _, field := range []string{"first_name", "last_name", "email", "phone", "company"} {
		for _, scenario := range []struct{ name, body string }{
			{"null", fmt.Sprintf(`{%q:null}`, field)},
			{"duplicate", fmt.Sprintf(`{%q:"",%q:""}`, field, field)},
			{"case_mismatch", fmt.Sprintf(`{%q:""}`, strings.ToUpper(field))},
		} {
			t.Run(field+"_"+scenario.name, func(t *testing.T) {
				for _, imported := range []bool{false, true} {
					body := scenario.body
					if imported {
						body = "[" + body + "]"
					}
					r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
					r.Header.Set("Content-Type", "application/json")
					w := httptest.NewRecorder()
					var accepted bool
					if imported {
						_, accepted = decodeContactImport(w, r)
					} else {
						_, accepted = decodeContact(w, r)
					}
					if accepted || w.Code != http.StatusBadRequest {
						t.Errorf("import=%v accepted=%v status=%d; want rejected400", imported, accepted, w.Code)
					}
				}
			})
		}
	}
	for _, body := range []string{
		`{"first_name":"one","first\u005fname":"two"}`,
		`{"first_name":false}`,
		`{"first_name":42}`,
		`{"first_name":[]}`,
		`{"first_name":{}}`,
		`{"owner_id":"other@example.com"}`,
	} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		if _, accepted := decodeContact(w, r); accepted || w.Code != http.StatusBadRequest {
			t.Errorf("body=%s accepted=%v status=%d; want rejected400", body, accepted, w.Code)
		}
	}
}

func TestDecodeContactExactBodyAndFieldLimits(t *testing.T) {
	body := fmt.Sprintf(`{"first_name":%q,"last_name":%q,"phone":%q,"company":%q}`, strings.Repeat("界", 256), strings.Repeat("x", 256), strings.Repeat("1", 64), strings.Repeat("y", 256))
	for _, extra := range []int{0, 1} {
		payload := body + strings.Repeat(" ", maxContactBody-len(body)+extra)
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(payload))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		// Exercise the streaming bound rather than only Content-Length rejection.
		r.ContentLength = -1
		w := httptest.NewRecorder()
		_, ok := decodeContact(w, r)
		if extra == 0 && !ok {
			t.Fatalf("exact limit rejected: %d %s", w.Code, w.Body.String())
		}
		if extra == 1 && (ok || w.Code != http.StatusRequestEntityTooLarge) {
			t.Fatalf("over-limit accepted=%v status=%d", ok, w.Code)
		}
	}
}

func TestDecodeImportLimitsAndShape(t *testing.T) {
	for _, tt := range []struct {
		name, body string
		status     int
		count      int
	}{
		{"empty", `[]`, 200, 0},
		{"exact count", "[" + strings.TrimSuffix(strings.Repeat("{},", maxImportContacts), ",") + "]", 200, maxImportContacts},
		{"too many", "[" + strings.TrimSuffix(strings.Repeat("{},", maxImportContacts+1), ",") + "]", 400, 0},
		{"not array", `{}`, 400, 0},
		{"null", `null`, 400, 0},
		{"null entry", `[null]`, 400, 0},
		{"unknown entry field", `[{"extra":1}]`, 400, 0},
		{"incomplete", `[{},`, 400, 0},
		{"trailing", `[] []`, 400, 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			contacts, ok := decodeContactImport(w, r)
			if tt.status == 200 {
				if !ok || len(contacts) != tt.count {
					t.Fatalf("accepted=%v count=%d want %d: %s", ok, len(contacts), tt.count, w.Body.String())
				}
			} else if ok || w.Code != tt.status {
				t.Fatalf("accepted=%v status=%d want %d: %s", ok, w.Code, tt.status, w.Body.String())
			}
		})
	}
}

func TestDecodeImportBodyLimit(t *testing.T) {
	for _, extra := range []int{0, 1} {
		r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("[]"+strings.Repeat(" ", maxImportBody-2+extra)))
		r.ContentLength = -1
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		_, ok := decodeContactImport(w, r)
		if extra == 0 && !ok || extra == 1 && (ok || w.Code != 413) {
			t.Fatalf("extra=%d accepted=%v status=%d", extra, ok, w.Code)
		}
	}
}

func TestCSVCellNeutralizesFormulaPrefixes(t *testing.T) {
	for _, value := range []string{"=1+1", "+1", "-1", "@SUM(A1)", " =1+1", "\t=1+1", "\r=1+1", "\n=1+1", "\u2003=1+1"} {
		if got := spreadsheetCell(value); got != "'"+value {
			t.Errorf("%q became %q, want apostrophe prefix", value, got)
		}
	}
	for _, value := range []string{"plain", "O'Brien", "  ordinary", "", "123"} {
		if got := spreadsheetCell(value); got != value {
			t.Errorf("safe %q became %q", value, got)
		}
	}
}
