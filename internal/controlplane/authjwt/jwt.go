// Package authjwt issues + verifies HS256 JWTs for pwrap's REST handoff.
// Claims mirror what PostgREST expects:
//
//	role        — the Postgres tenant role to SET ROLE into
//	project_id  — pwrap project UUID (useful for RLS policies)
//	user_id     — optional app-level user, echoed into request.jwt.claims
//	exp, iat    — standard
package authjwt

import (
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
)

type Claims struct {
	Role      string    `json:"role"`
	ProjectID uuid.UUID `json:"project_id"`
	UserID    string    `json:"user_id,omitempty"`
	jwt.RegisteredClaims
}

type Signer struct {
	secret []byte
}

func NewSigner(secret string) (*Signer, error) {
	if len(secret) < 16 {
		return nil, errors.New("jwt secret must be at least 16 chars")
	}
	return &Signer{secret: []byte(secret)}, nil
}

type IssueOpts struct {
	Role      string
	ProjectID uuid.UUID
	UserID    string // optional
	TTL       time.Duration
}

// Issue returns a signed HS256 JWT + its expiration time.
func (s *Signer) Issue(o IssueOpts) (token string, expiresAt time.Time, err error) {
	if o.Role == "" {
		return "", time.Time{}, errors.New("role is required")
	}
	if o.TTL <= 0 {
		o.TTL = time.Hour
	}
	now := time.Now().UTC()
	exp := now.Add(o.TTL)
	claims := Claims{
		Role:      o.Role,
		ProjectID: o.ProjectID,
		UserID:    o.UserID,
		RegisteredClaims: jwt.RegisteredClaims{
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
			Issuer:    "pwrap",
		},
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := tok.SignedString(s.secret)
	if err != nil {
		return "", time.Time{}, fmt.Errorf("sign: %w", err)
	}
	return signed, exp, nil
}

// Verify parses and validates the signature + expiration. Returns the decoded claims.
func (s *Signer) Verify(token string) (Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return s.secret, nil
	})
	if err != nil {
		return Claims{}, err
	}
	return c, nil
}
