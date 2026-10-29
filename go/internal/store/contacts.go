package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type Contact struct {
	ID        string    `json:"id"`
	OwnerID   string    `json:"owner_id"`
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Email     string    `json:"email"`
	Phone     string    `json:"phone"`
	Company   string    `json:"company"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var ErrNotFound = errors.New("not found")

var ErrInvalidPageToken = errors.New("invalid page token")

type PageToken struct {
	OwnerID   string
	CreatedAt time.Time
	ID        string
}

func (s *Store) ListContacts(ctx context.Context, actor string, limit int, cursor *PageToken) (contacts []Contact, err error) {
	contacts = make([]Contact, 0)
	if cursor != nil && cursor.OwnerID != actor {
		return nil, ErrInvalidPageToken
	}
	var rows *sql.Rows

	if cursor == nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at
			FROM contacts
			WHERE owner_id = ?
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		`, actor, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at
			FROM contacts
			WHERE owner_id = ? AND (created_at, id) < (?, ?)
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		`, actor, DatabaseTime(cursor.CreatedAt), cursor.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer closeWithError(rows, &err)

	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (s *Store) GetContact(ctx context.Context, actor, id string) (Contact, error) {
	var c Contact
	err := s.db.QueryRowContext(ctx, `
		SELECT id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at
		FROM contacts WHERE owner_id = ? AND id = ?
	`, actor, id).Scan(&c.ID, &c.OwnerID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Contact{}, ErrNotFound
	}
	if err != nil {
		return Contact{}, fmt.Errorf("query contact: %w", err)
	}
	return c, nil
}

func (s *Store) CreateContact(ctx context.Context, actor string, c *Contact) error {
	now := time.Now().UTC()

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO contacts (id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, c.ID, actor, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, DatabaseTime(now), DatabaseTime(now))
		if err != nil {
			return fmt.Errorf("insert contact: %w", err)
		}
		return logActivity(ctx, tx, actor, "create", "contact", c.ID)
	})
	if err != nil {
		return err
	}
	c.OwnerID, c.CreatedAt, c.UpdatedAt = actor, now, now
	return nil
}

func (s *Store) UpdateContact(ctx context.Context, actor string, c *Contact) error {
	updatedAt := time.Now().UTC()
	var createdAt time.Time

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
		UPDATE contacts
		SET first_name = ?, last_name = ?, email = ?, phone = ?, company = ?, updated_at = ?
		WHERE owner_id = ? AND id = ?
		RETURNING created_at
	`, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, DatabaseTime(updatedAt), actor, c.ID).Scan(&createdAt)
		if errors.Is(err, sql.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return fmt.Errorf("update contact: %w", err)
		}
		return logActivity(ctx, tx, actor, "update", "contact", c.ID)
	})
	if err != nil {
		return err
	}
	c.OwnerID = actor
	c.CreatedAt = createdAt
	c.UpdatedAt = updatedAt
	return nil
}

func (s *Store) DeleteContact(ctx context.Context, actor, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE owner_id = ? AND id = ?`, actor, id)
		if err != nil {
			return fmt.Errorf("delete contact: %w", err)
		}
		if err := requireAffected(result); err != nil {
			return err
		}
		return logActivity(ctx, tx, actor, "delete", "contact", id)
	})
}

func requireAffected(result sql.Result) error {
	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) CountContacts(ctx context.Context, actor string) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts WHERE owner_id = ?`, actor).Scan(&count)
	return count, err
}

// CountAllContacts is for local startup/seeding checks, never API visibility.
func (s *Store) CountAllContacts(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&count)
	return count, err
}

func (s *Store) ImportContacts(ctx context.Context, actor string, contacts []Contact) (int, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) (err error) {
		stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO contacts (id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
		if err != nil {
			return fmt.Errorf("prepare stmt: %w", err)
		}
		defer closeWithError(stmt, &err)

		now := time.Now().UTC()
		for _, c := range contacts {
			c.CreatedAt = now
			c.UpdatedAt = now
			if _, err := stmt.ExecContext(ctx, c.ID, actor, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, DatabaseTime(c.CreatedAt), DatabaseTime(c.UpdatedAt)); err != nil {
				return fmt.Errorf("insert contact %s: %w", c.ID, err)
			}
		}
		return logActivity(ctx, tx, actor, "import", "contacts", "")
	})
	if err != nil {
		return 0, err
	}
	return len(contacts), nil
}

func (s *Store) ExportContacts(ctx context.Context, actor string) (contacts []Contact, err error) {
	contacts = make([]Contact, 0)
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, owner_id, first_name, last_name, email, phone, company, created_at, updated_at
		FROM contacts
		WHERE owner_id = ?
		ORDER BY created_at, id
	`, actor)
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer closeWithError(rows, &err)

	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.OwnerID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}
