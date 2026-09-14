package admintokens

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/b3vet/pwrap/internal/controlplane/keys"
)

// TokenPrefix marks an admin token, so a leaked credential is identifiable at a
// glance and cannot be confused with a project API key ("pwk_").
const TokenPrefix = "pwa_"

var (
	ErrNotFound = errors.New("admin token not found")
	ErrRevoked  = errors.New("admin token revoked or expired")
	ErrInvalid  = errors.New("invalid admin token")
)

// Token is an admin token's metadata. The secret itself is never stored or
// returned after issue.
type Token struct {
	ID         uuid.UUID  `json:"id"`
	Prefix     string     `json:"prefix"`
	Name       string     `json:"name"`
	Scopes     []string   `json:"scopes"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Issued carries the plaintext exactly once, at creation.
type Issued struct {
	Token
	Secret string `json:"token"`
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service { return &Service{pool: pool} }

// Issue mints a token. The plaintext is returned once and never recoverable:
// only its argon2id hash is stored, matching how project API keys are handled.
func (s *Service) Issue(ctx context.Context, name string, scopes []Scope, ttl time.Duration) (Issued, error) {
	if len(scopes) == 0 {
		return Issued{}, fmt.Errorf("at least one scope is required (%s)", JoinAll())
	}
	plaintext, prefix, err := keys.GenerateWithPrefix(TokenPrefix)
	if err != nil {
		return Issued{}, err
	}
	hash, err := keys.Hash(plaintext)
	if err != nil {
		return Issued{}, fmt.Errorf("hash token: %w", err)
	}
	var expires *time.Time
	if ttl > 0 {
		t := time.Now().UTC().Add(ttl)
		expires = &t
	}

	var out Issued
	err = s.pool.QueryRow(ctx, `
		INSERT INTO admin_tokens (prefix, token_hash, name, scopes, expires_at)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, prefix, name, scopes, last_used_at, expires_at, revoked_at, created_at
	`, prefix, hash, name, Strings(scopes), expires).Scan(
		&out.ID, &out.Prefix, &out.Name, &out.Scopes,
		&out.LastUsedAt, &out.ExpiresAt, &out.RevokedAt, &out.CreatedAt)
	if err != nil {
		return Issued{}, err
	}
	out.Secret = plaintext
	return out, nil
}

func (s *Service) List(ctx context.Context) ([]Token, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, prefix, name, scopes, last_used_at, expires_at, revoked_at, created_at
		FROM admin_tokens ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Token{}
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Prefix, &t.Name, &t.Scopes,
			&t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Revoke marks a token unusable. Idempotent: revoking twice is not an error,
// because the caller's intent is already satisfied.
func (s *Service) Revoke(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE admin_tokens SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		// Either it never existed or it was already revoked; distinguish so a
		// typo'd id is not silently reported as success.
		var exists bool
		if err := s.pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM admin_tokens WHERE id = $1)`, id).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return ErrNotFound
		}
	}
	return nil
}

// Verify authenticates a plaintext token and returns its scopes.
//
// Lookup is by the clear prefix, then argon2id verification of the full secret —
// the prefix narrows to one row so a valid-looking token costs exactly one hash
// comparison, and an invalid one costs none.
func (s *Service) Verify(ctx context.Context, plaintext string) (Token, []Scope, error) {
	if !strings.HasPrefix(plaintext, TokenPrefix) {
		return Token{}, nil, ErrInvalid
	}
	payload := plaintext[len(TokenPrefix):]
	if len(payload) < keys.PrefixLookup {
		return Token{}, nil, ErrInvalid
	}
	prefix := payload[:keys.PrefixLookup]

	var t Token
	var hash string
	err := s.pool.QueryRow(ctx, `
		SELECT id, prefix, name, scopes, token_hash, last_used_at, expires_at, revoked_at, created_at
		FROM admin_tokens WHERE prefix = $1
	`, prefix).Scan(&t.ID, &t.Prefix, &t.Name, &t.Scopes, &hash,
		&t.LastUsedAt, &t.ExpiresAt, &t.RevokedAt, &t.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, nil, ErrInvalid
	}
	if err != nil {
		return Token{}, nil, err
	}
	if err := keys.Verify(plaintext, hash); err != nil {
		return Token{}, nil, ErrInvalid
	}
	if t.RevokedAt != nil {
		return Token{}, nil, ErrRevoked
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return Token{}, nil, ErrRevoked
	}

	// Best-effort: a failure to record use must not deny an otherwise valid
	// request.
	_, _ = s.pool.Exec(ctx, `UPDATE admin_tokens SET last_used_at = now() WHERE id = $1`, t.ID)

	scopes := make([]Scope, len(t.Scopes))
	for i, sc := range t.Scopes {
		scopes[i] = Scope(sc)
	}
	return t, scopes, nil
}
