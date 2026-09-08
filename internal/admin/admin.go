// Package admin は Admin アプリケーションの HTTP ハンドラを提供する。
package admin

import (
	"context"
	"embed"
	"errors"
	"io/fs"
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

// Server は Admin アプリケーション。
type Server struct {
	cfg   *config.Config
	store *store.Store
	gcp   *gcp.Client
}

// New は Admin の Server を作る。
func New(cfg *config.Config, s *store.Store, g *gcp.Client) *Server {
	return &Server{cfg: cfg, store: s, gcp: g}
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
