package keys

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type APIKey struct {
	ID         uuid.UUID  `json:"id"`
	ProjectID  uuid.UUID  `json:"project_id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

type Issued struct {
	APIKey
	Plaintext string `json:"key"` // returned once, never again
}

var (
	ErrNotFound = errors.New("key not found")
	ErrRevoked  = errors.New("key revoked")
	ErrExpired  = errors.New("key expired")
)

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

func (s *Service) Issue(ctx context.Context, projectID uuid.UUID, name string) (Issued, error) {
	plaintext, prefix, err := Generate()
	if err != nil {
		return Issued{}, err
	}
	hash, err := Hash(plaintext)
	if err != nil {
		return Issued{}, err
	}

	var k APIKey
	err = s.pool.QueryRow(ctx, `
		INSERT INTO api_keys (project_id, prefix, hash, name)
		VALUES ($1, $2, $3, $4)
		RETURNING id, project_id, prefix, name, last_used_at, expires_at, created_at, revoked_at
	`, projectID, prefix, hash, name).Scan(
		&k.ID, &k.ProjectID, &k.Prefix, &k.Name, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt, &k.RevokedAt,
	)
	if err != nil {
		return Issued{}, fmt.Errorf("insert api_key: %w", err)
	}
	return Issued{APIKey: k, Plaintext: plaintext}, nil
}

func (s *Service) List(ctx context.Context, projectID uuid.UUID) ([]APIKey, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, project_id, prefix, name, last_used_at, expires_at, created_at, revoked_at
		FROM api_keys
		WHERE project_id = $1
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []APIKey
	for rows.Next() {
		var k APIKey
		if err := rows.Scan(&k.ID, &k.ProjectID, &k.Prefix, &k.Name, &k.LastUsedAt, &k.ExpiresAt, &k.CreatedAt, &k.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *Service) Revoke(ctx context.Context, projectID, keyID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at = now()
		WHERE id = $1 AND project_id = $2 AND revoked_at IS NULL
	`, keyID, projectID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// Verify looks up a plaintext key, argon2id-verifies it, and returns the owning project and key IDs.
// On success, it updates last_used_at best-effort.
func (s *Service) Verify(ctx context.Context, plaintext string) (projectID, keyID uuid.UUID, err error) {
	prefix, err := ParsePrefix(plaintext)
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	var (
		id, pid    uuid.UUID
		hash       string
		revokedAt  *time.Time
		expiresAt  *time.Time
	)
	err = s.pool.QueryRow(ctx, `
		SELECT id, project_id, hash, revoked_at, expires_at
		FROM api_keys
		WHERE prefix = $1
	`, prefix).Scan(&id, &pid, &hash, &revokedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, ErrNotFound
	}
	if err != nil {
		return uuid.Nil, uuid.Nil, err
	}
	if revokedAt != nil {
		return uuid.Nil, uuid.Nil, ErrRevoked
	}
	if expiresAt != nil && time.Now().After(*expiresAt) {
		return uuid.Nil, uuid.Nil, ErrExpired
	}
	if err := Verify(plaintext, hash); err != nil {
		return uuid.Nil, uuid.Nil, err
	}

	_, _ = s.pool.Exec(ctx, `UPDATE api_keys SET last_used_at = now() WHERE id = $1`, id)
	return pid, id, nil
}
