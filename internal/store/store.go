// Package store は Firestore への読み書きを担当する。
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sinmetalcraft/champions/internal/model"
)

var (
	// ErrNotFound は対象の Document が存在しないときに返る。
	ErrNotFound = errors.New("store: not found")
	// ErrAlreadyExists は既に同じ Document が存在するときに返る。
	ErrAlreadyExists = errors.New("store: already exists")
)

// Store は Firestore クライアントのラッパ。
type Store struct {
	fs *firestore.Client
}

// New は Firestore に接続した Store を作る。
// databaseID には Firestore のデータベース ID を渡す。既定のデータベースは "(default)"。
func New(ctx context.Context, projectID, databaseID string) (*Store, error) {
	fs, err := firestore.NewClientWithDatabase(ctx, projectID, databaseID)
	if err != nil {
		return nil, fmt.Errorf("store: failed to create firestore client for %s/%s: %w", projectID, databaseID, err)
	}
	return &Store{fs: fs}, nil
}

// Close は Firestore クライアントを閉じる。
func (s *Store) Close() error { return s.fs.Close() }

// CreateEvent はイベントを新規登録する。同じコードのイベントが既にある場合は ErrAlreadyExists を返す。
func (s *Store) CreateEvent(ctx context.Context, e *model.Event) error {
	now := time.Now()
	e.CreatedAt = now
	e.UpdatedAt = now
	if _, err := s.eventDoc(e.Code).Create(ctx, e); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("event %s: %w", e.Code, ErrAlreadyExists)
		}
		return fmt.Errorf("store: failed to create event %s: %w", e.Code, err)
	}
	return nil
}

// UpdateEvent はイベントを上書きする。存在しない場合は ErrNotFound を返す。
func (s *Store) UpdateEvent(ctx context.Context, e *model.Event) error {
	e.UpdatedAt = time.Now()
	if _, err := s.eventDoc(e.Code).Set(ctx, e); err != nil {
		return fmt.Errorf("store: failed to update event %s: %w", e.Code, err)
	}
	return nil
}

// GetEvent はイベントを取得する。存在しない場合は ErrNotFound を返す。
func (s *Store) GetEvent(ctx context.Context, code string) (*model.Event, error) {
	doc, err := s.eventDoc(code).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("event %s: %w", code, ErrNotFound)
		}
		return nil, fmt.Errorf("store: failed to get event %s: %w", code, err)
	}
	var e model.Event
	if err := doc.DataTo(&e); err != nil {
		return nil, fmt.Errorf("store: failed to decode event %s: %w", code, err)
	}
	e.Code = doc.Ref.ID
	return &e, nil
}

// ListEvents は全てのイベントをコード順で返す。
func (s *Store) ListEvents(ctx context.Context) ([]*model.Event, error) {
	iter := s.fs.Collection(model.KindEvent).OrderBy(firestore.DocumentID, firestore.Asc).Documents(ctx)
	defer iter.Stop()

	events := make([]*model.Event, 0)
	for {
		doc, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			return events, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: failed to list events: %w", err)
		}
		var e model.Event
		if err := doc.DataTo(&e); err != nil {
			return nil, fmt.Errorf("store: failed to decode event %s: %w", doc.Ref.ID, err)
		}
		e.Code = doc.Ref.ID
		events = append(events, &e)
	}
}

// DeleteEvent はイベントを削除する。払い出し済みの Project やフォルダは削除しない。
func (s *Store) DeleteEvent(ctx context.Context, code string) error {
	if _, err := s.eventDoc(code).Delete(ctx); err != nil {
		return fmt.Errorf("store: failed to delete event %s: %w", code, err)
	}
	return nil
}

// CreateAllocation は払い出しレコードを新規作成する。
// 同じユーザが同じイベントで既に払い出しを受けている場合は ErrAlreadyExists を返す。
func (s *Store) CreateAllocation(ctx context.Context, a *model.Allocation) error {
	now := time.Now()
	a.CreatedAt = now
	a.UpdatedAt = now
	a.ExpireAt = model.AllocationExpireAt(now)
	if _, err := s.allocationDoc(a.ID).Create(ctx, a); err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return fmt.Errorf("allocation %s: %w", a.ID, ErrAlreadyExists)
		}
		return fmt.Errorf("store: failed to create allocation %s: %w", a.ID, err)
	}
	return nil
}

// UpdateAllocation は払い出しレコードを上書きする。
func (s *Store) UpdateAllocation(ctx context.Context, a *model.Allocation) error {
	a.UpdatedAt = time.Now()
	if _, err := s.allocationDoc(a.ID).Set(ctx, a); err != nil {
		return fmt.Errorf("store: failed to update allocation %s: %w", a.ID, err)
	}
	return nil
}

// GetAllocation は払い出しレコードを取得する。存在しない場合は ErrNotFound を返す。
func (s *Store) GetAllocation(ctx context.Context, id string) (*model.Allocation, error) {
	doc, err := s.allocationDoc(id).Get(ctx)
	if err != nil {
		if status.Code(err) == codes.NotFound {
			return nil, fmt.Errorf("allocation %s: %w", id, ErrNotFound)
		}
		return nil, fmt.Errorf("store: failed to get allocation %s: %w", id, err)
	}
	return decodeAllocation(doc)
}

// ListAllocationsByUser はユーザが受け取った払い出しを新しい順に返す。
func (s *Store) ListAllocationsByUser(ctx context.Context, email string) ([]*model.Allocation, error) {
	q := s.fs.Collection(model.KindAllocation).
		Where("UserEmail", "==", email).
		OrderBy("CreatedAt", firestore.Desc)
	return s.queryAllocations(ctx, q)
}

// ListAllocationsByEvent はイベントで払い出した Project を新しい順に返す。
func (s *Store) ListAllocationsByEvent(ctx context.Context, eventCode string) ([]*model.Allocation, error) {
	q := s.fs.Collection(model.KindAllocation).
		Where("EventCode", "==", eventCode).
		OrderBy("CreatedAt", firestore.Desc)
	return s.queryAllocations(ctx, q)
}

// CountAllocationsByEvent はイベントで払い出した Project 数を返す。
func (s *Store) CountAllocationsByEvent(ctx context.Context, eventCode string) (int64, error) {
	q := s.fs.Collection(model.KindAllocation).Where("EventCode", "==", eventCode)
	result, err := q.NewAggregationQuery().WithCount("count").Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("store: failed to count allocations of %s: %w", eventCode, err)
	}
	var count struct {
		Count int64 `firestore:"count"`
	}
	if err := result.DataTo(&count); err != nil {
		return 0, fmt.Errorf("store: failed to decode count of %s: %w", eventCode, err)
	}
	return count.Count, nil
}

func (s *Store) queryAllocations(ctx context.Context, q firestore.Query) ([]*model.Allocation, error) {
	iter := q.Documents(ctx)
	defer iter.Stop()

	allocations := make([]*model.Allocation, 0)
	for {
		doc, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			return allocations, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: failed to list allocations: %w", err)
		}
		a, err := decodeAllocation(doc)
		if err != nil {
			return nil, err
		}
		allocations = append(allocations, a)
	}
}

func decodeAllocation(doc *firestore.DocumentSnapshot) (*model.Allocation, error) {
	var a model.Allocation
	if err := doc.DataTo(&a); err != nil {
		return nil, fmt.Errorf("store: failed to decode allocation %s: %w", doc.Ref.ID, err)
	}
	a.ID = doc.Ref.ID
	return &a, nil
}

func (s *Store) eventDoc(code string) *firestore.DocumentRef {
	return s.fs.Collection(model.KindEvent).Doc(code)
}

func (s *Store) allocationDoc(id string) *firestore.DocumentRef {
	return s.fs.Collection(model.KindAllocation).Doc(id)
}

// DeleteAllocation は払い出しレコードを削除する。払い出し済みの Project は削除しない。
func (s *Store) DeleteAllocation(ctx context.Context, id string) error {
	if _, err := s.allocationDoc(id).Delete(ctx); err != nil {
		return fmt.Errorf("store: failed to delete allocation %s: %w", id, err)
	}
	return nil
}
