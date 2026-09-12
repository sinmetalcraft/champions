// Package provision は Cloud Tasks から呼ばれる払い出し処理の本体。
package provision

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"

	"github.com/sinmetalcraft/champions/internal/gcp"
	"github.com/sinmetalcraft/champions/internal/model"
	"github.com/sinmetalcraft/champions/internal/store"
)

// projectIDSuffixChars は ProjectID の末尾に付けるランダム文字列に使う文字。
// ProjectID は小文字英数字とハイフンのみ許されるため、紛らわしい文字を除いた英数字を使う。
const projectIDSuffixChars = "abcdefghijkmnopqrstuvwxyz23456789"

// projectIDSuffixLen は ProjectID の末尾に付けるランダム文字列の長さ。
const projectIDSuffixLen = 4

// createProjectRetry は ProjectID の衝突時に別の ID で作り直す回数。
const createProjectRetry = 5

// NewProjectID はイベントコードから "{EventCode}-{ランダム4文字}" の ProjectID を作る。
func NewProjectID(eventCode string) (string, error) {
	buf := make([]byte, projectIDSuffixLen)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("provision: failed to read random bytes: %w", err)
	}
	suffix := make([]byte, projectIDSuffixLen)
	for i, b := range buf {
		suffix[i] = projectIDSuffixChars[int(b)%len(projectIDSuffixChars)]
	}
	return eventCode + "-" + string(suffix), nil
}

// Provisioner は Allocation を 1 件処理する。
type Provisioner struct {
	store          *store.Store
	gcp            *gcp.Client
	billingAccount string
	maxAttempts    int
}

// New は Provisioner を作る。
// billingAccount が空の場合は請求先アカウントの紐付けを行わない。
func New(s *store.Store, g *gcp.Client, billingAccount string, maxAttempts int) *Provisioner {
	if maxAttempts < 1 {
		maxAttempts = 1
	}
	return &Provisioner{store: s, gcp: g, billingAccount: billingAccount, maxAttempts: maxAttempts}
}

// Run は allocationID の払い出しを進める。
// リトライ上限に達した場合は Allocation を FAILED にして nil を返し、Cloud Tasks のリトライを止める。
// error を返した場合は Cloud Tasks にリトライさせる。
func (p *Provisioner) Run(ctx context.Context, allocationID string, retryCount int) error {
	a, err := p.store.GetAllocation(ctx, allocationID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// 手動で消された Allocation はリトライしても意味がないので成功扱いにする。
			slog.Warn("allocation is not found. skip provisioning", "allocationID", allocationID)
			return nil
		}
		return err
	}
	if a.Status == model.AllocationStatusReady || a.Status == model.AllocationStatusFailed {
		slog.Info("allocation is already finished", "allocationID", allocationID, "status", a.Status)
		return nil
	}

	e, err := p.store.GetEvent(ctx, a.EventCode)
	if err != nil {
		return p.fail(ctx, a, retryCount, err)
	}

	a.Attempts = retryCount + 1
	a.Status = model.AllocationStatusProvisioning
	if err := p.store.UpdateAllocation(ctx, a); err != nil {
		return err
	}

	if err := p.provision(ctx, a, e); err != nil {
		return p.fail(ctx, a, retryCount, err)
	}

	a.Status = model.AllocationStatusReady
	a.Step = model.StepDone
	a.Error = ""
	if err := p.store.UpdateAllocation(ctx, a); err != nil {
		return err
	}
	slog.Info("allocation is ready", "allocationID", a.ID, "projectID", a.ProjectID, "userEmail", a.UserEmail)
	return nil
}

func (p *Provisioner) provision(ctx context.Context, a *model.Allocation, e *model.Event) error {
	if e.FolderName == "" {
		return fmt.Errorf("provision: event %s does not have a folder", e.Code)
	}

	if a.ProjectName == "" {
		if err := p.setStep(ctx, a, model.StepCreateProject); err != nil {
			return err
		}
		if err := p.ensureProject(ctx, a, e); err != nil {
			return err
		}
	}

	if p.billingAccount != "" {
		if err := p.setStep(ctx, a, model.StepLinkBilling); err != nil {
			return err
		}
		if err := p.gcp.LinkBillingAccount(ctx, a.ProjectID, p.billingAccount); err != nil {
			return err
		}
	}

	if err := p.setStep(ctx, a, model.StepGrantIAM); err != nil {
		return err
	}
	if err := p.gcp.GrantProjectRoles(ctx, a.ProjectID, "user:"+a.UserEmail, e.Roles); err != nil {
		return err
	}

	if err := p.setStep(ctx, a, model.StepEnableServices); err != nil {
		return err
	}
	if err := p.gcp.EnableServices(ctx, a.ProjectName, e.APIs); err != nil {
		return err
	}

	if err := p.setStep(ctx, a, model.StepApplyQuotas); err != nil {
		return err
	}
	for _, q := range e.Quotas {
		if err := p.gcp.ApplyQuota(ctx, a.ProjectID, q); err != nil {
			return err
		}
	}
	return nil
}

// ensureProject は Allocation に対応する Project を用意する。
// 既に作成済みなら取得し、未作成なら作る。ProjectID が他所で使われていた場合は別の ID で作り直す。
func (p *Provisioner) ensureProject(ctx context.Context, a *model.Allocation, e *model.Event) error {
	labels := map[string]string{"champions-event": e.Code}

	for i := 0; i < createProjectRetry; i++ {
		if a.ProjectID == "" {
			id, err := NewProjectID(e.Code)
			if err != nil {
				return err
			}
			a.ProjectID = id
			// 作成前に ProjectID を保存しておくと、応答を取りこぼしてもリトライ時に同じ Project を拾える。
			if err := p.store.UpdateAllocation(ctx, a); err != nil {
				return err
			}
		}

		project, err := p.gcp.GetProject(ctx, a.ProjectID)
		if err == nil {
			if project.GetParent() != e.FolderName {
				// 自分たちのフォルダの外にある Project は使えないので、別の ID を引き直す。
				slog.Warn("project id is used by another folder. retry with another id", "projectID", a.ProjectID, "parent", project.GetParent())
				a.ProjectID = ""
				continue
			}
			a.ProjectName = project.GetName()
			a.FolderName = e.FolderName
			return p.store.UpdateAllocation(ctx, a)
		}
		if !errors.Is(err, gcp.ErrNotFound) {
			return err
		}

		project, err = p.gcp.CreateProject(ctx, a.ProjectID, a.ProjectID, e.FolderName, labels)
		if errors.Is(err, gcp.ErrProjectIDTaken) {
			slog.Warn("project id is already taken. retry with another id", "projectID", a.ProjectID)
			a.ProjectID = ""
			continue
		}
		if err != nil {
			return err
		}
		a.ProjectName = project.GetName()
		a.FolderName = e.FolderName
		return p.store.UpdateAllocation(ctx, a)
	}
	return fmt.Errorf("provision: failed to find an available project id for event %s", e.Code)
}

func (p *Provisioner) setStep(ctx context.Context, a *model.Allocation, step string) error {
	a.Step = step
	return p.store.UpdateAllocation(ctx, a)
}

// fail はエラーを Allocation に記録する。
// リトライ上限に達していれば FAILED にして nil を返し、それ以外は元のエラーを返してリトライさせる。
func (p *Provisioner) fail(ctx context.Context, a *model.Allocation, retryCount int, cause error) error {
	a.Attempts = retryCount + 1
	a.Error = cause.Error()
	if a.Attempts >= p.maxAttempts {
		a.Status = model.AllocationStatusFailed
		slog.Error("allocation is failed. give up retrying", "allocationID", a.ID, "attempts", a.Attempts, "error", cause.Error())
	} else {
		a.Status = model.AllocationStatusProvisioning
		slog.Warn("allocation is failed. retry later", "allocationID", a.ID, "attempts", a.Attempts, "error", cause.Error())
	}
	if err := p.store.UpdateAllocation(ctx, a); err != nil {
		return errors.Join(cause, err)
	}
	if a.Status == model.AllocationStatusFailed {
		return nil
	}
	return cause
}

// LocalDispatcher は Cloud Tasks を使わずに、その場で払い出し処理を実行する。
// Cloud Tasks は localhost に届かないため、ローカル開発でだけ使う。
type LocalDispatcher struct {
	provisioner *Provisioner
}

// NewLocalDispatcher は LocalDispatcher を作る。
func NewLocalDispatcher(p *Provisioner) *LocalDispatcher {
	return &LocalDispatcher{provisioner: p}
}

// EnqueueProvision は払い出し処理を goroutine で実行する。
// Cloud Tasks と同じくリクエストとは非同期になるので、画面のポーリングもそのまま動く。
func (d *LocalDispatcher) EnqueueProvision(ctx context.Context, allocationID string) error {
	// リクエストの context はレスポンスを返した時点で終わるため、切り離してから実行する。
	ctx = context.WithoutCancel(ctx)
	go func() {
		if err := d.provisioner.Run(ctx, allocationID, 0); err != nil {
			slog.Error("local provisioning failed", "allocationID", allocationID, "error", err.Error())
		}
	}()
	return nil
}
