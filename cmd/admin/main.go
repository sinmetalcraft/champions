// champions の Admin アプリケーション。
// IAP で認証した運営メンバーがハンズオンのイベントコードと払い出し設定を管理する。
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/sinmetalcraft/champions/internal/admin"
	"github.com/sinmetalcraft/champions/internal/config"
	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/iap"
	"github.com/sinmetalcraft/champions/internal/provision"
	"github.com/sinmetalcraft/champions/internal/shutdown"
	"github.com/sinmetalcraft/champions/internal/store"
	"github.com/sinmetalcraft/champions/internal/tasks"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("admin exited with error", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateAdmin(); err != nil {
		return err
	}

	st, err := store.New(ctx, cfg.ProjectID, cfg.FirestoreDatabaseID)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	gc, err := gcp.NewClient(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = gc.Close() }()

	auth, err := iap.NewAuthenticator(ctx, cfg.IAPAudience, cfg.DevUserEmail)
	if err != nil {
		return err
	}

	provisioner := provision.New(st, gc, cfg.BillingAccount, cfg.MaxProvisionAttempts)
	shutdowner := shutdown.New(st, gc)
	queue, err := newEnqueuer(ctx, cfg, provisioner, shutdowner)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()

	s := admin.New(cfg, st, gc, queue)
	return listenAndServe(ctx, ":"+cfg.Port, s.Handler(auth))
}

// enqueuer は時間のかかる処理を非同期実行に回す。
type enqueuer interface {
	admin.Enqueuer
	Close() error
}

// newEnqueuer は設定に応じて Cloud Tasks かアプリケーション内実行かを選ぶ。
func newEnqueuer(ctx context.Context, cfg *config.Config, p *provision.Provisioner, sd *shutdown.Shutdowner) (enqueuer, error) {
	if cfg.LocalTasks {
		slog.Warn("LOCAL_TASKS is enabled. tasks run in this process instead of Cloud Tasks")
		return &tasks.LocalDispatcher{Provision: p.Run, Reissue: p.Reissue, Shutdown: sd.Run}, nil
	}
	return tasks.NewQueue(ctx, tasks.Config{
		ProjectID:             cfg.ProjectID,
		Location:              cfg.TasksLocation,
		ProvisionQueue:        cfg.ProvisionQueue,
		ShutdownQueue:         cfg.ShutdownQueue,
		WorkerBaseURL:         cfg.WorkerBaseURL,
		InvokerServiceAccount: cfg.WorkerInvokerServiceAccount,
	})
}

// listenAndServe は SIGTERM を受け取るまで HTTP サーバを動かす。
func listenAndServe(ctx context.Context, addr string, h http.Handler) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	hs := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 20 * time.Second,
		// フォルダ作成の完了を待つことがあるので少し長めにしている。
		WriteTimeout: 5 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		slog.Info("start listening", "addr", addr)
		if err := hs.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		slog.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return hs.Shutdown(shutdownCtx)
	}
}
