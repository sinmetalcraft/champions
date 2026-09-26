// Package admin は Admin アプリケーションの HTTP ハンドラを提供する。
package admin

import (
	"context"
	"embed"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sinmetalcraft/champions/internal/config"
	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/httpx"
	"github.com/sinmetalcraft/champions/internal/iap"
	"github.com/sinmetalcraft/champions/internal/model"
	"github.com/sinmetalcraft/champions/internal/store"
)

//go:embed static
var staticFS embed.FS

// Enqueuer は時間のかかる処理を非同期実行に回す。
// 本番では Cloud Tasks に投入し、ローカル開発では tasks.LocalDispatcher がその場で実行する。
type Enqueuer interface {
	// EnqueueProvision は払い出し処理をやり直す。
	EnqueueProvision(ctx context.Context, allocationID string) error
	// EnqueueReissue は Project を作り直す。
	EnqueueReissue(ctx context.Context, allocationID, projectID string) error
	// EnqueueShutdown はイベントの Project をまとめて片付ける。
	EnqueueShutdown(ctx context.Context, eventCode string) error
	// EnqueueSync はイベントの払い出し済み Project に設定を同期する。
	EnqueueSync(ctx context.Context, eventCode string) error
}

// Server は Admin アプリケーション。
type Server struct {
	cfg   *config.Config
	store *store.Store
	gcp   *gcp.Client
	queue Enqueuer
}

// New は Admin の Server を作る。
func New(cfg *config.Config, s *store.Store, g *gcp.Client, q Enqueuer) *Server {
	return &Server{cfg: cfg, store: s, gcp: g, queue: q}
}

// Handler はルーティングを組み立てる。
func (s *Server) Handler(auth *iap.Authenticator) http.Handler {
	app := http.NewServeMux()
	app.Handle("GET /api/me", httpx.Handler(s.handleMe))
	app.Handle("GET /api/events", httpx.Handler(s.handleListEvents))
	app.Handle("POST /api/events", httpx.Handler(s.handleCreateEvent))
	app.Handle("GET /api/events/{code}", httpx.Handler(s.handleGetEvent))
	app.Handle("PUT /api/events/{code}", httpx.Handler(s.handleUpdateEvent))
	app.Handle("DELETE /api/events/{code}", httpx.Handler(s.handleDeleteEvent))
	app.Handle("POST /api/events/{code}/folder", httpx.Handler(s.handleEnsureFolder))
	app.Handle("GET /api/events/{code}/allocations", httpx.Handler(s.handleListAllocations))
	app.Handle("POST /api/events/{code}/sync", httpx.Handler(s.handleSync))
	app.Handle("POST /api/events/{code}/shutdown", httpx.Handler(s.handleShutdown))
	app.Handle("POST /api/events/{code}/allocations/{id}/retry", httpx.Handler(s.handleRetry))
	app.Handle("POST /api/events/{code}/allocations/{id}/reissue", httpx.Handler(s.handleReissue))
	app.Handle("GET /", s.staticHandler())

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", httpx.Handler(func(w http.ResponseWriter, r *http.Request) error {
		httpx.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return nil
	}))
	mux.Handle("/", auth.Middleware(s.requireAdmin(app)))
	return mux
}

// requireAdmin は ADMIN_EMAILS の許可リストで追加のチェックを行う。
func (s *Server) requireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := iap.FromContext(r.Context())
		if err == nil && !s.cfg.IsAdmin(u.Email) {
			err = httpx.Errorf(http.StatusForbidden, "%s is not an admin", u.Email)
		}
		if err != nil {
			httpx.Handler(func(http.ResponseWriter, *http.Request) error { return err }).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
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

// eventRequest はイベントの登録・更新リクエストのボディ。
type eventRequest struct {
	// Code はイベントコード。更新時は URL のコードが優先される。
	Code string `json:"code"`
	// DisplayName は画面表示用のイベント名。
	DisplayName string `json:"displayName"`
	// Enabled が false のイベントは払い出しを受け付けない。
	Enabled bool `json:"enabled"`
	// MaxAllocations は払い出せる Project 数の上限。0 は無制限。
	MaxAllocations int `json:"maxAllocations"`
	// Roles は参加者に付与する IAM Role。
	Roles []string `json:"roles"`
	// APIs は有効化するサービス。
	APIs []string `json:"apis"`
	// Quotas は適用する Quota。
	Quotas []model.Quota `json:"quotas"`
}

func (req *eventRequest) applyTo(e *model.Event) {
	e.DisplayName = strings.TrimSpace(req.DisplayName)
	e.Enabled = req.Enabled
	e.MaxAllocations = req.MaxAllocations
	e.Roles = normalizeList(req.Roles)
	e.APIs = normalizeList(req.APIs)
	e.Quotas = req.Quotas
	if e.Quotas == nil {
		e.Quotas = []model.Quota{}
	}
}

func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) error {
	events, err := s.store.ListEvents(r.Context())
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, events)
	return nil
}

func (s *Server) handleGetEvent(w http.ResponseWriter, r *http.Request) error {
	e, err := s.getEvent(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, e)
	return nil
}

// handleCreateEvent はイベントを登録し、払い出した Project を入れるフォルダを作る。
func (s *Server) handleCreateEvent(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req eventRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	e := &model.Event{Code: strings.ToLower(strings.TrimSpace(req.Code))}
	req.applyTo(e)
	if err := e.Validate(); err != nil {
		return httpx.WrapError(http.StatusBadRequest, err, "%v", err)
	}
	if e.DisplayName == "" {
		e.DisplayName = e.Code
	}

	// 先にイベントコードを押さえてからフォルダを作る。
	// フォルダ作成に失敗しても POST /api/events/{code}/folder でやり直せる。
	if err := s.store.CreateEvent(ctx, e); err != nil {
		if errors.Is(err, store.ErrAlreadyExists) {
			return httpx.Errorf(http.StatusConflict, "event %s already exists", e.Code)
		}
		return err
	}

	folderName, err := s.ensureEventFolder(ctx, e.Code)
	if err != nil {
		return httpx.WrapError(http.StatusInternalServerError, err, "event %s is created but failed to create a folder. retry POST /api/events/%s/folder", e.Code, e.Code)
	}
	e.FolderName = folderName
	if err := s.store.UpdateEvent(ctx, e); err != nil {
		return err
	}

	httpx.WriteJSON(w, http.StatusCreated, e)
	return nil
}

func (s *Server) handleUpdateEvent(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	e, err := s.getEvent(ctx, r.PathValue("code"))
	if err != nil {
		return err
	}

	var req eventRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}

	prevRoles := e.Roles
	prevAPIs := e.APIs
	prevQuotas := e.Quotas

	req.applyTo(e)
	if err := e.Validate(); err != nil {
		return httpx.WrapError(http.StatusBadRequest, err, "%v", err)
	}
	if e.DisplayName == "" {
		e.DisplayName = e.Code
	}
	if err := s.store.UpdateEvent(ctx, e); err != nil {
		return err
	}

	if hasSettingsChanged(prevRoles, prevAPIs, prevQuotas, e.Roles, e.APIs, e.Quotas) {
		if err := s.queue.EnqueueSync(ctx, e.Code); err != nil {
			slog.Error("failed to enqueue sync after event update", "eventCode", e.Code, "error", err.Error())
		} else {
			slog.Info("sync is enqueued after event update", "eventCode", e.Code)
		}
	}

	httpx.WriteJSON(w, http.StatusOK, e)
	return nil
}

// handleDeleteEvent はイベントの設定を削除する。払い出し済みの Project とフォルダは残る。
func (s *Server) handleDeleteEvent(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	code := r.PathValue("code")
	if _, err := s.getEvent(ctx, code); err != nil {
		return err
	}
	if err := s.store.DeleteEvent(ctx, code); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// handleEnsureFolder はイベント用のフォルダを作り直す。作成済みの場合は既存のフォルダを紐付ける。
func (s *Server) handleEnsureFolder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	e, err := s.getEvent(ctx, r.PathValue("code"))
	if err != nil {
		return err
	}
	folderName, err := s.ensureEventFolder(ctx, e.Code)
	if err != nil {
		return err
	}
	e.FolderName = folderName
	if err := s.store.UpdateEvent(ctx, e); err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, e)
	return nil
}

func (s *Server) handleListAllocations(w http.ResponseWriter, r *http.Request) error {
	allocations, err := s.store.ListAllocationsByEvent(r.Context(), r.PathValue("code"))
	if err != nil {
		return err
	}
	httpx.WriteJSON(w, http.StatusOK, allocations)
	return nil
}

// syncResponse は Sync の受け付け結果。
type syncResponse struct {
	// EventCode は設定を適用する対象のイベント。
	EventCode string `json:"eventCode"`
	// Targets は設定を適用する Project の数。
	Targets int `json:"targets"`
}

// handleSync はイベントの設定を既存の Project に適用するタスクを Cloud Tasks に投入する。
func (s *Server) handleSync(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	e, err := s.getEvent(ctx, r.PathValue("code"))
	if err != nil {
		return err
	}

	allocations, err := s.store.ListAllocationsByEvent(ctx, e.Code)
	if err != nil {
		return err
	}
	targets := 0
	for _, a := range allocations {
		if a.ProjectID != "" && a.Status != model.AllocationStatusShutdown {
			targets++
		}
	}

	if err := s.queue.EnqueueSync(ctx, e.Code); err != nil {
		return err
	}
	slog.Info("sync is enqueued", "eventCode", e.Code, "targets", targets)

	httpx.WriteJSON(w, http.StatusAccepted, &syncResponse{EventCode: e.Code, Targets: targets})
	return nil
}

// shutdownResponse は Shutdown の受け付け結果。
type shutdownResponse struct {
	// EventCode は片付ける対象のイベント。
	EventCode string `json:"eventCode"`
	// Targets はこれから削除する Project の数。
	Targets int `json:"targets"`
}

// handleShutdown はイベントに紐づく Project の片付けを Cloud Tasks に投入する。
// ハンズオンが終わった後の後片付けに使う。削除依頼から 30 日間は復元できる。
//
// Project の削除は 1 件ずつ順番に行うため、数が多いと数分かかることがある。
// リクエスト内で待たずに worker へ渡し、進捗は払い出し状況の Status で確認する。
//
// 削除中に新しい払い出しが走らないよう、投入前にイベントの受付を止める。
// フォルダとイベントの設定は残すため、必要なら同じイベントコードで払い出しを再開できる。
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	e, err := s.getEvent(ctx, r.PathValue("code"))
	if err != nil {
		return err
	}

	if e.Enabled {
		e.Enabled = false
		if err := s.store.UpdateEvent(ctx, e); err != nil {
			return err
		}
		slog.Info("event is disabled before shutdown", "eventCode", e.Code)
	}

	allocations, err := s.store.ListAllocationsByEvent(ctx, e.Code)
	if err != nil {
		return err
	}
	targets := 0
	for _, a := range allocations {
		if a.ProjectID != "" && a.Status != model.AllocationStatusShutdown {
			targets++
		}
	}

	if err := s.queue.EnqueueShutdown(ctx, e.Code); err != nil {
		return err
	}
	slog.Info("shutdown is enqueued", "eventCode", e.Code, "targets", targets)

	httpx.WriteJSON(w, http.StatusAccepted, &shutdownResponse{EventCode: e.Code, Targets: targets})
	return nil
}

// handleRetry は止まってしまった払い出しをやり直す。
// 今の Project をそのまま使って、失敗したステップから先を作り直す。
func (s *Server) handleRetry(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a, err := s.getRestartableAllocation(ctx, r)
	if err != nil {
		return err
	}

	a.Status = model.AllocationStatusPending
	a.Step = model.StepQueued
	a.Attempts = 0
	a.Error = ""
	if err := s.store.UpdateAllocation(ctx, a); err != nil {
		return err
	}
	if err := s.queue.EnqueueProvision(ctx, a.ID); err != nil {
		return err
	}
	slog.Info("provisioning is re-enqueued", "allocationID", a.ID, "projectID", a.ProjectID)

	httpx.WriteJSON(w, http.StatusAccepted, a)
	return nil
}

// handleReissue は今の Project を落として、同じユーザに新しい Project を払い出す。
// 参加者が最初の Project でハンズオンを進められなくなったときに使う。
func (s *Server) handleReissue(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	a, err := s.getRestartableAllocation(ctx, r)
	if err != nil {
		return err
	}

	// worker に渡す前の ProjectID が削除対象。作り直しで作った Project を消さないよう payload で固定する。
	oldProjectID := a.ProjectID

	a.Status = model.AllocationStatusPending
	a.Step = model.StepReissue
	a.Attempts = 0
	a.Error = ""
	if err := s.store.UpdateAllocation(ctx, a); err != nil {
		return err
	}
	if err := s.queue.EnqueueReissue(ctx, a.ID, oldProjectID); err != nil {
		return err
	}
	slog.Info("reissue is enqueued", "allocationID", a.ID, "oldProjectID", oldProjectID)

	httpx.WriteJSON(w, http.StatusAccepted, a)
	return nil
}

// getRestartableAllocation は再実行・再発行の対象になる Allocation を取り出す。
// イベントの設定が今の検証を通らない場合は、直してからにしてもらうためにエラーにする。
func (s *Server) getRestartableAllocation(ctx context.Context, r *http.Request) (*model.Allocation, error) {
	e, err := s.getEvent(ctx, r.PathValue("code"))
	if err != nil {
		return nil, err
	}
	if err := e.Validate(); err != nil {
		return nil, httpx.WrapError(http.StatusBadRequest, err, "event %s has an invalid setting. fix it first: %v", e.Code, err)
	}
	if e.FolderName == "" {
		return nil, httpx.Errorf(http.StatusConflict, "event %s does not have a folder yet", e.Code)
	}

	a, err := s.store.GetAllocation(ctx, r.PathValue("id"))
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, httpx.Errorf(http.StatusNotFound, "allocation is not found")
		}
		return nil, err
	}
	if a.EventCode != e.Code {
		return nil, httpx.Errorf(http.StatusNotFound, "allocation is not found")
	}
	return a, nil
}

// ensureEventFolder はイベント用のフォルダを用意する。
// FOLDER_PARENT の直下に champions のルートフォルダを作り、その中にイベントコードと同じ名前のフォルダを作る。
// どちらも同名のフォルダがあれば再利用するため、何度呼んでも増えない。
func (s *Server) ensureEventFolder(ctx context.Context, code string) (string, error) {
	root, err := s.gcp.EnsureFolder(ctx, s.cfg.FolderParent, s.cfg.RootFolderName)
	if err != nil {
		return "", err
	}
	folder, err := s.gcp.EnsureFolder(ctx, root.GetName(), code)
	if err != nil {
		return "", err
	}
	return folder.GetName(), nil
}

func (s *Server) getEvent(ctx context.Context, code string) (*model.Event, error) {
	code = strings.ToLower(strings.TrimSpace(code))
	if err := model.ValidateEventCode(code); err != nil {
		return nil, httpx.WrapError(http.StatusBadRequest, err, "%v", err)
	}
	e, err := s.store.GetEvent(ctx, code)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, httpx.Errorf(http.StatusNotFound, "event %s is not found", code)
		}
		return nil, err
	}
	return e, nil
}

func normalizeList(list []string) []string {
	result := make([]string, 0, len(list))
	seen := make(map[string]bool, len(list))
	for _, v := range list {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			continue
		}
		seen[v] = true
		result = append(result, v)
	}
	return result
}

func hasSettingsChanged(r1, a1 []string, q1 []model.Quota, r2, a2 []string, q2 []model.Quota) bool {
	return !slicesEqual(r1, r2) || !slicesEqual(a1, a2) || !quotasEqual(q1, q2)
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func quotasEqual(a, b []model.Quota) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Service != b[i].Service ||
			a[i].QuotaID != b[i].QuotaID ||
			a[i].PreferredValue != b[i].PreferredValue ||
			a[i].ContactEmail != b[i].ContactEmail ||
			!dimensionsEqual(a[i].Dimensions, b[i].Dimensions) {
			return false
		}
	}
	return true
}

func dimensionsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

