package idempotency

import (
	"bytes"
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

var (
	ErrInProgress  = errors.New("idempotent request is in progress")
	ErrKeyConflict = errors.New("idempotency key was used with a different request")
)

type Request struct {
	TokenID           uuid.UUID
	Method            string
	CanonicalResource string
	Key               string
	Fingerprint       []byte
	TTL               time.Duration
	RequestID         string
	ClassifyError     func(error) (ErrorResponse, bool)
	OnTerminal        func(context.Context, pgx.Tx, error) error
}

type ErrorResponse struct {
	Status int
	Title  string
	Code   string
}

type Response struct {
	Status  int
	Body    []byte
	Encrypt bool
}

type Result struct {
	Status   int
	Body     []byte
	Replayed bool
}

type CompletedError struct {
	Status   int
	Body     []byte
	Replayed bool
	cause    error
}

func (e *CompletedError) Error() string {
	if e.cause != nil {
		return e.cause.Error()
	}
	return fmt.Sprintf("completed idempotent request returned HTTP %d", e.Status)
}

func (e *CompletedError) Unwrap() error {
	return e.cause
}

func CompletedErrorFrom(err error) (*CompletedError, bool) {
	var completed *CompletedError
	if !errors.As(err, &completed) {
		return nil, false
	}
	copy := *completed
	copy.Body = cloneBody(completed.Body)
	return &copy, true
}

// BeginResult describes the outcome of starting an operation whose external
// side effects must happen after the pending record is committed.
type BeginResult struct {
	RecordID *uuid.UUID
	Replay   *Result
}

type Service struct {
	pool   *pgxpool.Pool
	sealer *Sealer
	now    func() time.Time
}

func NewService(pool *pgxpool.Pool, sealer *Sealer, now func() time.Time) *Service {
	return &Service{pool: pool, sealer: sealer, now: now}
}

// BeginInTx arbitrates an idempotency key without committing the surrounding
// transaction. Callers can create their durable recovery record in the same
// transaction, commit it, and perform external I/O afterwards.
func (s *Service) BeginInTx(ctx context.Context, tx pgx.Tx, request Request) (BeginResult, error) {
	if tx == nil {
		return BeginResult{}, fmt.Errorf("begin idempotency: transaction is nil")
	}
	if request.Key == "" {
		return BeginResult{}, nil
	}
	queries := db.New(tx)
	now := s.now().UTC()
	record, err := queries.InsertIdempotencyRecord(ctx, db.InsertIdempotencyRecordParams{
		TokenID:            request.TokenID,
		HttpMethod:         request.Method,
		CanonicalResource:  request.CanonicalResource,
		IdempotencyKey:     request.Key,
		RequestFingerprint: request.Fingerprint,
		ExpiresAt:          pgtype.Timestamptz{Time: now.Add(request.TTL), Valid: true},
	})
	inserted := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		record, err = queries.GetIdempotencyRecordForUpdate(ctx, db.GetIdempotencyRecordForUpdateParams{
			TokenID:           request.TokenID,
			HttpMethod:        request.Method,
			CanonicalResource: request.CanonicalResource,
			IdempotencyKey:    request.Key,
		})
	}
	if err != nil {
		return BeginResult{}, fmt.Errorf("arbitrate external idempotency record: %w", err)
	}
	if !bytes.Equal(record.RequestFingerprint, request.Fingerprint) {
		return BeginResult{}, ErrKeyConflict
	}
	if record.State == "completed" {
		result, err := s.replay(record, request)
		if err != nil {
			return BeginResult{}, err
		}
		return BeginResult{Replay: &result}, nil
	}
	if record.State != "pending" {
		return BeginResult{}, fmt.Errorf("unknown idempotency state %q", record.State)
	}
	if !inserted {
		return BeginResult{}, ErrInProgress
	}
	return BeginResult{RecordID: &record.ID}, nil
}

// CompleteInTx stores the final response for a previously begun operation.
// The caller owns the transaction and must commit it together with its domain
// changes.
func (s *Service) CompleteInTx(ctx context.Context, tx pgx.Tx, recordID *uuid.UUID, request Request, response Response) error {
	if tx == nil {
		return fmt.Errorf("complete idempotency: transaction is nil")
	}
	if recordID == nil {
		return nil
	}
	plaintext := cloneBody(response.Body)
	stored := plaintext
	var err error
	if response.Encrypt {
		stored, err = s.sealer.Seal(plaintext, responseAAD(request))
		if err != nil {
			return fmt.Errorf("seal idempotency response: %w", err)
		}
	}
	status := int32(response.Status)
	if _, err := db.New(tx).CompleteIdempotencyRecord(ctx, db.CompleteIdempotencyRecordParams{
		ID:                *recordID,
		HttpStatus:        &status,
		ResponseBody:      stored,
		ResponseEncrypted: response.Encrypt,
		CompletedAt:       pgtype.Timestamptz{Time: s.now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("complete external idempotency record: %w", err)
	}
	return nil
}

func (s *Service) RunInTx(
	ctx context.Context,
	request Request,
	fn func(context.Context, pgx.Tx) (Response, error),
) (Result, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return Result{}, fmt.Errorf("begin idempotent transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if request.Key == "" {
		return s.executeInTx(ctx, tx, request, nil, fn)
	}

	queries := db.New(tx)
	now := s.now().UTC()
	record, err := queries.InsertIdempotencyRecord(ctx, db.InsertIdempotencyRecordParams{
		TokenID:            request.TokenID,
		HttpMethod:         request.Method,
		CanonicalResource:  request.CanonicalResource,
		IdempotencyKey:     request.Key,
		RequestFingerprint: request.Fingerprint,
		ExpiresAt:          pgtype.Timestamptz{Time: now.Add(request.TTL), Valid: true},
	})
	inserted := err == nil
	if errors.Is(err, pgx.ErrNoRows) {
		record, err = queries.GetIdempotencyRecordForUpdate(ctx, db.GetIdempotencyRecordForUpdateParams{
			TokenID:           request.TokenID,
			HttpMethod:        request.Method,
			CanonicalResource: request.CanonicalResource,
			IdempotencyKey:    request.Key,
		})
	}
	if err != nil {
		return Result{}, fmt.Errorf("arbitrate idempotency record: %w", err)
	}

	if !bytes.Equal(record.RequestFingerprint, request.Fingerprint) {
		return Result{}, ErrKeyConflict
	}
	if record.State == "completed" {
		result, err := s.replay(record, request)
		if err != nil {
			return Result{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit idempotency replay: %w", err)
		}
		if result.Status >= 400 && result.Status < 500 {
			return Result{}, &CompletedError{Status: result.Status, Body: cloneBody(result.Body), Replayed: true}
		}
		return result, nil
	}
	if record.State != "pending" {
		return Result{}, fmt.Errorf("unknown idempotency state %q", record.State)
	}
	if !inserted {
		return Result{}, ErrInProgress
	}
	return s.executeInTx(ctx, tx, request, &record.ID, fn)
}

func (s *Service) executeInTx(
	ctx context.Context,
	tx pgx.Tx,
	request Request,
	recordID *uuid.UUID,
	fn func(context.Context, pgx.Tx) (Response, error),
) (Result, error) {
	work, err := tx.Begin(ctx)
	if err != nil {
		return Result{}, fmt.Errorf("begin idempotent work savepoint: %w", err)
	}
	response, callbackErr := fn(ctx, work)
	if callbackErr != nil {
		classification, terminal := classifyError(request, callbackErr)
		if !terminal {
			_ = work.Rollback(ctx)
			return Result{}, callbackErr
		}
		if err := work.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			return Result{}, fmt.Errorf("rollback terminal work savepoint: %w", err)
		}
		if request.OnTerminal != nil {
			if err := request.OnTerminal(ctx, tx, callbackErr); err != nil {
				return Result{}, fmt.Errorf("finalize terminal idempotent request: %w", err)
			}
		}
		body, err := encodeErrorResponse(classification, request.RequestID)
		if err != nil {
			return Result{}, err
		}
		response = Response{Status: classification.Status, Body: body}
		if err := s.completeResponse(ctx, tx, recordID, request, response); err != nil {
			return Result{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return Result{}, fmt.Errorf("commit terminal idempotent transaction: %w", err)
		}
		return Result{}, &CompletedError{
			Status: response.Status,
			Body:   cloneBody(response.Body),
			cause:  callbackErr,
		}
	}
	if err := work.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit idempotent work savepoint: %w", err)
	}
	if err := s.completeResponse(ctx, tx, recordID, request, response); err != nil {
		return Result{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Result{}, fmt.Errorf("commit idempotent transaction: %w", err)
	}
	return Result{Status: response.Status, Body: cloneBody(response.Body)}, nil
}

func (s *Service) completeResponse(
	ctx context.Context,
	tx pgx.Tx,
	recordID *uuid.UUID,
	request Request,
	response Response,
) error {
	if recordID == nil {
		return nil
	}
	plaintext := cloneBody(response.Body)
	stored := plaintext
	var err error
	if response.Encrypt {
		stored, err = s.sealer.Seal(plaintext, responseAAD(request))
		if err != nil {
			return fmt.Errorf("seal idempotency response: %w", err)
		}
	}
	status := int32(response.Status)
	if _, err := db.New(tx).CompleteIdempotencyRecord(ctx, db.CompleteIdempotencyRecordParams{
		ID:                *recordID,
		HttpStatus:        &status,
		ResponseBody:      stored,
		ResponseEncrypted: response.Encrypt,
		CompletedAt:       pgtype.Timestamptz{Time: s.now().UTC(), Valid: true},
	}); err != nil {
		return fmt.Errorf("complete idempotency record: %w", err)
	}
	return nil
}

func classifyError(request Request, err error) (ErrorResponse, bool) {
	if request.ClassifyError == nil {
		return ErrorResponse{}, false
	}
	response, ok := request.ClassifyError(err)
	if !ok {
		return ErrorResponse{}, false
	}
	if response.Status < 400 || response.Status >= 500 || response.Title == "" || response.Code == "" {
		return ErrorResponse{}, false
	}
	return response, true
}

func encodeErrorResponse(response ErrorResponse, requestID string) ([]byte, error) {
	body, err := json.Marshal(struct {
		Type      string `json:"type"`
		Title     string `json:"title"`
		Status    int    `json:"status"`
		Code      string `json:"code"`
		RequestID string `json:"requestId"`
	}{
		Type:      "about:blank",
		Title:     response.Title,
		Status:    response.Status,
		Code:      response.Code,
		RequestID: requestID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode terminal idempotency response: %w", err)
	}
	return body, nil
}

func (s *Service) replay(record db.IdempotencyRecord, request Request) (Result, error) {
	if record.HttpStatus == nil {
		return Result{}, fmt.Errorf("completed idempotency record %s has no HTTP status", record.ID)
	}
	body := cloneBody(record.ResponseBody)
	if record.ResponseEncrypted {
		var err error
		body, err = s.sealer.Open(body, responseAAD(request))
		if err != nil {
			return Result{}, fmt.Errorf("decrypt idempotency replay: %w", err)
		}
	}
	return Result{Status: int(*record.HttpStatus), Body: body, Replayed: true}, nil
}

func responseAAD(request Request) []byte {
	aad, _ := json.Marshal(struct {
		TokenID           uuid.UUID `json:"tokenId"`
		Method            string    `json:"method"`
		CanonicalResource string    `json:"canonicalResource"`
		Key               string    `json:"key"`
		Fingerprint       []byte    `json:"fingerprint"`
	}{
		TokenID:           request.TokenID,
		Method:            request.Method,
		CanonicalResource: request.CanonicalResource,
		Key:               request.Key,
		Fingerprint:       request.Fingerprint,
	})
	return aad
}

func cloneBody(body []byte) []byte {
	cloned := make([]byte, len(body))
	copy(cloned, body)
	return cloned
}
