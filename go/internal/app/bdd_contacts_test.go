package app

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/weave-lab/interview-public/go/internal/store"
)

type bddContactPage struct {
	Contacts      []store.Contact `json:"contacts"`
	NextPageToken string          `json:"next_page_token"`
}

func thenContactContract(t *testing.T, response bddResponse, actor string) store.Contact {
	t.Helper()
	if response.header.Get("Content-Type") != "application/json" || response.header.Get("Cache-Control") != "no-store" {
		t.Fatalf("Then contact responses must be uncached JSON: %v", response.header)
	}
	fields := decodeBDD[map[string]json.RawMessage](t, response)
	want := []string{"id", "owner_id", "first_name", "last_name", "email", "phone", "company", "created_at", "updated_at"}
	if len(fields) != len(want) {
		t.Fatalf("Then contact must have exactly %d contract fields, got %v", len(want), fields)
	}
	for _, field := range want {
		var value any
		if raw, ok := fields[field]; !ok || json.Unmarshal(raw, &value) != nil {
			t.Fatalf("Then contact field %q must be a JSON string", field)
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("Then contact field %q must be a JSON string, got %T", field, value)
		}
	}
	contact := decodeBDD[store.Contact](t, response)
	if contact.ID == "" || contact.OwnerID != actor || contact.CreatedAt.IsZero() || contact.UpdatedAt.IsZero() {
		t.Fatalf("Then the server must assign identity and timestamps: %+v", contact)
	}
	return contact
}

func thenContactAuditCount(t *testing.T, world *bddWorld, want int) {
	t.Helper()
	var total int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM activity_log WHERE user_id = ?`, world.actor).Scan(&total); err != nil {
		t.Fatal(err)
	}
	if total != want {
		t.Fatalf("Then actor %s audit total=%d, want %d", world.actor, total, want)
	}
}

func TestBDDContactsCRUD(t *testing.T) {
	// Given an empty actor workspace and an authenticated API consumer.
	world := givenBDDApp(t)
	empty := world.request(t, http.MethodGet, "/api/contacts", "", nil)
	thenStatus(t, empty, http.StatusOK)
	page := decodeBDD[bddContactPage](t, empty)
	if len(page.Contacts) != 0 || page.NextPageToken != "" {
		t.Fatalf("Given an empty workspace, listing returned %+v", page)
	}
	// Empty collections have a stable array representation for API clients.
	if fields := decodeBDD[map[string]json.RawMessage](t, empty); string(fields["contacts"]) != "[]" {
		t.Fatalf("Then the empty contacts collection must be a JSON array: %s", empty.body)
	}

	// When a consumer creates a contact with every writable field.
	createdResponse := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Jane","last_name":"Doe","email":"jane@example.com","phone":"5551234","company":"Example"}`))
	thenStatus(t, createdResponse, http.StatusCreated)
	created := thenContactContract(t, createdResponse, world.actor)
	if created.FirstName != "Jane" || created.LastName != "Doe" || created.Email != "jane@example.com" || created.Phone != "5551234" || created.Company != "Example" || !created.CreatedAt.Equal(created.UpdatedAt) {
		t.Fatalf("Then the created contact must preserve fields and initial timestamps: %+v", created)
	}
	path := "/api/contacts/" + created.ID

	// Then GET returns the same resource and PUT replaces its writable fields.
	read := world.request(t, http.MethodGet, path, "", nil)
	thenStatus(t, read, http.StatusOK)
	if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, created) {
		t.Fatalf("Then read contact=%+v, want %+v", got, created)
	}
	updatedResponse := world.request(t, http.MethodPut, path, "application/json", strings.NewReader(`{"first_name":"Janet"}`))
	thenStatus(t, updatedResponse, http.StatusOK)
	updated := thenContactContract(t, updatedResponse, world.actor)
	if updated.ID != created.ID || updated.FirstName != "Janet" || updated.LastName != "" || updated.Email != "" || updated.Phone != "" || updated.Company != "" || !updated.CreatedAt.Equal(created.CreatedAt) || updated.UpdatedAt.Before(created.UpdatedAt) {
		t.Fatalf("Then replacement must preserve identity/creation time and clear omitted fields: %+v", updated)
	}
	read = world.request(t, http.MethodGet, path, "", nil)
	thenStatus(t, read, http.StatusOK)
	if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, updated) {
		t.Fatalf("Then replacement must persist: %+v, want %+v", got, updated)
	}

	// When the contact is deleted, then it disappears and 204 has no body.
	deleted := world.request(t, http.MethodDelete, path, "", nil)
	thenStatus(t, deleted, http.StatusNoContent)
	if len(deleted.body) != 0 {
		t.Fatalf("Then deletion must have no response body: %q", deleted.body)
	}
	for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
		t.Run("missing_contact_"+method, func(t *testing.T) {
			response := world.request(t, method, path, "application/json", strings.NewReader(`{}`))
			thenStatus(t, response, http.StatusNotFound)
			if errorBody := decodeBDD[map[string]string](t, response); errorBody["error"] != "contact not found" {
				t.Fatalf("Then a missing resource must return the JSON error contract: %s", response.body)
			}
		})
	}
	thenContactAuditCount(t, world, 3)
	thenContactAuditEvents(t, world, []contactAuditEvent{
		{world.actor, "create", "contact", created.ID},
		{world.actor, "update", "contact", created.ID},
		{world.actor, "delete", "contact", created.ID},
	})
}

func TestBDDContactOptionalFieldsMayBeEmpty(t *testing.T) {
	// Given a fresh workspace, when an empty contact object is submitted.
	world := givenBDDApp(t)
	response := world.request(t, http.MethodPost, "/api/contacts", "application/json; charset=utf-8", strings.NewReader(`{}`))
	thenStatus(t, response, http.StatusCreated)
	contact := thenContactContract(t, response, world.actor)
	// Then optional values remain empty while identity/timestamps are assigned.
	if contact.FirstName != "" || contact.LastName != "" || contact.Email != "" || contact.Phone != "" || contact.Company != "" {
		t.Fatalf("Then optional fields must remain empty: %+v", contact)
	}
	thenContactAuditCount(t, world, 1)
}

func TestBDDContactPaginationWithTiedTimestamps(t *testing.T) {
	// Given five contacts imported together, with one shared creation timestamp.
	world := givenBDDApp(t)
	var contacts []store.Contact
	for _, id := range []string{"a", "b", "c", "d", "e"} {
		contacts = append(contacts, store.Contact{ID: id, FirstName: "Fixture " + id})
	}
	if _, err := world.store.ImportContacts(t.Context(), world.actor, contacts); err != nil {
		t.Fatal(err)
	}

	// When the consumer follows opaque cursors one record at a time.
	path := "/api/contacts?limit=1"
	var seen []string
	for pageIndex, wantIDs := range [][]string{{"e"}, {"d"}, {"c"}, {"b"}, {"a"}} {
		response := world.request(t, http.MethodGet, path, "", nil)
		thenStatus(t, response, http.StatusOK)
		page := decodeBDD[bddContactPage](t, response)
		var ids []string
		for _, contact := range page.Contacts {
			ids = append(ids, contact.ID)
		}
		// Then ties use descending IDs and every record appears exactly once.
		if !slices.Equal(ids, wantIDs) {
			t.Fatalf("Then page %d IDs=%v, want %v", pageIndex+1, ids, wantIDs)
		}
		seen = append(seen, ids...)
		if pageIndex < 4 {
			if page.NextPageToken == "" {
				t.Fatal("Then an unfinished page must include the next cursor")
			}
			path = "/api/contacts?limit=1&page_token=" + url.QueryEscape(page.NextPageToken)
		} else if _, exists := decodeBDD[map[string]json.RawMessage](t, response)["next_page_token"]; exists {
			t.Fatal("Then the final page must omit next_page_token")
		}
	}
	if !slices.Equal(seen, []string{"e", "d", "c", "b", "a"}) {
		t.Fatalf("Then the complete cursor walk must neither skip nor duplicate: %v", seen)
	}
	thenContactAuditCount(t, world, 1) // Reads do not create mutation events.
}

func TestBDDContactPaginationPageSizes(t *testing.T) {
	// Given more contacts than either the default or maximum page size.
	world := givenBDDApp(t)
	contacts := make([]store.Contact, 101)
	for i := range contacts {
		contacts[i] = store.Contact{ID: fmt.Sprintf("page-%03d", i)}
	}
	if _, err := world.store.ImportContacts(t.Context(), world.actor, contacts); err != nil {
		t.Fatal(err)
	}
	for _, scenario := range []struct {
		name, query string
		want        int
	}{
		{"default_fifty", "", 50},
		{"minimum_one", "?limit=1", 1},
		{"maximum_one_hundred", "?limit=100", 100},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// When the consumer chooses a supported page size.
			response := world.request(t, http.MethodGet, "/api/contacts"+scenario.query, "", nil)
			thenStatus(t, response, http.StatusOK)
			page := decodeBDD[bddContactPage](t, response)
			// Then the response is bounded and advertises the remaining records.
			if len(page.Contacts) != scenario.want || page.NextPageToken == "" {
				t.Fatalf("Then page has %d contacts/cursor %q, want %d/nonempty", len(page.Contacts), page.NextPageToken, scenario.want)
			}
		})
	}
}

func TestBDDContactImports(t *testing.T) {
	for _, scenario := range []struct {
		name, body string
		status     int
		imported   int
		audits     int
	}{
		{"empty_batch_is_a_successful_attempt", `[]`, http.StatusOK, 0, 1},
		{"valid_batch_commits_together", `[{"first_name":"One"},{"first_name":"Two"}]`, http.StatusOK, 2, 1},
		{"invalid_later_item_does_not_partially_import", `[{"first_name":"One"},{"email":"invalid"}]`, http.StatusBadRequest, 0, 0},
		{"trailing_value_does_not_partially_import", `[{"first_name":"One"}] {}`, http.StatusBadRequest, 0, 0},
		{"null_field_does_not_partially_import", `[{"first_name":"One"},{"company":null}]`, http.StatusBadRequest, 0, 0},
		{"duplicate_field_does_not_partially_import", `[{"first_name":"One"},{"phone":"1","phone":"2"}]`, http.StatusBadRequest, 0, 0},
		{"case_mismatch_does_not_partially_import", `[{"first_name":"One"},{"Email":"two@example.com"}]`, http.StatusBadRequest, 0, 0},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given an empty workspace, when one batch is submitted.
			world := givenBDDApp(t)
			response := world.request(t, http.MethodPost, "/api/contacts/import", "application/json", strings.NewReader(scenario.body))
			thenStatus(t, response, scenario.status)
			// Then success reports its committed count and invalid batches write nothing.
			if scenario.status == http.StatusOK {
				result := decodeBDD[map[string]int](t, response)
				if len(result) != 1 || result["imported"] != scenario.imported {
					t.Fatalf("Then import response=%s, want imported=%d", response.body, scenario.imported)
				}
			}
			count, err := world.store.CountContacts(t.Context(), world.actor)
			if err != nil || count != scenario.imported {
				t.Fatalf("Then persisted contacts=%d, want %d: %v", count, scenario.imported, err)
			}
			thenContactAuditCount(t, world, scenario.audits)
		})
	}
}

func TestBDDContactValidationPreservesState(t *testing.T) {
	// Given a valid persisted contact before rejected mutations.
	world := givenBDDApp(t)
	response := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Keep","email":"keep@example.com"}`))
	thenStatus(t, response, http.StatusCreated)
	original := thenContactContract(t, response, world.actor)
	for _, method := range []string{http.MethodPost, http.MethodPut} {
		for _, scenario := range []struct {
			name, body, mediaType string
			status                int
		}{
			{"malformed_json", `{`, "application/json", http.StatusBadRequest},
			{"server_owned_field", `{"id":"client-chosen"}`, "application/json", http.StatusBadRequest},
			{"invalid_email", `{"email":"not-an-email"}`, "application/json", http.StatusBadRequest},
			{"null_object", `null`, "application/json", http.StatusBadRequest},
			{"null_field", `{"first_name":null}`, "application/json", http.StatusBadRequest},
			{"duplicate_field", `{"first_name":"One","first_name":"Two"}`, "application/json", http.StatusBadRequest},
			{"case_mismatch", `{"First_Name":"One"}`, "application/json", http.StatusBadRequest},
			{"server_owned_owner", `{"owner_id":"other@example.com"}`, "application/json", http.StatusBadRequest},
			{"unsupported_media", `{}`, "text/plain", http.StatusUnsupportedMediaType},
		} {
			t.Run(method+"_"+scenario.name, func(t *testing.T) {
				// When create or replacement input violates the public contract.
				path := "/api/contacts"
				if method == http.MethodPut {
					path += "/" + original.ID
				}
				rejected := world.request(t, method, path, scenario.mediaType, strings.NewReader(scenario.body))
				thenStatus(t, rejected, scenario.status)
				if rejected.header.Get("Content-Type") != "application/json" || decodeBDD[map[string]string](t, rejected)["error"] == "" {
					t.Fatalf("Then rejection must provide a JSON error: %s", rejected.body)
				}
				// Then existing state and audit history remain unchanged.
				read := world.request(t, http.MethodGet, "/api/contacts/"+original.ID, "", nil)
				thenStatus(t, read, http.StatusOK)
				if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, original) {
					t.Fatalf("Then rejected input must not change stored contact: %+v", got)
				}
				if count, err := world.store.CountContacts(t.Context(), world.actor); err != nil || count != 1 {
					t.Fatalf("Then rejected input must not add records: count=%d error=%v", count, err)
				}
				thenContactAuditCount(t, world, 1)
			})
		}
	}
}

func TestBDDContactAuditFailuresRollbackEveryMutation(t *testing.T) {
	for _, scenario := range []struct {
		name, method, path, body string
		status, count            int
	}{
		{"create", http.MethodPost, "/api/contacts", `{"first_name":"New"}`, http.StatusCreated, 2},
		{"update", http.MethodPut, "/api/contacts/", `{"first_name":"Changed"}`, http.StatusOK, 1},
		{"delete", http.MethodDelete, "/api/contacts/", "", http.StatusNoContent, 0},
		{"import", http.MethodPost, "/api/contacts/import", `[{},{}]`, http.StatusOK, 3},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Given an existing contact and a database that refuses audit writes.
			world := givenBDDApp(t)
			created := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Original"}`))
			thenStatus(t, created, http.StatusCreated)
			original := thenContactContract(t, created, world.actor)
			if _, err := world.store.DB().ExecContext(t.Context(), `CREATE TRIGGER reject_bdd_audit BEFORE INSERT ON activity_log BEGIN SELECT RAISE(FAIL, 'synthetic audit failure'); END`); err != nil {
				t.Fatal(err)
			}
			path := scenario.path
			if strings.HasSuffix(path, "/") {
				path += original.ID
			}
			// When any contact mutation reaches the failed audit transaction.
			failed := world.request(t, scenario.method, path, "application/json", strings.NewReader(scenario.body))
			thenStatus(t, failed, http.StatusInternalServerError)
			// Then neither the mutation nor its audit event commits.
			if count, err := world.store.CountContacts(t.Context(), world.actor); err != nil || count != 1 {
				t.Fatalf("Then failed audit must preserve contact count: %d, %v", count, err)
			}
			read := world.request(t, http.MethodGet, "/api/contacts/"+original.ID, "", nil)
			thenStatus(t, read, http.StatusOK)
			if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, original) {
				t.Fatalf("Then failed audit must preserve original contact: %+v", got)
			}
			thenContactAuditCount(t, world, 1)

			// When the audit store recovers, then the same operation can succeed.
			if _, err := world.store.DB().ExecContext(t.Context(), `DROP TRIGGER reject_bdd_audit`); err != nil {
				t.Fatal(err)
			}
			retried := world.request(t, scenario.method, path, "application/json", strings.NewReader(scenario.body))
			thenStatus(t, retried, scenario.status)
			if count, err := world.store.CountContacts(t.Context(), world.actor); err != nil || count != scenario.count {
				t.Fatalf("Then retry must commit once: count=%d, want %d: %v", count, scenario.count, err)
			}
			thenContactAuditCount(t, world, 2)
			if scenario.method == http.MethodPut {
				read = world.request(t, http.MethodGet, "/api/contacts/"+original.ID, "", nil)
				thenStatus(t, read, http.StatusOK)
				if got := thenContactContract(t, read, world.actor); got.FirstName != "Changed" {
					t.Fatalf("Then recovered update must persist the replacement, got %+v", got)
				}
			}
		})
	}
}

func TestBDDContactCSVExport(t *testing.T) {
	t.Run("empty_workspace_has_header_only", func(t *testing.T) {
		// Given no contacts, when CSV export is requested.
		world := givenBDDApp(t)
		response := world.request(t, http.MethodGet, "/api/contacts/export", "", nil)
		thenStatus(t, response, http.StatusOK)
		rows, err := csv.NewReader(bytes.NewReader(response.body)).ReadAll()
		// Then the export is a valid header-only CSV, with an audited attempt.
		if err != nil || len(rows) != 1 || !slices.Equal(rows[0], []string{"id", "first_name", "last_name", "email", "phone", "company", "created_at", "updated_at"}) {
			t.Fatalf("Then empty CSV must retain its schema: rows=%v error=%v", rows, err)
		}
		thenContactAuditCount(t, world, 1)
	})
	t.Run("quoting_and_spreadsheet_safety_preserve_stored_values", func(t *testing.T) {
		// Given commas, quotes, apostrophes, Unicode and formula-like values.
		world := givenBDDApp(t)
		created := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Jane, \"JJ\"","last_name":"O'Brien 界","phone":"+15551234","company":"=1+1"}`))
		thenStatus(t, created, http.StatusCreated)
		original := thenContactContract(t, created, world.actor)
		// When the consumer downloads CSV and parses it as CSV.
		response := world.request(t, http.MethodGet, "/api/contacts/export", "", nil)
		thenStatus(t, response, http.StatusOK)
		rows, err := csv.NewReader(bytes.NewReader(response.body)).ReadAll()
		// Then quoting round-trips text and formula prefixes are neutralized.
		if err != nil || len(rows) != 2 || len(rows[1]) != 8 || !slices.Equal(rows[1][:6], []string{original.ID, `Jane, "JJ"`, "O'Brien 界", "", "'+15551234", "'=1+1"}) {
			t.Fatalf("Then exported values must be safely encoded: rows=%v error=%v", rows, err)
		}
		if response.header.Get("Content-Type") != "text/csv" || response.header.Get("Content-Disposition") != "attachment; filename=contacts.csv" || response.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("Then export must use its attachment contract: %v", response.header)
		}
		read := world.request(t, http.MethodGet, "/api/contacts/"+original.ID, "", nil)
		thenStatus(t, read, http.StatusOK)
		if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, original) {
			t.Fatalf("Then CSV safety must not modify stored values: %+v", got)
		}
		thenContactAuditCount(t, world, 2)
	})
}

func TestBDDContactReadModifyWriteProjection(t *testing.T) {
	// Given a contact returned with server-owned identity, owner and timestamps.
	world := givenBDDApp(t)
	created := world.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Before","last_name":"Keep","email":"keep@example.com","phone":"123","company":"Example"}`))
	thenStatus(t, created, http.StatusCreated)
	contact := thenContactContract(t, created, world.actor)
	path := "/api/contacts/" + contact.ID
	read := world.request(t, http.MethodGet, path, "", nil)
	thenStatus(t, read, http.StatusOK)
	// When a client submits the entire response, then read-only fields are rejected.
	rejected := world.request(t, http.MethodPut, path, "application/json", bytes.NewReader(read.body))
	thenStatus(t, rejected, http.StatusBadRequest)
	thenContactAuditCount(t, world, 1)

	// When a client projects the five writable fields and changes one value.
	projection := map[string]string{
		"first_name": "After", "last_name": contact.LastName,
		"email": contact.Email, "phone": contact.Phone, "company": contact.Company,
	}
	body, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	updated := world.request(t, http.MethodPut, path, "application/json", bytes.NewReader(body))
	thenStatus(t, updated, http.StatusOK)
	result := thenContactContract(t, updated, world.actor)
	// Then replacement preserves the submitted fields and server-owned metadata.
	if result.FirstName != "After" || result.LastName != contact.LastName || result.Email != contact.Email || result.Phone != contact.Phone || result.Company != contact.Company || result.ID != contact.ID || !result.CreatedAt.Equal(contact.CreatedAt) {
		t.Fatalf("Then projected replacement must preserve all other fields: %+v", result)
	}
	thenContactAuditCount(t, world, 2)
	read = world.request(t, http.MethodGet, path, "", nil)
	thenStatus(t, read, http.StatusOK)
	if got := thenContactContract(t, read, world.actor); !reflect.DeepEqual(got, result) {
		t.Fatalf("Then projected replacement must be persisted, not only echoed: %+v, want %+v", got, result)
	}
}

func TestBDDContactCursorContinuesAfterDeletionAndInterveningInserts(t *testing.T) {
	// Given an actor's tied-timestamp records ordered c,b,a and a one-item cursor.
	world := givenBDDApp(t)
	if _, err := world.store.ImportContacts(t.Context(), world.actor, []store.Contact{{ID: "a"}, {ID: "b"}, {ID: "c"}}); err != nil {
		t.Fatal(err)
	}
	first := world.request(t, http.MethodGet, "/api/contacts?limit=1", "", nil)
	thenStatus(t, first, http.StatusOK)
	page := decodeBDD[bddContactPage](t, first)
	if len(page.Contacts) != 1 || page.Contacts[0].ID != "c" || page.NextPageToken == "" {
		t.Fatalf("Given initial keyset page c with cursor, got %+v", page)
	}
	cursor := page.NextPageToken
	anchorTime := page.Contacts[0].CreatedAt

	// When the anchor is deleted and records arrive on either side of the cursor.
	deleted := world.request(t, http.MethodDelete, "/api/contacts/c", "", nil)
	thenStatus(t, deleted, http.StatusNoContent)
	for _, fixture := range []struct {
		id string
		at time.Time
	}{
		{"newer", anchorTime.Add(time.Second)},
		{"older", anchorTime.Add(-time.Second)},
	} {
		contact := store.Contact{ID: fixture.id}
		if err := world.store.CreateContact(t.Context(), world.actor, &contact); err != nil {
			t.Fatal(err)
		}
		// Controlled fixture timestamps make both insertion positions deterministic.
		if _, err := world.store.DB().ExecContext(t.Context(), `UPDATE contacts SET created_at = ? WHERE id = ? AND owner_id = ?`, store.DatabaseTime(fixture.at), fixture.id, world.actor); err != nil {
			t.Fatal(err)
		}
	}
	// Then live keyset pagination skips new records before the cursor, includes
	// later records after it, and does not require the deleted anchor to exist.
	for i, want := range []string{"b", "a", "older"} {
		response := world.request(t, http.MethodGet, "/api/contacts?limit=1&page_token="+url.QueryEscape(cursor), "", nil)
		thenStatus(t, response, http.StatusOK)
		next := decodeBDD[bddContactPage](t, response)
		if len(next.Contacts) != 1 || next.Contacts[0].ID != want {
			t.Fatalf("Then continuation item %d=%+v, want %s", i, next, want)
		}
		cursor = next.NextPageToken
		if (i < 2) != (cursor != "") {
			t.Fatalf("Then continuation cursor at item %d=%q", i, cursor)
		}
	}
	fresh := world.request(t, http.MethodGet, "/api/contacts", "", nil)
	thenStatus(t, fresh, http.StatusOK)
	freshPage := decodeBDD[bddContactPage](t, fresh)
	if len(freshPage.Contacts) != 4 || freshPage.Contacts[0].ID != "newer" {
		t.Fatalf("Then a fresh list sees the new leading record: %+v", freshPage)
	}
}
