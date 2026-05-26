// Package enrollment manages agent enrollment tokens backed by the
// enrollment_token DB table via the Hub store layer.
package enrollment

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/identity/store/enrollstore"
	"github.com/AlphaBitCore/nexus-gateway/packages/nexus-hub/internal/storage/store"
)

// Token represents an enrollment token (API response shape).
type Token struct {
	ID        string         `json:"id"`
	RawToken  string         `json:"token,omitempty"`
	ThingType string         `json:"thingType"`
	Label     string         `json:"label"`
	ExpiresAt time.Time      `json:"expiresAt"`
	Status    string         `json:"status"`
	Metadata  map[string]any `json:"metadata,omitempty"`
	CreatedBy *string        `json:"createdBy,omitempty"`
	CreatedAt time.Time      `json:"createdAt"`
}

// GenerateRequest holds params for generating a token.
type GenerateRequest struct {
	ThingType string         `json:"thingType"`
	Label     string         `json:"label"`
	ExpiresIn string         `json:"expiresIn"`
	Metadata  map[string]any `json:"metadata"`
	CreatedBy string         `json:"createdBy"`
}

// Service manages enrollment tokens backed by PostgreSQL.
type Service struct {
	store *store.Store
}

// NewService creates an enrollment token service.
func NewService(s *store.Store) *Service {
	return &Service{store: s}
}

// GenerateToken creates a new enrollment token.
func (s *Service) GenerateToken(ctx context.Context, req GenerateRequest) (*Token, error) {
	expiresIn := 24 * time.Hour
	if req.ExpiresIn != "" {
		d, err := time.ParseDuration(req.ExpiresIn)
		if err != nil {
			return nil, fmt.Errorf("invalid expiresIn: %w", err)
		}
		expiresIn = d
	}

	thingType := req.ThingType
	if thingType == "" {
		thingType = "agent"
	}

	et, rawToken, err := s.store.EnrollStore().InsertEnrollmentToken(ctx, store.InsertEnrollmentTokenParams{
		ThingType: thingType,
		Label:     req.Label,
		ExpiresIn: expiresIn,
		Metadata:  req.Metadata,
		CreatedBy: req.CreatedBy,
	})
	if err != nil {
		return nil, fmt.Errorf("generate token: %w", err)
	}

	return &Token{
		ID:        et.ID,
		RawToken:  rawToken,
		ThingType: et.ThingType,
		Label:     et.Label,
		ExpiresAt: et.ExpiresAt,
		Status:    et.Status,
		Metadata:  et.Metadata,
		CreatedBy: et.CreatedBy,
		CreatedAt: et.CreatedAt,
	}, nil
}

// ListTokens returns all enrollment tokens (raw token strings are not exposed).
func (s *Service) ListTokens(ctx context.Context) ([]Token, error) {
	tokens, err := s.store.EnrollStore().ListEnrollmentTokens(ctx, "", "")
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}

	result := make([]Token, len(tokens))
	for i, et := range tokens {
		result[i] = Token{
			ID:        et.ID,
			ThingType: et.ThingType,
			Label:     et.Label,
			ExpiresAt: et.ExpiresAt,
			Status:    computeStatus(et),
			Metadata:  et.Metadata,
			CreatedBy: et.CreatedBy,
			CreatedAt: et.CreatedAt,
		}
	}
	return result, nil
}

// ValidateToken checks if a raw token string is valid for enrollment.
// Returns the token if valid (pending + not expired), nil otherwise.
func (s *Service) ValidateToken(ctx context.Context, rawToken string) (*Token, bool) {
	et, err := s.store.EnrollStore().ValidateEnrollmentToken(ctx, rawToken)
	if err != nil || et == nil {
		return nil, false
	}
	if et.Status != "pending" || time.Now().After(et.ExpiresAt) {
		return nil, false
	}
	return &Token{
		ID:        et.ID,
		ThingType: et.ThingType,
		Label:     et.Label,
		ExpiresAt: et.ExpiresAt,
		Status:    et.Status,
		Metadata:  et.Metadata,
		CreatedBy: et.CreatedBy,
		CreatedAt: et.CreatedAt,
	}, true
}

// MarkUsed marks a token as used and associates it with a thing.
func (s *Service) MarkUsed(ctx context.Context, tokenID, thingID string) error {
	err := s.store.EnrollStore().MarkEnrollmentTokenUsed(ctx, tokenID, thingID)
	if errors.Is(err, enrollstore.ErrNotFound) {
		return store.ErrNotFound
	}
	return err
}

// Revoke revokes a pending token.
func (s *Service) Revoke(ctx context.Context, tokenID string) error {
	err := s.store.EnrollStore().RevokeEnrollmentToken(ctx, tokenID)
	if errors.Is(err, enrollstore.ErrNotFound) {
		return store.ErrNotFound
	}
	return err
}

func computeStatus(et store.EnrollmentToken) string {
	if et.Status == "pending" && time.Now().After(et.ExpiresAt) {
		return "expired"
	}
	return et.Status
}
