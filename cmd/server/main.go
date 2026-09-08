// champions の一般公開アプリケーション。
// IAP で認証したユーザにハンズオン用の Google Cloud Project を払い出す。
// Cloud Tasks から呼ばれる払い出し worker も同じバイナリに入っている。
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

	"github.com/sinmetalcraft/champions/internal/config"
	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/iap"
	"github.com/sinmetalcraft/champions/internal/provision"
	"github.com/sinmetalcraft/champions/internal/server"
	"github.com/sinmetalcraft/champions/internal/store"
	"github.com/sinmetalcraft/champions/internal/tasks"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stdout, nil)))
	if err := run(); err != nil {
		slog.Error("server exited with error", "error", err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx := context.Background()

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := cfg.ValidateServer(); err != nil {
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

	auth, err := iap.NewAuthenticator(cfg.IAPAudience, cfg.DevUserEmail)
	if err != nil {
		return err
	}
	verifier := tasks.NewVerifier(cfg.WorkerBaseURL, cfg.TaskInvokerEmails)
	provisioner := provision.New(st, gc, cfg.BillingAccount, cfg.MaxProvisionAttempts)

	queue, err := newEnqueuer(ctx, cfg, provisioner)
	if err != nil {
		return err
	}
	defer func() { _ = queue.Close() }()

	s := server.New(cfg, st, queue, provisioner)
	return listenAndServe(ctx, ":"+cfg.Port, s.Handler(auth, verifier))
}

// enqueuer は払い出し処理を非同期実行に回す。
type enqueuer interface {
	server.Enqueuer
	Close() error
}

// localEnqueuer は Close が要らない LocalDispatcher を enqueuer に合わせる。
type localEnqueuer struct{ *provision.LocalDispatcher }

func (localEnqueuer) Close() error { return nil }

// newEnqueuer は設定に応じて Cloud Tasks かアプリケーション内実行かを選ぶ。
func newEnqueuer(ctx context.Context, cfg *config.Config, p *provision.Provisioner) (enqueuer, error) {
	if cfg.LocalTasks {
		slog.Warn("LOCAL_TASKS is enabled. provisioning runs in this process instead of Cloud Tasks")
		return localEnqueuer{provision.NewLocalDispatcher(p)}, nil
	}
	return tasks.NewQueue(ctx, cfg.ProjectID, cfg.TasksLocation, cfg.TasksQueue, cfg.WorkerBaseURL, cfg.WorkerInvokerServiceAccount)
}

// listenAndServe は SIGTERM を受け取るまで HTTP サーバを動かす。
// Cloud Run は停止時に SIGTERM を送るので、処理中のリクエストを待ってから終了する。
func listenAndServe(ctx context.Context, addr string, h http.Handler) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	hs := &http.Server{
		Addr:              addr,
		Handler:           h,
		ReadHeaderTimeout: 20 * time.Second,
		// Project 作成や API の有効化を待つため、worker のリクエストは長時間かかる。
		WriteTimeout: 30 * time.Minute,
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
