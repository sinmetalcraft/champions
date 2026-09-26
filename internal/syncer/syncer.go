// Package syncer はイベント設定 (IAM Role, API, Quota) が変更された際に、
// すでに払い出し済みの Project に対し最新設定を同期・適用する。
package syncer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/model"
	"github.com/sinmetalcraft/champions/internal/store"
)

// Syncer はイベントの設定を既存の Project に適用する。
type Syncer struct {
	store          *store.Store
	gcp            *gcp.Client
	billingAccount string
}

// New は Syncer を作る。
// billingAccount が空の場合は請求先アカウントの紐付けを行わない。
func New(s *store.Store, g *gcp.Client, billingAccount string) *Syncer {
	return &Syncer{store: s, gcp: g, billingAccount: billingAccount}
}

// Run はイベントで払い出された Project に最新の IAM, API, Quota 設定を順次適用する。
// 急ぐ処理ではないので並列にはせず、API に負荷をかけない速度で順番に適用していく。
// 1 件失敗しても残りは続け、最後にまとめて error を返してリトライさせる。
func (s *Syncer) Run(ctx context.Context, eventCode string) error {
	e, err := s.store.GetEvent(ctx, eventCode)
	if err != nil {
		return err
	}

	allocations, err := s.store.ListAllocationsByEvent(ctx, eventCode)
	if err != nil {
		return err
	}

	var done, skipped int
	var failed []string
	for _, a := range allocations {
		// Project がまだ採番されていないものや、すでに削除依頼済みのものはスキップ
		if a.ProjectID == "" || a.Status == model.AllocationStatusShutdown {
			skipped++
			continue
		}

		if err := s.applyToAllocation(ctx, a, e); err != nil {
			slog.Error("failed to sync settings to project",
				"eventCode", eventCode,
				"projectID", a.ProjectID,
				"userEmail", a.UserEmail,
				"error", err.Error(),
			)
			failed = append(failed, a.ProjectID)
			a.Error = err.Error()
			if uerr := s.store.UpdateAllocation(ctx, a); uerr != nil {
				slog.Error("failed to record sync error", "allocationID", a.ID, "error", uerr.Error())
			}
			continue
		}

		if a.Error != "" {
			a.Error = ""
			if uerr := s.store.UpdateAllocation(ctx, a); uerr != nil {
				slog.Error("failed to clear allocation error", "allocationID", a.ID, "error", uerr.Error())
			}
		}
		done++
		slog.Info("project settings synced", "eventCode", eventCode, "projectID", a.ProjectID, "userEmail", a.UserEmail)
	}

	slog.Info("sync settings is finished",
		"eventCode", eventCode,
		"total", len(allocations),
		"synced", done,
		"skipped", skipped,
		"failed", len(failed),
	)
	if len(failed) > 0 {
		return fmt.Errorf("syncer: failed to sync %d projects of %s: %v", len(failed), eventCode, failed)
	}
	return nil
}

func (s *Syncer) applyToAllocation(ctx context.Context, a *model.Allocation, e *model.Event) error {
	projectName := a.ProjectName
	if projectName == "" {
		p, err := s.gcp.GetProject(ctx, a.ProjectID)
		if err != nil {
			return fmt.Errorf("get project: %w", err)
		}
		projectName = p.GetName()
		a.ProjectName = projectName
		if err := s.store.UpdateAllocation(ctx, a); err != nil {
			return fmt.Errorf("update allocation project name: %w", err)
		}
	}

	// 1. 請求先アカウントの紐付け (設定されている場合)
	if s.billingAccount != "" {
		if err := s.gcp.LinkBillingAccount(ctx, a.ProjectID, s.billingAccount); err != nil {
			return fmt.Errorf("link billing: %w", err)
		}
	}

	// 2. IAM Role の同期
	if err := s.gcp.SyncProjectRoles(ctx, a.ProjectID, "user:"+a.UserEmail, e.Roles); err != nil {
		return fmt.Errorf("sync iam: %w", err)
	}

	// 3. API の有効化
	if len(e.APIs) > 0 {
		if err := s.gcp.EnableServices(ctx, projectName, e.APIs); err != nil {
			return fmt.Errorf("sync services: %w", err)
		}
	}

	// 4. Quota の適用
	for _, q := range e.Quotas {
		if err := s.gcp.ApplyQuota(ctx, a.ProjectID, q); err != nil {
			return fmt.Errorf("sync quota: %w", err)
		}
	}

	return nil
}
