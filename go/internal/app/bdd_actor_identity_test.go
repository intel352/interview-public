package app

import (
	"bytes"
	"encoding/csv"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

func TestBDDActorIdentityFlowsToOwnershipAndAudit(t *testing.T) {
	// Given two configured actors with separate opaque credentials on one server.
	world := givenBDDApp(t)
	alice := world.forActor(t, "alice@example.com")
	bob := world.forActor(t, "bob@example.com")
	for _, consumer := range []*bddWorld{alice, bob} {
		t.Run(consumer.actor, func(t *testing.T) {
			// When each actor verifies its credential and creates a contact.
			response := consumer.request(t, http.MethodPost, "/api/auth/token", "", nil)
			thenStatus(t, response, http.StatusOK)
			identity := decodeBDD[map[string]string](t, response)
			if len(identity) != 2 || identity["user_id"] != consumer.actor || identity["status"] != "authenticated" {
				t.Fatalf("Then authenticated context identifies this actor without issuing credentials: %v", identity)
			}
			response = consumer.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(`{"first_name":"Owned contact"}`))
			thenStatus(t, response, http.StatusCreated)
			contact := decodeBDD[store.Contact](t, response)
			if contact.ID == "" || contact.OwnerID != consumer.actor {
				t.Fatalf("Then API ownership comes from authenticated context: %+v", contact)
			}
			// Then the same actor is persisted as owner and mutation-audit actor.
			var owner, actor, action string
			err := alice.store.DB().QueryRowContext(t.Context(), `
				SELECT c.owner_id, a.user_id, a.action
				FROM contacts c JOIN activity_log a ON a.resource_id = c.id
				WHERE c.id = ?
			`, contact.ID).Scan(&owner, &actor, &action)
			if err != nil {
				t.Fatal(err)
			}
			if owner != consumer.actor || actor != consumer.actor || action != "create" {
				t.Fatalf("Then context reaches both ownership and audit: owner=%q actor=%q action=%q", owner, actor, action)
			}
		})
	}
	thenBDDActorStateCounts(t, alice, 2, 2)
}

func TestBDDActorsCannotReadMutateOrReassignEachOthersContacts(t *testing.T) {
	// Given Alice and Bob each own a contact on the same server.
	world := givenBDDApp(t)
	alice := world.forActor(t, "alice@example.com")
	bob := world.forActor(t, "bob@example.com")
	actors := []*bddWorld{alice, bob}
	contacts := make([]store.Contact, len(actors))
	for i, consumer := range actors {
		body := fmt.Sprintf(`{"first_name":"Private to %s"}`, consumer.actor)
		response := consumer.request(t, http.MethodPost, "/api/contacts", "application/json", strings.NewReader(body))
		thenStatus(t, response, http.StatusCreated)
		contacts[i] = decodeBDD[store.Contact](t, response)
	}
	for i, consumer := range actors {
		other := contacts[1-i]
		for _, method := range []string{http.MethodGet, http.MethodPut, http.MethodDelete} {
			t.Run(consumer.actor+" cross-owner "+method, func(t *testing.T) {
				// When another actor's ID is used for a read, update or deletion.
				response := consumer.request(t, method, "/api/contacts/"+other.ID, "application/json", strings.NewReader(`{"first_name":"Must not change"}`))
				// Then the resource is indistinguishable from a missing contact.
				thenStatus(t, response, http.StatusNotFound)
				errorBody := decodeBDD[map[string]string](t, response)
				if len(errorBody) != 1 || errorBody["error"] != "contact not found" {
					t.Fatalf("Then denied access discloses no contact fields: %s", response.body)
				}
			})
		}
	}

	for _, attempt := range []struct{ name, method, path, body string }{
		{"create", http.MethodPost, "/api/contacts", `{"first_name":"Spoof","owner_id":"bob@example.com"}`},
		{"update", http.MethodPut, "/api/contacts/" + contacts[0].ID, `{"first_name":"Spoof","owner_id":"bob@example.com"}`},
		{"import", http.MethodPost, "/api/contacts/import", `[{"first_name":"Valid first"},{"owner_id":"bob@example.com"}]`},
	} {
		t.Run("client cannot choose owner during "+attempt.name, func(t *testing.T) {
			// When a client supplies a server-owned identity field.
			response := alice.request(t, attempt.method, attempt.path, "application/json", strings.NewReader(attempt.body))
			// Then validation rejects the entire operation before mutation/audit.
			thenStatus(t, response, http.StatusBadRequest)
		})
	}
	thenBDDActorStateCounts(t, alice, 2, 2)
	for i, consumer := range actors {
		response := consumer.request(t, http.MethodGet, "/api/contacts/"+contacts[i].ID, "", nil)
		thenStatus(t, response, http.StatusOK)
		got := decodeBDD[store.Contact](t, response)
		if got != contacts[i] {
			t.Fatalf("Then each owner's contact remains unchanged: got %+v, want %+v", got, contacts[i])
		}
	}
}

func TestBDDActorContactCollectionsAndCursorsAreIsolated(t *testing.T) {
	// Given each actor imports its own batch, including tied creation timestamps.
	world := givenBDDApp(t)
	alice := world.forActor(t, "alice@example.com")
	bob := world.forActor(t, "bob@example.com")
	actors := []*bddWorld{alice, bob}
	for _, consumer := range actors {
		body := fmt.Sprintf(`[{"first_name":"%s one"},{"first_name":"%s two"},{"first_name":"%s three"}]`, consumer.actor, consumer.actor, consumer.actor)
		response := consumer.request(t, http.MethodPost, "/api/contacts/import", "application/json", strings.NewReader(body))
		thenStatus(t, response, http.StatusOK)
		if imported := decodeBDD[map[string]int](t, response)["imported"]; imported != 3 {
			t.Fatalf("Then each actor's import commits three records, got %d", imported)
		}
	}
	var aliceCursor string
	for _, consumer := range actors {
		t.Run(consumer.actor+" collection", func(t *testing.T) {
			// When the actor walks one-record pages, every returned row is its own.
			path := "/api/contacts?limit=1"
			seen := make(map[string]bool)
			for pageIndex := range 3 {
				response := consumer.request(t, http.MethodGet, path, "", nil)
				thenStatus(t, response, http.StatusOK)
				page := decodeBDD[bddContactPage](t, response)
				if len(page.Contacts) != 1 {
					t.Fatalf("Then page %d contains exactly one owned record, got %d", pageIndex+1, len(page.Contacts))
				}
				contact := page.Contacts[0]
				if contact.OwnerID != consumer.actor || !strings.HasPrefix(contact.FirstName, consumer.actor+" ") || seen[contact.ID] {
					t.Fatalf("Then pages neither leak nor duplicate records: %+v", contact)
				}
				seen[contact.ID] = true
				if pageIndex < 2 {
					if page.NextPageToken == "" {
						t.Fatal("Then the next owned record has a cursor")
					}
					if consumer == alice && pageIndex == 0 {
						aliceCursor = page.NextPageToken
					}
					path = "/api/contacts?limit=1&page_token=" + url.QueryEscape(page.NextPageToken)
				} else if page.NextPageToken != "" {
					t.Fatal("Then the cursor stops after the actor's third record")
				}
			}
			// When exporting, then the CSV contains exactly those same owned IDs.
			response := consumer.request(t, http.MethodGet, "/api/contacts/export", "", nil)
			thenStatus(t, response, http.StatusOK)
			rows, err := csv.NewReader(bytes.NewReader(response.body)).ReadAll()
			if err != nil || len(rows) != 4 {
				t.Fatalf("Then export is a header plus three owned records: rows=%d error=%v", len(rows), err)
			}
			for _, row := range rows[1:] {
				if len(row) != 8 || !seen[row[0]] || !strings.HasPrefix(row[1], consumer.actor+" ") {
					t.Fatalf("Then export exposes only this actor's imported records: %v", row)
				}
				delete(seen, row[0])
			}
			if len(seen) != 0 {
				t.Fatalf("Then export includes every owned record exactly once: omitted=%v", seen)
			}
		})
	}
	// When Bob replays Alice's cursor, then it is rejected rather than applying
	// Alice's paging scope to Bob's collection or exposing Alice's data.
	if aliceCursor == "" {
		t.Fatal("Given Alice's pagination must produce a cursor")
	}
	response := bob.request(t, http.MethodGet, "/api/contacts?limit=1&page_token="+url.QueryEscape(aliceCursor), "", nil)
	thenStatus(t, response, http.StatusBadRequest)
	malformed := bob.request(t, http.MethodGet, "/api/contacts?page_token=not-base64", "", nil)
	thenStatus(t, malformed, http.StatusBadRequest)
	if !bytes.Equal(response.body, malformed.body) {
		t.Fatalf("Then a foreign cursor discloses no special ownership information: foreign=%s malformed=%s", response.body, malformed.body)
	}
	thenBDDActorStateCounts(t, alice, 6, 4) // Two imports and two export attempts.
}

func TestBDDUnknownActorCredentialIsRejected(t *testing.T) {
	// Given a validly shaped random credential that is not configured for an actor.
	world := givenBDDApp(t)
	unknown := *world
	var err error
	unknown.token, err = auth.NewToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, methodPath := range [][2]string{{http.MethodPost, "/api/auth/token"}, {http.MethodGet, "/api/contacts"}, {http.MethodPost, "/api/contacts"}} {
		// When it is presented, then no actor context or data access is granted.
		response := unknown.request(t, methodPath[0], methodPath[1], "application/json", strings.NewReader(`{}`))
		thenStatus(t, response, http.StatusUnauthorized)
	}
	thenBDDActorStateCounts(t, world, 0, 0)
}

func thenBDDActorStateCounts(t *testing.T, world *bddWorld, contacts, audits int) {
	t.Helper()
	var actualContacts, actualAudits int
	if err := world.store.DB().QueryRowContext(t.Context(), `SELECT (SELECT COUNT(*) FROM contacts), (SELECT COUNT(*) FROM activity_log)`).Scan(&actualContacts, &actualAudits); err != nil {
		t.Fatal(err)
	}
	if actualContacts != contacts || actualAudits != audits {
		t.Fatalf("Then writes and audits match authorized operations: contacts=%d audits=%d, want %d/%d", actualContacts, actualAudits, contacts, audits)
	}
}
