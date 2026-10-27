package api

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/mail"
	"unicode"
	"unicode/utf8"

	"github.com/weave-lab/interview-public/go/internal/store"
)

const (
	maxContactBody    = 1 << 20
	maxImportBody     = 8 << 20
	maxImportContacts = 10000
)

// Separate writable fields from stored fields so clients cannot supply IDs or
// timestamps. Empty fields remain valid for compatibility with the demo CRM.
type contactInput struct {
	FirstName string
	LastName  string
	Email     string
	Phone     string
	Company   string
}

func jsonDecoder(w http.ResponseWriter, r *http.Request, limit int64) (*json.Decoder, bool) {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		writeError(w, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return nil, false
	}
	if r.ContentLength > limit {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return nil, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	return json.NewDecoder(r.Body), true
}

// Decode object members individually: normal struct decoding accepts duplicate
// keys, case-insensitive names and null strings, which obscure client mistakes.
func readContactInput(decoder *json.Decoder) (contactInput, error) {
	var input contactInput
	opening, err := decoder.Token()
	if err != nil {
		return input, err
	}
	if opening != json.Delim('{') {
		return input, errors.New("expected a contact object")
	}
	remaining := map[string]*string{
		"first_name": &input.FirstName,
		"last_name":  &input.LastName,
		"email":      &input.Email,
		"phone":      &input.Phone,
		"company":    &input.Company,
	}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return input, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return input, errors.New("expected a contact field name")
		}
		field, ok := remaining[key]
		if !ok {
			return input, errors.New("unknown or duplicate contact field")
		}
		valueToken, err := decoder.Token()
		if err != nil {
			return input, err
		}
		value, ok := valueToken.(string)
		if !ok {
			return input, errors.New("contact fields must be non-null strings")
		}
		*field = value
		delete(remaining, key)
	}
	closing, err := decoder.Token()
	if err != nil {
		return input, err
	}
	if closing != json.Delim('}') {
		return input, errors.New("expected end of contact object")
	}
	return input, nil
}

func decodeError(w http.ResponseWriter, err error) {
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		writeError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	writeError(w, http.StatusBadRequest, "invalid JSON body")
}

func requireJSONEnd(w http.ResponseWriter, decoder *json.Decoder) bool {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		decodeError(w, err)
		return false
	}
	return true
}

func decodeContact(w http.ResponseWriter, r *http.Request) (store.Contact, bool) {
	decoder, ok := jsonDecoder(w, r, maxContactBody)
	if !ok {
		return store.Contact{}, false
	}
	input, err := readContactInput(decoder)
	if err != nil {
		decodeError(w, err)
		return store.Contact{}, false
	}
	if !validContact(input) {
		writeError(w, http.StatusBadRequest, "invalid contact fields")
		return store.Contact{}, false
	}
	if !requireJSONEnd(w, decoder) {
		return store.Contact{}, false
	}
	return input.contact(), true
}

func decodeContactImport(w http.ResponseWriter, r *http.Request) ([]store.Contact, bool) {
	decoder, ok := jsonDecoder(w, r, maxImportBody)
	if !ok {
		return nil, false
	}
	opening, err := decoder.Token()
	if err != nil {
		decodeError(w, err)
		return nil, false
	}
	if opening != json.Delim('[') {
		writeError(w, http.StatusBadRequest, "expected an array of contacts")
		return nil, false
	}
	contacts := make([]store.Contact, 0)
	for decoder.More() {
		if len(contacts) == maxImportContacts {
			writeError(w, http.StatusBadRequest, "maximum 10000 contacts per import")
			return nil, false
		}
		input, err := readContactInput(decoder)
		if err != nil {
			decodeError(w, err)
			return nil, false
		}
		if !validContact(input) {
			writeError(w, http.StatusBadRequest, "invalid contact fields")
			return nil, false
		}
		contacts = append(contacts, input.contact())
	}
	if _, err := decoder.Token(); err != nil {
		decodeError(w, err)
		return nil, false
	}
	if !requireJSONEnd(w, decoder) {
		return nil, false
	}
	return contacts, true
}

func validContact(c contactInput) bool {
	for _, field := range []struct {
		value string
		limit int
	}{
		{c.FirstName, 256}, {c.LastName, 256}, {c.Company, 256},
		{c.Phone, 64}, {c.Email, 254},
	} {
		if utf8.RuneCountInString(field.value) > field.limit {
			return false
		}
		for _, r := range field.value {
			if unicode.IsControl(r) {
				return false
			}
		}
	}
	if c.Email != "" {
		address, err := mail.ParseAddress(c.Email)
		if err != nil || address.Address != c.Email || len(c.Email) > 254 {
			return false
		}
	}
	return true
}

func (c contactInput) contact() store.Contact {
	return store.Contact{FirstName: c.FirstName, LastName: c.LastName, Email: c.Email, Phone: c.Phone, Company: c.Company}
}
