package api

import (
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"unicode"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/weave-lab/interview-public/go/internal/auth"
	"github.com/weave-lab/interview-public/go/internal/store"
)

type listContactsResponse struct {
	Contacts      []store.Contact `json:"contacts"`
	NextPageToken string          `json:"next_page_token,omitempty"`
}

func (a *API) HandleListContacts(w http.ResponseWriter, r *http.Request) {
	actor := auth.UserID(r.Context())
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 100 {
			writeError(w, http.StatusBadRequest, "limit must be between 1 and 100")
			return
		}
		limit = n
	}

	var cursor *store.PageToken
	if v := r.URL.Query().Get("page_token"); v != "" {
		cursor = decodePageToken(v)
		if cursor == nil {
			writeError(w, http.StatusBadRequest, "invalid page token")
			return
		}
	}

	contacts, err := a.store.ListContacts(r.Context(), actor, limit+1, cursor)
	if errors.Is(err, store.ErrInvalidPageToken) {
		writeError(w, http.StatusBadRequest, "invalid page token")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list contacts")
		return
	}

	var nextToken string
	if len(contacts) > limit {
		last := contacts[limit-1]
		nextToken, err = encodePageToken(&store.PageToken{
			OwnerID:   actor,
			CreatedAt: last.CreatedAt,
			ID:        last.ID,
		})
		if err != nil {
			writeError(w, http.StatusInternalServerError, "failed to encode page token")
			return
		}
		contacts = contacts[:limit]
	}

	writeJSON(w, http.StatusOK, listContactsResponse{
		Contacts:      contacts,
		NextPageToken: nextToken,
	})
}

func encodePageToken(t *store.PageToken) (string, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodePageToken(s string) *store.PageToken {
	if len(s) > 2048 {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	var t store.PageToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil
	}
	if t.CreatedAt.IsZero() || t.ID == "" || len(t.ID) > 128 || t.OwnerID == "" {
		return nil
	}
	return &t
}

func (a *API) HandleGetContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	contact, err := a.store.GetContact(r.Context(), auth.UserID(r.Context()), id)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "contact not found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to get contact")
		return
	}

	writeJSON(w, http.StatusOK, contact)
}

func (a *API) HandleCreateContact(w http.ResponseWriter, r *http.Request) {
	c, ok := decodeContact(w, r)
	if !ok {
		return
	}

	c.ID = uuid.NewString()
	if err := a.store.CreateContact(r.Context(), auth.UserID(r.Context()), &c); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to create contact")
		return
	}

	writeJSON(w, http.StatusCreated, c)
}

func (a *API) HandleUpdateContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	c, ok := decodeContact(w, r)
	if !ok {
		return
	}

	c.ID = id
	if err := a.store.UpdateContact(r.Context(), auth.UserID(r.Context()), &c); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "contact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to update contact")
		return
	}

	writeJSON(w, http.StatusOK, c)
}

func (a *API) HandleDeleteContact(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	if err := a.store.DeleteContact(r.Context(), auth.UserID(r.Context()), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "contact not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to delete contact")
		return
	}

	w.WriteHeader(http.StatusNoContent)
}

func (a *API) HandleImportContacts(w http.ResponseWriter, r *http.Request) {
	contacts, ok := decodeContactImport(w, r)
	if !ok {
		return
	}

	for i := range contacts {
		contacts[i].ID = uuid.NewString()
	}

	imported, err := a.store.ImportContacts(r.Context(), auth.UserID(r.Context()), contacts)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to import contacts")
		return
	}

	writeJSON(w, http.StatusOK, map[string]int{"imported": imported})
}

func (a *API) HandleExportContacts(w http.ResponseWriter, r *http.Request) {
	contacts, err := a.store.ExportContacts(r.Context(), auth.UserID(r.Context()))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to export contacts")
		return
	}
	// This records the attempt, not guaranteed delivery to a connected client.
	if err := a.store.LogActivity(r.Context(), auth.UserID(r.Context()), "export", "contacts", ""); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record export attempt")
		return
	}

	w.Header().Set("Content-Type", "text/csv")
	w.Header().Set("Content-Disposition", "attachment; filename=contacts.csv")
	w.Header().Set("X-Content-Type-Options", "nosniff")

	cw := csv.NewWriter(w)
	if err := cw.Write([]string{"id", "first_name", "last_name", "email", "phone", "company", "created_at", "updated_at"}); err != nil {
		log.Print("write CSV response failed")
		panic(http.ErrAbortHandler)
	}

	for _, c := range contacts {
		if err := cw.Write([]string{
			spreadsheetCell(c.ID),
			spreadsheetCell(c.FirstName),
			spreadsheetCell(c.LastName),
			spreadsheetCell(c.Email),
			spreadsheetCell(c.Phone),
			spreadsheetCell(c.Company),
			c.CreatedAt.Format("2006-01-02T15:04:05Z"),
			c.UpdatedAt.Format("2006-01-02T15:04:05Z"),
		}); err != nil {
			log.Print("write CSV response failed")
			panic(http.ErrAbortHandler)
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Print("flush CSV response failed")
		panic(http.ErrAbortHandler)
	}
}

// CSV quoting is not sufficient to prevent spreadsheet formula interpretation.
// Prefix dangerous values with an apostrophe; do not change the stored value.
func spreadsheetCell(value string) string {
	trimmed := strings.TrimLeftFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
	if strings.HasPrefix(value, "\t") || strings.HasPrefix(value, "\r") || strings.HasPrefix(value, "\n") || strings.ContainsAny(firstByte(trimmed), "=+-@") {
		return "'" + value
	}
	return value
}

func firstByte(value string) string {
	if value == "" {
		return ""
	}
	return value[:1]
}
