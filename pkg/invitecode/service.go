package invitecode

import (
	"context"
	"database/sql"
	"errors"
	"time"

	pgdb "github.com/eternisai/enchanted-proxy/pkg/storage/pg/sqlc"
)

type Service struct {
	queries pgdb.Querier
}

func NewService(queries pgdb.Querier) *Service {
	return &Service{queries: queries}
}

func (s *Service) CreateInviteCode(code string, codeHash string, boundEmail *string, createdBy int64, isUsed bool, redeemedBy *string, redeemedAt *time.Time, expiresAt *time.Time, isActive bool) (*pgdb.InviteCode, error) {
	ctx := context.Background()

	params := pgdb.CreateInviteCodeParams{
		Code:       code,
		CodeHash:   codeHash,
		BoundEmail: boundEmail,
		CreatedBy:  createdBy,
		IsUsed:     isUsed,
		RedeemedBy: redeemedBy,
		RedeemedAt: redeemedAt,
		ExpiresAt:  expiresAt,
		IsActive:   isActive,
	}

	result, err := s.queries.CreateInviteCode(ctx, params)
	if err != nil {
		return nil, err
	}

	return &result, nil
}

func (s *Service) GetInviteCodeByCode(code string) (*pgdb.InviteCode, error) {
	ctx := context.Background()
	codeHash := HashCode(code)

	result, err := s.queries.GetInviteCodeByCodeHash(ctx, codeHash)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, errors.New("invite code not found")
		}
		return nil, err
	}

	return &result, nil
}

func (s *Service) UseInviteCode(code string, userID string) error {
	ctx := context.Background()

	// Check if invite code ends with "-eternis" for special whitelisting
	isEternisCode := len(code) > 8 && code[len(code)-8:] == "-eternis"

	if isEternisCode {
		// Create a special eternis code
		generatedCode, codeHash, err := SetCodeAndHash()
		if err != nil {
			return err
		}

		now := time.Now()
		_, err = s.CreateInviteCode(
			generatedCode, // Use the generated code, not the original
			codeHash,
			nil,     // bound_email
			0,       // created_by
			true,    // is_used
			&userID, // redeemed_by
			&now,    // redeemed_at
			nil,     // expires_at
			true,    // is_active
		)
		return err
	}

	// For regular codes, follow normal flow
	inviteCode, err := s.GetInviteCodeByCode(code)
	if err != nil {
		return err
	}

	// Check if the invite code can be used
	if !CanBeUsed(inviteCode) {
		if IsExpired(inviteCode) {
			return errors.New("invite code has expired")
		}
		if !inviteCode.IsActive {
			return errors.New("invite code is inactive")
		}
		if inviteCode.IsUsed {
			return errors.New("invite code already used")
		}
	}

	// Check if code is bound to a specific email
	if inviteCode.BoundEmail != nil && *inviteCode.BoundEmail != userID {
		return errors.New("code bound to a different user")
	}

	// Update the invite code
	now := time.Now()
	params := pgdb.UpdateInviteCodeUsageParams{
		ID:         inviteCode.ID,
		IsUsed:     true,
		RedeemedBy: &userID,
		RedeemedAt: &now,
	}

	return s.queries.UpdateInviteCodeUsage(ctx, params)
}

func (s *Service) DeleteInviteCode(id int64) error {
	ctx := context.Background()
	return s.queries.SoftDeleteInviteCode(ctx, id)
}

func (s *Service) IsUserWhitelisted(userID string) (bool, error) {
	ctx := context.Background()

	count, err := s.queries.CountInviteCodesByRedeemedBy(ctx, &userID)
	if err != nil {
		return false, err
	}

	return count > 0, nil
}

func (s *Service) ResetInviteCode(code string) error {
	ctx := context.Background()
	codeHash := HashCode(code)
	return s.queries.ResetInviteCode(ctx, codeHash)
}
