// Package gcp は Google Cloud のリソース操作をまとめたクライアント。
package gcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	billing "cloud.google.com/go/billing/apiv1"
	"cloud.google.com/go/billing/apiv1/billingpb"
	cloudquotas "cloud.google.com/go/cloudquotas/apiv1"
	"cloud.google.com/go/cloudquotas/apiv1/cloudquotaspb"
	"cloud.google.com/go/iam/apiv1/iampb"
	resourcemanager "cloud.google.com/go/resourcemanager/apiv3"
	"cloud.google.com/go/resourcemanager/apiv3/resourcemanagerpb"
	serviceusage "cloud.google.com/go/serviceusage/apiv1"
	"cloud.google.com/go/serviceusage/apiv1/serviceusagepb"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/sinmetalcraft/champions/internal/model"
)

// ErrNotFound は対象のリソースが存在しないときに返る。
var ErrNotFound = errors.New("gcp: not found")

// batchEnableServicesLimit は BatchEnableServices が 1 回で扱えるサービス数の上限。
const batchEnableServicesLimit = 20

// Client は Project / Folder / Service Usage / Cloud Quotas / Billing を操作する。
type Client struct {
	projects     *resourcemanager.ProjectsClient
	folders      *resourcemanager.FoldersClient
	serviceUsage *serviceusage.Client
	quotas       *cloudquotas.Client
	billing      *billing.CloudBillingClient
}

// NewClient は必要な API クライアントをまとめて作る。
func NewClient(ctx context.Context) (*Client, error) {
	projects, err := resourcemanager.NewProjectsClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create projects client: %w", err)
	}
	folders, err := resourcemanager.NewFoldersClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create folders client: %w", err)
	}
	su, err := serviceusage.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create serviceusage client: %w", err)
	}
	quotas, err := cloudquotas.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create cloudquotas client: %w", err)
	}
	bill, err := billing.NewCloudBillingClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create billing client: %w", err)
	}
	return &Client{projects: projects, folders: folders, serviceUsage: su, quotas: quotas, billing: bill}, nil
}

// Close は全てのクライアントを閉じる。
func (c *Client) Close() error {
	return errors.Join(
		c.projects.Close(),
		c.folders.Close(),
		c.serviceUsage.Close(),
		c.quotas.Close(),
		c.billing.Close(),
	)
}

// EnsureFolder は parent の下に displayName のフォルダを作る。
// 同名のフォルダが既にある場合はそれを返すため、何度呼んでも安全。
func (c *Client) EnsureFolder(ctx context.Context, parent, displayName string) (*resourcemanagerpb.Folder, error) {
	if folder, err := c.findFolder(ctx, parent, displayName); err != nil {
		return nil, err
	} else if folder != nil {
		return folder, nil
	}

	op, err := c.folders.CreateFolder(ctx, &resourcemanagerpb.CreateFolderRequest{
		Folder: &resourcemanagerpb.Folder{
			Parent:      parent,
			DisplayName: displayName,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to create folder %s under %s: %w", displayName, parent, err)
	}
	folder, err := op.Wait(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcp: failed to wait create folder %s under %s: %w", displayName, parent, err)
	}
	return folder, nil
}

func (c *Client) findFolder(ctx context.Context, parent, displayName string) (*resourcemanagerpb.Folder, error) {
	iter := c.folders.ListFolders(ctx, &resourcemanagerpb.ListFoldersRequest{Parent: parent})
	for {
		folder, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("gcp: failed to list folders under %s: %w", parent, err)
		}
		if folder.GetDisplayName() == displayName && folder.GetState() == resourcemanagerpb.Folder_ACTIVE {
			return folder, nil
		}
	}
}

// GetProject は ProjectID から Project を取得する。存在しない場合は ErrNotFound を返す。
func (c *Client) GetProject(ctx context.Context, projectID string) (*resourcemanagerpb.Project, error) {
	p, err := c.projects.GetProject(ctx, &resourcemanagerpb.GetProjectRequest{Name: "projects/" + projectID})
	if err != nil {
		if code := status.Code(err); code == codes.NotFound || code == codes.PermissionDenied {
			// 他の組織が使っている ProjectID でも PermissionDenied になるため、存在しない扱いにはできない。
			// 呼び出し側で作成を試みて ALREADY_EXISTS を見るのが確実なので、ここでは NotFound として返す。
			return nil, fmt.Errorf("project %s: %w", projectID, ErrNotFound)
		}
		return nil, fmt.Errorf("gcp: failed to get project %s: %w", projectID, err)
	}
	return p, nil
}

// CreateProject は parent の下に Project を作る。
// ProjectID が既に使われている場合は ErrProjectIDTaken を返す。
func (c *Client) CreateProject(ctx context.Context, projectID, displayName, parent string, labels map[string]string) (*resourcemanagerpb.Project, error) {
	op, err := c.projects.CreateProject(ctx, &resourcemanagerpb.CreateProjectRequest{
		Project: &resourcemanagerpb.Project{
			ProjectId:   projectID,
			DisplayName: displayName,
			Parent:      parent,
			Labels:      labels,
		},
	})
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, fmt.Errorf("project %s: %w", projectID, ErrProjectIDTaken)
		}
		return nil, fmt.Errorf("gcp: failed to create project %s: %w", projectID, err)
	}
	p, err := op.Wait(ctx)
	if err != nil {
		if status.Code(err) == codes.AlreadyExists {
			return nil, fmt.Errorf("project %s: %w", projectID, ErrProjectIDTaken)
		}
		return nil, fmt.Errorf("gcp: failed to wait create project %s: %w", projectID, err)
	}
	return p, nil
}

// ErrProjectIDTaken は ProjectID が既に他の Project に使われているときに返る。
var ErrProjectIDTaken = errors.New("gcp: project id is already taken")

// LinkBillingAccount は Project に請求先アカウントを紐付ける。既に同じアカウントが紐付いている場合は何もしない。
func (c *Client) LinkBillingAccount(ctx context.Context, projectID, billingAccount string) error {
	name := "projects/" + projectID
	info, err := c.billing.GetProjectBillingInfo(ctx, &billingpb.GetProjectBillingInfoRequest{Name: name})
	if err == nil && info.GetBillingAccountName() == billingAccount {
		return nil
	}
	if _, err := c.billing.UpdateProjectBillingInfo(ctx, &billingpb.UpdateProjectBillingInfoRequest{
		Name:               name,
		ProjectBillingInfo: &billingpb.ProjectBillingInfo{BillingAccountName: billingAccount},
	}); err != nil {
		return fmt.Errorf("gcp: failed to link billing account %s to %s: %w", billingAccount, projectID, err)
	}
	return nil
}

// GrantProjectRoles は Project の IAM Policy に member と roles の binding を追加する。
// 既に付与済みの Role は追加しないため、何度呼んでも安全。
func (c *Client) GrantProjectRoles(ctx context.Context, projectID, member string, roles []string) error {
	if len(roles) == 0 {
		return nil
	}
	resource := "projects/" + projectID

	// SetIamPolicy は etag が合わないと ABORTED になるため、read-modify-write をリトライする。
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		policy, err := c.projects.GetIamPolicy(ctx, &iampb.GetIamPolicyRequest{Resource: resource})
		if err != nil {
			return fmt.Errorf("gcp: failed to get iam policy of %s: %w", projectID, err)
		}
		if !addBindings(policy, member, roles) {
			return nil
		}
		if _, err := c.projects.SetIamPolicy(ctx, &iampb.SetIamPolicyRequest{Resource: resource, Policy: policy}); err != nil {
			if status.Code(err) == codes.Aborted {
				lastErr = err
				continue
			}
			return fmt.Errorf("gcp: failed to set iam policy of %s: %w", projectID, err)
		}
		return nil
	}
	return fmt.Errorf("gcp: failed to set iam policy of %s after retries: %w", projectID, lastErr)
}

// EnableServices は Project でサービスを有効化する。
// projectName は "projects/{PROJECT_NUMBER}" 形式のリソース名。
// BatchEnableServices は 1 回に 20 件までのため分割して呼ぶ。既に有効なサービスを指定しても成功する。
func (c *Client) EnableServices(ctx context.Context, projectName string, apis []string) error {
	if len(apis) == 0 {
		return nil
	}
	for i := 0; i < len(apis); i += batchEnableServicesLimit {
		chunk := apis[i:min(i+batchEnableServicesLimit, len(apis))]
		op, err := c.serviceUsage.BatchEnableServices(ctx, &serviceusagepb.BatchEnableServicesRequest{
			Parent:     projectName,
			ServiceIds: chunk,
		})
		if err != nil {
			return fmt.Errorf("gcp: failed to enable services %v on %s: %w", chunk, projectName, err)
		}
		if _, err := op.Wait(ctx); err != nil {
			return fmt.Errorf("gcp: failed to wait enable services %v on %s: %w", chunk, projectName, err)
		}
	}
	return nil
}

// ApplyQuota は Project に QuotaPreference を作成もしくは更新する。
// QuotaPreference の ID は設定内容から決定的に作るため、同じ設定で何度呼んでも増えない。
func (c *Client) ApplyQuota(ctx context.Context, projectID string, q model.Quota) error {
	name := fmt.Sprintf("projects/%s/locations/global/quotaPreferences/%s", projectID, q.PreferenceID())
	_, err := c.quotas.UpdateQuotaPreference(ctx, &cloudquotaspb.UpdateQuotaPreferenceRequest{
		// AllowMissing を立てると存在しない場合に作成される。リトライ時も同じ呼び出しで済む。
		AllowMissing: true,
		QuotaPreference: &cloudquotaspb.QuotaPreference{
			Name:          name,
			Service:       q.Service,
			QuotaId:       q.QuotaID,
			Dimensions:    q.Dimensions,
			ContactEmail:  q.ContactEmail,
			Justification: "Provisioned by champions for a Google Cloud hands-on",
			QuotaConfig: &cloudquotaspb.QuotaConfig{
				PreferredValue: q.PreferredValue,
			},
		},
	})
	if err != nil {
		return fmt.Errorf("gcp: failed to apply quota %s/%s on %s: %w", q.Service, q.QuotaID, projectID, err)
	}
	return nil
}

func addBindings(policy *iampb.Policy, member string, roles []string) bool {
	changed := false
	for _, role := range roles {
		var binding *iampb.Binding
		for _, b := range policy.Bindings {
			if b.Role == role && b.Condition == nil {
				binding = b
				break
			}
		}
		if binding == nil {
			policy.Bindings = append(policy.Bindings, &iampb.Binding{Role: role, Members: []string{member}})
			changed = true
			continue
		}
		if !slicesContains(binding.Members, member) {
			binding.Members = append(binding.Members, member)
			changed = true
		}
	}
	return changed
}

func slicesContains(list []string, v string) bool {
	for _, s := range list {
		if strings.EqualFold(s, v) {
			return true
		}
	}
	return false
}
