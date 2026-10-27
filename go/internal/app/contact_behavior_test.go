package app

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

type contactAuditEvent struct {
	actor, action, resourceType, resourceID string
}

func thenContactAuditEvents(t *testing.T, world *bddWorld, want []contactAuditEvent) {
	t.Helper()
	rows, err := world.store.DB().QueryContext(t.Context(), `SELECT user_id, action, resource_type, resource_id FROM activity_log ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var got []contactAuditEvent
	for rows.Next() {
		var event contactAuditEvent
		if err := rows.Scan(&event.actor, &event.action, &event.resourceType, &event.resourceID); err != nil {
			t.Fatal(err)
		}
		got = append(got, event)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Then persisted audit events=%+v, want %+v", got, want)
	}
}

func TestBDDContactImportPersistsEveryWritableField(t *testing.T) {
	// Given a real client batch containing distinct values in every writable field.
	world := givenBDDApp(t)
	type input struct {
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Email     string `json:"email"`
		Phone     string `json:"phone"`
		Company   string `json:"company"`
	}
	want := []input{
		{"Jane, JJ", "O'Brien", "jane@example.com", "+15551234", "First Company"},
		{"悠", "山田", "yamada@example.com", "555-5678", "Second Company"},
	}
	body, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	response := world.request(t, http.MethodPost, "/api/contacts/import", "application/json", bytes.NewReader(body))
	thenStatus(t, response, http.StatusOK)
	if imported := decodeBDD[map[string]int](t, response)["imported"]; imported != len(want) {
		t.Fatalf("Then imported count=%d, want %d", imported, len(want))
	}
	// Then HTTP reads and actual SQLite rows preserve every field, ownership and
	// server-assigned identity; the response count alone cannot establish this.
	response = world.request(t, http.MethodGet, "/api/contacts", "", nil)
	thenStatus(t, response, http.StatusOK)
	page := decodeBDD[bddContactPage](t, response)
	if len(page.Contacts) != len(want) {
		t.Fatalf("Then list has %d records, want %d", len(page.Contacts), len(want))
	}
	seen := make(map[string]bool)
	remaining := make(map[string]input, len(want))
	for _, expected := range want {
		remaining[expected.Email] = expected
	}
	for _, contact := range page.Contacts {
		expected, exists := remaining[contact.Email]
		got := input{contact.FirstName, contact.LastName, contact.Email, contact.Phone, contact.Company}
		if !exists || got != expected || contact.ID == "" || seen[contact.ID] || contact.OwnerID != world.actor || contact.CreatedAt.IsZero() || !contact.CreatedAt.Equal(contact.UpdatedAt) {
			t.Fatalf("Then each imported contact preserves independent input values and metadata: %+v, expected %+v", contact, expected)
		}
		delete(remaining, contact.Email)
		seen[contact.ID] = true
		var persisted input
		var owner string
		if err := world.store.DB().QueryRowContext(t.Context(), `SELECT owner_id, first_name, last_name, email, phone, company FROM contacts WHERE id = ?`, contact.ID).
			Scan(&owner, &persisted.FirstName, &persisted.LastName, &persisted.Email, &persisted.Phone, &persisted.Company); err != nil {
			t.Fatal(err)
		}
		if owner != world.actor || persisted != expected {
			t.Fatalf("Then SQLite stores actor=%q contact=%+v, want %q/%+v", owner, persisted, world.actor, expected)
		}
	}
	thenContactAuditEvents(t, world, []contactAuditEvent{{world.actor, "import", "contacts", ""}})
}

func TestBDDInterruptedContactRequestsDoNotCommit(t *testing.T) {
	for _, scenario := range []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/api/contacts", `{"first_name":"Incomplete create"}`},
		{"replace", http.MethodPut, "/api/contacts/", `{"first_name":"Incomplete replacement"}`},
		{"import", http.MethodPost, "/api/contacts/import", `[{"first_name":"Complete first item"},`},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given a persisted contact and an authenticated client sending a body
			// that ends before its declared Content-Length, as on an interrupted upload.
			finished := make(chan struct{})
			world := givenBDDApp(t, func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("X-Test-Interrupted") == "true" {
						defer close(finished)
					}
					next.ServeHTTP(w, r)
				})
			})
			created := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Keep","email":"keep@example.com"}`))
			thenStatus(t, created, http.StatusCreated)
			original := thenContactContract(t, created, world.actor)
			path := scenario.path
			if strings.HasSuffix(path, "/") {
				path += original.ID
			}
			address, err := url.Parse(world.url)
			if err != nil {
				t.Fatal(err)
			}
			dialer := net.Dialer{Timeout: 5 * time.Second}
			connection, err := dialer.DialContext(t.Context(), "tcp", address.Host)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := connection.Close(); err != nil {
					t.Error(err)
				}
			}()
			if err := connection.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if _, err := fmt.Fprintf(connection, "%s %s HTTP/1.1\r\nHost: %s\r\nAuthorization: Bearer %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\nX-Test-Interrupted: true\r\n\r\n%s", scenario.method, path, address.Host, world.token, len(scenario.body)+64, scenario.body); err != nil {
				t.Fatal(err)
			}
			// A half-close delivers a real unexpected EOF to net/http while still
			// allowing the client to observe the server's completed error response.
			tcp, ok := connection.(*net.TCPConn)
			if !ok {
				t.Fatalf("expected a TCP connection, got %T", connection)
			}
			if err := tcp.CloseWrite(); err != nil {
				t.Fatal(err)
			}
			response, err := http.ReadResponse(bufio.NewReader(connection), nil)
			if err != nil {
				t.Fatal(err)
			}
			body, readErr := io.ReadAll(response.Body)
			if closeErr := response.Body.Close(); readErr != nil || closeErr != nil {
				t.Fatalf("read interrupted-request response: %v; close: %v", readErr, closeErr)
			}
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("Then interrupted body status=%d, want 400: %s", response.StatusCode, body)
			}
			// Wait for the actual handler to return before asserting no side effects,
			// independently of HTTP response buffering. There are no sleeps or polls.
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Fatal("Then interrupted request handling must finish")
			}
			read := world.request(t, http.MethodGet, "/api/contacts/"+original.ID, "", nil)
			thenStatus(t, read, http.StatusOK)
			if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, original) {
				t.Fatalf("Then interrupted requests preserve the existing record: %+v", got)
			}
			thenBDDActorStateCounts(t, world, 1, 1)
			thenContactAuditEvents(t, world, []contactAuditEvent{{world.actor, "create", "contact", original.ID}})
		})
	}
}
