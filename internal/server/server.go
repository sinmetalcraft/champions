// Package server は一般公開アプリケーションの HTTP ハンドラを提供する。
package server

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sinmetalcraft/champions/internal/config"
	"github.com/sinmetalcraft/champions/internal/httpx"
	"github.com/sinmetalcraft/champions/internal/iap"
	"github.com/sinmetalcraft/champions/internal/model"
	"github.com/sinmetalcraft/champions/internal/provision"
	"github.com/sinmetalcraft/champions/internal/shutdown"
	"github.com/sinmetalcraft/champions/internal/store"
	"github.com/sinmetalcraft/champions/internal/tasks"
)

//go:embed static
var staticFS embed.FS

// Enqueuer は払い出し処理を非同期実行に回す。
// 本番では Cloud Tasks に投入し、ローカル開発では provision.LocalDispatcher がその場で実行する。
type Enqueuer interface {
	EnqueueProvision(ctx context.Context, allocationID string) error
}

// Server は一般公開アプリケーション。
// Cloud Tasks から呼ばれる worker も兼ねるため、払い出しと後片付けの両方を持つ。
type Server struct {
	cfg         *config.Config
	store       *store.Store
	queue       Enqueuer
	provisioner *provision.Provisioner
	shutdowner  *shutdown.Shutdowner
}

// New は Server を作る。
func New(cfg *config.Config, s *store.Store, q Enqueuer, p *provision.Provisioner, sd *shutdown.Shutdowner) *Server {
	return &Server{cfg: cfg, store: s, queue: q, provisioner: p, shutdowner: sd}
}

// Handler はルーティングを組み立てる。
// /tasks/ 配下は Cloud Tasks からの呼び出しなので IAP ではなく OIDC トークンで認証する。
func (s *Server) Handler(auth *iap.Authenticator, verifier *tasks.Verifier) http.Handler {
	app := http.NewServeMux()
	app.Handle("GET /api/me", httpx.Handler(s.handleMe))
	app.Handle("GET /api/allocations", httpx.Handler(s.handleListAllocations))
	app.Handle("POST /api/allocations", httpx.Handler(s.handleCreateAllocation))
	app.Handle("GET /api/allocations/{id}", httpx.Handler(s.handleGetAllocation))
	app.Handle("GET /", s.staticHandler())

	worker := http.NewServeMux()
	worker.Handle("POST "+tasks.ProvisionPath, httpx.Handler(s.handleProvision))
	worker.Handle("POST "+tasks.ShutdownPath, httpx.Handler(s.handleShutdown))

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
	mux.Handle("/tasks/", verifier.Middleware(worker))
	mux.Handle("/", auth.Middleware(app))
	return mux
}

func (s *Server) staticHandler() http.Handler {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		panic(err)
	}
	return http.FileServerFS(sub)
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) error {
	u, err := iap.FromContext(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"email": u.Email})
	return nil
}

func (s *Server) handleListAllocations(w http.ResponseWriter, r *http.Request) error {
	u, err := iap.FromContext(r.Context())
	if err != nil {
		return err
	}
	allocations, err := s.store.ListAllocationsByUser(r.Context(), u.Email)
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, allocations)
	return nil
}

func (s *Server) handleGetAllocation(w http.ResponseWriter, r *http.Request) error {
	u, err := iap.FromContext(r.Context())
	if err != nil {
		return err
	}
	a, err := s.store.GetAllocation(r.Context(), r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return httpx.Errorf(http.StatusNotFound, "allocation is not found")
		}
		return err
	}
	if !strings.EqualFold(a.UserEmail, u.Email) {
		// 他人の払い出しは存在自体を知らせない。
		return httpx.Errorf(http.StatusNotFound, "allocation is not found")
	}
	httpx.WriteJSON(w, http.StatusOK, a)
	return nil
}

// createAllocationRequest は払い出しリクエストのボディ。
type createAllocationRequest struct {
	// EventCode は参加するハンズオンのイベントコード。
	EventCode string `json:"eventCode"`
}

func (s *Server) handleCreateAllocation(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	u, err := iap.FromContext(ctx)
	if err != nil {
		return err
	}

	var req createAllocationRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	code := strings.ToLower(strings.TrimSpace(req.EventCode))
	if err := model.ValidateEventCode(code); err != nil {
		return httpx.WrapError(http.StatusBadRequest, err, "event code is invalid")
	}

	e, err := s.store.GetEvent(ctx, code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return httpx.Errorf(http.StatusNotFound, "event %s is not found", code)
		}
		return err
	}
	if !e.Enabled {
		return httpx.Errorf(http.StatusForbidden, "event %s does not accept allocation now", code)
	}
	if e.FolderName == "" {
		return httpx.Errorf(http.StatusConflict, "event %s does not have a folder yet", code)
	}

	id := model.AllocationID(code, u.Email)
	if existing, err := s.store.GetAllocation(ctx, id); err == nil {
		// 二重に押されても同じ払い出しを返す。
		httpx.WriteJSON(w, http.StatusOK, existing)
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}

	if e.MaxAllocations > 0 {
		count, err := s.store.CountAllocationsByEvent(ctx, code)
		if err != nil {
			return err
		}
		if count >= int64(e.MaxAllocations) {
			return httpx.Errorf(http.StatusForbidden, "event %s reached the maximum number of allocations", code)
		}
	}

	a := &model.Allocation{
		ID:         id,
		EventCode:  code,
		UserEmail:  strings.ToLower(u.Email),
		UserID:     u.ID,
		FolderName: e.FolderName,
		Status:     model.AllocationStatusPending,
		Step:       model.StepQueued,
	}
	if err := s.store.CreateAllocation(ctx, a); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			existing, err := s.store.GetAllocation(ctx, id)
			if err != nil {
				return err
			}
			httpx.WriteJSON(w, http.StatusOK, existing)
			return nil
		}
		return err
	}

	if err := s.queue.EnqueueProvision(ctx, a.ID); err != nil {
		// タスクを積めないと PENDING のまま残り続けるので、レコードを消してリトライできるようにする。
		if derr := s.store.DeleteAllocation(ctx, a.ID); derr != nil {
			slog.Error("failed to delete allocation after enqueue failure", "allocationID", a.ID, "error", derr.Error())
		}
		return err
	}

	httpx.WriteJSON(w, http.StatusAccepted, a)
	return nil
}

// handleShutdown はイベントで払い出した Project をまとめて削除依頼状態にする。Cloud Tasks から呼ばれる。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) error {
	var req tasks.ShutdownRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.EventCode == "" {
		return httpx.Errorf(http.StatusBadRequest, "eventCode is required")
	}
	if err := s.shutdowner.Run(r.Context(), req.EventCode); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}

func (s *Server) handleProvision(w http.ResponseWriter, r *http.Request) error {
	var req tasks.ProvisionRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if req.AllocationID == "" {
		return httpx.Errorf(http.StatusBadRequest, "allocationID is required")
	}
	if err := s.provisioner.Run(r.Context(), req.AllocationID, tasks.RetryCount(r)); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	return nil
}
