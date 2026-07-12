package audit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	db "superfan.myasustor.com/fanchao/artifact-repository/internal/database/sqlc"
)

const (
	defaultPageLimit int32 = 50
	maxPageLimit     int32 = 200
)

var ErrInvalidPage = errors.New("invalid audit page request")

type Outcome string

const (
	OutcomeSuccess Outcome = "success"
	OutcomeDenied  Outcome = "denied"
	OutcomeFailed  Outcome = "failed"
)

type Event struct {
	ActorTokenID uuid.UUID
	RepositoryID uuid.UUID
	Action       string
	ResourceType string
	ResourceID   string
	Outcome      Outcome
	Code         string
	RequestID    string
	Details      map[string]any
}

type Entry struct {
	ID           uuid.UUID
	ActorTokenID *uuid.UUID
	RepositoryID *uuid.UUID
	Action       string
	ResourceType string
	ResourceID   *string
	Outcome      Outcome
	Code         *string
	RequestID    string
	Details      map[string]any
	CreatedAt    time.Time
}

type Cursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type ListRequest struct {
	After *Cursor
	Limit int32
}

type Page struct {
	Items []Entry
	Next  *Cursor
}

type Service struct {
	pool *pgxpool.Pool
}

func NewService(pool *pgxpool.Pool) *Service {
	return &Service{pool: pool}
}

func (s *Service) Record(ctx context.Context, tx pgx.Tx, event Event) (Entry, error) {
	details := event.Details
	if details == nil {
		details = map[string]any{}
	}
	encodedDetails, err := json.Marshal(details)
	if err != nil {
		return Entry{}, fmt.Errorf("encode audit details: %w", err)
	}

	created, err := db.New(tx).CreateAuditEvent(ctx, db.CreateAuditEventParams{
		ActorTokenID: optionalUUID(event.ActorTokenID),
		RepositoryID: optionalUUID(event.RepositoryID),
		Action:       event.Action,
		ResourceType: event.ResourceType,
		ResourceID:   optionalString(event.ResourceID),
		Outcome:      string(event.Outcome),
		Code:         optionalString(event.Code),
		RequestID:    event.RequestID,
		Details:      encodedDetails,
	})
	if err != nil {
		return Entry{}, fmt.Errorf("create audit event: %w", err)
	}
	return entryFromRow(created)
}

func (s *Service) RecordStandalone(ctx context.Context, event Event) (Entry, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Entry{}, fmt.Errorf("begin audit transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	entry, err := s.Record(ctx, tx, event)
	if err != nil {
		return Entry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Entry{}, fmt.Errorf("commit audit transaction: %w", err)
	}
	return entry, nil
}

func (s *Service) List(ctx context.Context, request ListRequest) (Page, error) {
	limit, err := pageLimit(request.Limit)
	if err != nil {
		return Page{}, err
	}
	params := db.ListAuditEventsParams{
		AfterID:   uuid.Nil,
		PageLimit: limit + 1,
	}
	if request.After != nil {
		if request.After.ID == uuid.Nil || request.After.CreatedAt.IsZero() {
			return Page{}, ErrInvalidPage
		}
		params.AfterCreatedAt = pgtype.Timestamptz{Time: request.After.CreatedAt, Valid: true}
		params.AfterID = request.After.ID
	}

	rows, err := db.New(s.pool).ListAuditEvents(ctx, params)
	if err != nil {
		return Page{}, fmt.Errorf("list audit events: %w", err)
	}
	page := Page{Items: make([]Entry, 0, min(len(rows), int(limit)))}
	for _, row := range rows[:min(len(rows), int(limit))] {
		entry, err := entryFromRow(row)
		if err != nil {
			return Page{}, err
		}
		page.Items = append(page.Items, entry)
	}
	if len(rows) > int(limit) {
		last := page.Items[len(page.Items)-1]
		page.Next = &Cursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
	return page, nil
}

func entryFromRow(row db.AuditEvent) (Entry, error) {
	details := map[string]any{}
	if err := json.Unmarshal(row.Details, &details); err != nil {
		return Entry{}, fmt.Errorf("decode audit event %s details: %w", row.ID, err)
	}
	return Entry{
		ID:           row.ID,
		ActorTokenID: row.ActorTokenID,
		RepositoryID: row.RepositoryID,
		Action:       row.Action,
		ResourceType: row.ResourceType,
		ResourceID:   row.ResourceID,
		Outcome:      Outcome(row.Outcome),
		Code:         row.Code,
		RequestID:    row.RequestID,
		Details:      details,
		CreatedAt:    row.CreatedAt.Time.UTC(),
	}, nil
}

func pageLimit(value int32) (int32, error) {
	if value == 0 {
		return defaultPageLimit, nil
	}
	if value < 1 || value > maxPageLimit {
		return 0, ErrInvalidPage
	}
	return value, nil
}

func optionalUUID(value uuid.UUID) *uuid.UUID {
	if value == uuid.Nil {
		return nil
	}
	return &value
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
