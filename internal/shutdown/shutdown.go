// Package shutdown はハンズオン終了後に、イベントで払い出した Project を片付ける。
package shutdown

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/model"
	"github.com/sinmetalcraft/champions/internal/store"
)

// Shutdowner はイベント 1 つ分の Project を片付ける。
type Shutdowner struct {
	store *store.Store
	gcp   *gcp.Client
}

// New は Shutdowner を作る。
func New(s *store.Store, g *gcp.Client) *Shutdowner {
	return &Shutdowner{store: s, gcp: g}
}

// Run はイベントで払い出した Project を 1 件ずつ削除依頼状態にする。
// 急ぐ処理ではないので並列にはせず、Resource Manager に負荷をかけない速度で順番に消していく。
//
// 成功した Allocation はその都度 SHUTDOWN に更新するため、
// 途中で失敗して Cloud Tasks にリトライされても、済んでいる分は飛ばして続きから再開する。
// 1 件失敗しても残りは続け、最後にまとめて error を返してリトライさせる。
func (s *Shutdowner) Run(ctx context.Context, eventCode string) error {
	allocations, err := s.store.ListAllocationsByEvent(ctx, eventCode)
	if err != nil {
		return err
	}

	var done, skipped int
	var failed []string
	for _, a := range allocations {
		if a.Status == model.AllocationStatusShutdown {
			skipped++
			continue
		}
		if a.ProjectID == "" {
			// ProjectID が採番される前に失敗した払い出し。消すものがない。
			skipped++
			continue
		}

		if err := s.gcp.ShutdownProject(ctx, a.ProjectID); err != nil {
			slog.Error("failed to shutdown project", "eventCode", eventCode, "projectID", a.ProjectID, "error", err.Error())
			failed = append(failed, a.ProjectID)
			a.Error = err.Error()
			if err := s.store.UpdateAllocation(ctx, a); err != nil {
				slog.Error("failed to record shutdown error", "allocationID", a.ID, "error", err.Error())
			}
			continue
		}

		a.Status = model.AllocationStatusShutdown
		a.Step = model.StepShutdown
		a.Error = ""
		if err := s.store.UpdateAllocation(ctx, a); err != nil {
			// Project は消えているので、リトライ時は ShutdownProject が何もせずに通り、ここだけやり直される。
			failed = append(failed, a.ProjectID)
			continue
		}
		done++
		slog.Info("project is shutdown", "eventCode", eventCode, "projectID", a.ProjectID, "userEmail", a.UserEmail)
	}

	slog.Info("shutdown is finished", "eventCode", eventCode, "total", len(allocations), "shutdown", done, "skipped", skipped, "failed", len(failed))
	if len(failed) > 0 {
		return fmt.Errorf("shutdown: failed to shutdown %d projects of %s: %v", len(failed), eventCode, failed)
	}
	return nil
}

// LocalDispatcher は Cloud Tasks を使わずに、その場で Shutdown を実行する。
// Cloud Tasks は localhost に届かないため、ローカル開発でだけ使う。
type LocalDispatcher struct {
	shutdowner *Shutdowner
}

// NewLocalDispatcher は LocalDispatcher を作る。
func NewLocalDispatcher(s *Shutdowner) *LocalDispatcher {
	return &LocalDispatcher{shutdowner: s}
}

// EnqueueShutdown は Shutdown を goroutine で実行する。
func (d *LocalDispatcher) EnqueueShutdown(ctx context.Context, eventCode string) error {
	// リクエストの context はレスポンスを返した時点で終わるため、切り離してから実行する。
	ctx = context.WithoutCancel(ctx)
	go func() {
		if err := d.shutdowner.Run(ctx, eventCode); err != nil {
			slog.Error("local shutdown failed", "eventCode", eventCode, "error", err.Error())
		}
	}()
	return nil
}
