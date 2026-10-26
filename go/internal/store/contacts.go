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
	FirstName string    `json:"first_name"`
	LastName  string    `json:"last_name"`
	Email     string    `json:"email"`
	Phone     string    `json:"phone"`
	Company   string    `json:"company"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

var ErrNotFound = errors.New("not found")

type PageToken struct {
	CreatedAt time.Time
	ID        string
}

func (s *Store) ListContacts(ctx context.Context, limit int, cursor *PageToken) (contacts []Contact, err error) {
	var rows *sql.Rows

	if cursor == nil {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, first_name, last_name, email, phone, company, created_at, updated_at
			FROM contacts
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		`, limit)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT id, first_name, last_name, email, phone, company, created_at, updated_at
			FROM contacts
			WHERE (created_at, id) < (?, ?)
			ORDER BY created_at DESC, id DESC
			LIMIT ?
		`, cursor.CreatedAt, cursor.ID, limit)
	}
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer closeWithError(rows, &err)

	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}

func (s *Store) GetContact(ctx context.Context, id string) (Contact, error) {
	var c Contact
	err := s.db.QueryRowContext(ctx, `
		SELECT id, first_name, last_name, email, phone, company, created_at, updated_at
		FROM contacts WHERE id = ?
	`, id).Scan(&c.ID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt)
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
	c.CreatedAt = now
	c.UpdatedAt = now

	return s.withTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO contacts (id, first_name, last_name, email, phone, company, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`, c.ID, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, c.CreatedAt, c.UpdatedAt)
		if err != nil {
			return fmt.Errorf("insert contact: %w", err)
		}
		return logActivity(ctx, tx, actor, "create", "contact", c.ID)
	})
}

func (s *Store) UpdateContact(ctx context.Context, actor string, c *Contact) error {
	updatedAt := time.Now().UTC()
	var createdAt time.Time

	err := s.withTx(ctx, func(tx *sql.Tx) error {
		err := tx.QueryRowContext(ctx, `
		UPDATE contacts
		SET first_name = ?, last_name = ?, email = ?, phone = ?, company = ?, updated_at = ?
		WHERE id = ?
		RETURNING created_at
	`, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, updatedAt, c.ID).Scan(&createdAt)
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
	c.CreatedAt = createdAt
	c.UpdatedAt = updatedAt
	return nil
}

func (s *Store) DeleteContact(ctx context.Context, actor, id string) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, `DELETE FROM contacts WHERE id = ?`, id)
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

func (s *Store) CountContacts(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM contacts`).Scan(&count)
	return count, err
}

func (s *Store) ImportContacts(ctx context.Context, actor string, contacts []Contact) (int, error) {
	err := s.withTx(ctx, func(tx *sql.Tx) (err error) {
		stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO contacts (id, first_name, last_name, email, phone, company, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
	`)
		if err != nil {
			return fmt.Errorf("prepare stmt: %w", err)
		}
		defer closeWithError(stmt, &err)

		now := time.Now().UTC()
		for _, c := range contacts {
			c.CreatedAt = now
			c.UpdatedAt = now
			if _, err := stmt.ExecContext(ctx, c.ID, c.FirstName, c.LastName, c.Email, c.Phone, c.Company, c.CreatedAt, c.UpdatedAt); err != nil {
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

func (s *Store) ExportContacts(ctx context.Context) (contacts []Contact, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, first_name, last_name, email, phone, company, created_at, updated_at
		FROM contacts
		ORDER BY created_at
	`)
	if err != nil {
		return nil, fmt.Errorf("query contacts: %w", err)
	}
	defer closeWithError(rows, &err)

	for rows.Next() {
		var c Contact
		if err := rows.Scan(&c.ID, &c.FirstName, &c.LastName, &c.Email, &c.Phone, &c.Company, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan contact: %w", err)
		}
		contacts = append(contacts, c)
	}
	return contacts, rows.Err()
}
