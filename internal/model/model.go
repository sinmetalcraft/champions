// Package model は Firestore に保存するエンティティを定義する。
package model

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// KindEvent はイベントを保存する Firestore のコレクション名。
	KindEvent = "Events"
	// KindAllocation は払い出し結果を保存する Firestore のコレクション名。
	KindAllocation = "Allocations"
)

// eventCodeRe はイベントコードとして許可する文字列。
// ProjectID は "{EventCode}-{4文字}" になるため、ProjectID の制約 (6-30文字, 先頭は英字, 末尾はハイフン不可) から
// EventCode は 1-25 文字に制限する。
var eventCodeRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}[a-z0-9]$`)

// ValidateEventCode はイベントコードとして利用できる文字列かを検証する。
func ValidateEventCode(code string) error {
	if code == "" {
		return fmt.Errorf("event code is required")
	}
	if !eventCodeRe.MatchString(code) {
		return fmt.Errorf("event code %q is invalid: 2-25 chars, lowercase alphanumeric and hyphen, must start with a letter and must not end with a hyphen", code)
	}
	if strings.Contains(code, "--") {
		return fmt.Errorf("event code %q is invalid: must not contain consecutive hyphens", code)
	}
	return nil
}

// Quota は 1 つの Quota に対する希望値。Cloud Quotas API の QuotaPreference に対応する。
type Quota struct {
	// Service は Quota を持つサービス。例: "compute.googleapis.com"
	Service string `firestore:"Service" json:"service"`
	// QuotaID は Quota の ID。例: "CPUS-per-project-region"
	QuotaID string `firestore:"QuotaID" json:"quotaID"`
	// Dimensions は Quota の適用範囲。例: {"region": "asia-northeast1"}
	Dimensions map[string]string `firestore:"Dimensions" json:"dimensions"`
	// PreferredValue は希望する上限値。-1 は無制限。
	PreferredValue int64 `firestore:"PreferredValue" json:"preferredValue"`
	// ContactEmail は Quota 引き上げ申請時に Google Cloud から連絡が届く email。引き上げ時は必須。
	ContactEmail string `firestore:"ContactEmail" json:"contactEmail"`
}

// Validate は Quota の設定内容を検証する。
func (q *Quota) Validate() error {
	if q.Service == "" {
		return fmt.Errorf("quota service is required")
	}
	if q.QuotaID == "" {
		return fmt.Errorf("quota quotaID is required")
	}
	if q.PreferredValue < -1 {
		return fmt.Errorf("quota preferredValue must be greater than or equal to -1")
	}
	return nil
}

// PreferenceID は QuotaPreference の ID を組み立てる。
// Project 内で一意であればよく、再実行時に同じ ID になるよう Service と QuotaID と Dimensions から決定的に作る。
func (q *Quota) PreferenceID() string {
	var b strings.Builder
	b.WriteString("champions-")
	b.WriteString(sanitizeID(strings.TrimSuffix(q.Service, ".googleapis.com")))
	b.WriteString("-")
	b.WriteString(sanitizeID(q.QuotaID))
	for _, k := range sortedKeys(q.Dimensions) {
		b.WriteString("-")
		b.WriteString(sanitizeID(k))
		b.WriteString("-")
		b.WriteString(sanitizeID(q.Dimensions[k]))
	}
	id := strings.ToLower(b.String())
	if len(id) > 63 {
		id = id[:63]
	}
	return strings.Trim(id, "-")
}

// Event はハンズオンのイベント。Firestore の Document ID は Code。
type Event struct {
	// Code はイベントコード。Document ID と同じ値。
	Code string `firestore:"Code" json:"code"`
	// DisplayName は画面表示用のイベント名。
	DisplayName string `firestore:"DisplayName" json:"displayName"`
	// FolderName は払い出した Project を格納するフォルダのリソース名。例: "folders/1234567890"
	FolderName string `firestore:"FolderName" json:"folderName"`
	// Enabled が false のイベントは払い出しを受け付けない。
	Enabled bool `firestore:"Enabled" json:"enabled"`
	// MaxAllocations は払い出せる Project 数の上限。0 は無制限。
	MaxAllocations int `firestore:"MaxAllocations" json:"maxAllocations"`
	// Roles は払い出した Project に対して参加者へ付与する IAM Role。例: "roles/owner"
	Roles []string `firestore:"Roles" json:"roles"`
	// APIs は払い出した Project で有効化するサービス。例: "compute.googleapis.com"
	APIs []string `firestore:"APIs" json:"apis"`
	// Quotas は払い出した Project に適用する Quota。
	Quotas []Quota `firestore:"Quotas" json:"quotas"`
	// CreatedAt はイベントを登録した時刻。
	CreatedAt time.Time `firestore:"CreatedAt" json:"createdAt"`
	// UpdatedAt はイベントを最後に更新した時刻。
	UpdatedAt time.Time `firestore:"UpdatedAt" json:"updatedAt"`
}

// Validate はイベントの設定内容を検証する。
func (e *Event) Validate() error {
	if err := ValidateEventCode(e.Code); err != nil {
		return err
	}
	for _, r := range e.Roles {
		if !strings.HasPrefix(r, "roles/") {
			return fmt.Errorf("role %q is invalid: must start with roles/", r)
		}
	}
	for _, api := range e.APIs {
		if !strings.Contains(api, ".") {
			return fmt.Errorf("api %q is invalid: must be a service name like compute.googleapis.com", api)
		}
	}
	for i := range e.Quotas {
		if err := e.Quotas[i].Validate(); err != nil {
			return fmt.Errorf("quotas[%d]: %w", i, err)
		}
	}
	if e.MaxAllocations < 0 {
		return fmt.Errorf("maxAllocations must be greater than or equal to 0")
	}
	return nil
}

// AllocationStatus は払い出しの状態。
type AllocationStatus string

const (
	// AllocationStatusPending は Cloud Tasks への投入が終わり、処理開始を待っている状態。
	AllocationStatusPending AllocationStatus = "PENDING"
	// AllocationStatusProvisioning は worker が払い出し処理を実行中の状態。
	AllocationStatusProvisioning AllocationStatus = "PROVISIONING"
	// AllocationStatusReady は全ての処理が完了し、参加者が Project を使える状態。
	AllocationStatusReady AllocationStatus = "READY"
	// AllocationStatusFailed はリトライ上限に達し、払い出しを打ち切った状態。
	AllocationStatusFailed AllocationStatus = "FAILED"
	// AllocationStatusShutdown はハンズオン終了後に Project を削除した状態。
	AllocationStatusShutdown AllocationStatus = "SHUTDOWN"
)

// 払い出し処理の各ステップ。Allocation.Step に入り、進捗表示に使う。
const (
	StepQueued         = "QUEUED"
	StepCreateProject  = "CREATE_PROJECT"
	StepLinkBilling    = "LINK_BILLING"
	StepGrantIAM       = "GRANT_IAM"
	StepEnableServices = "ENABLE_SERVICES"
	StepApplyQuotas    = "APPLY_QUOTAS"
	StepDone           = "DONE"
	StepShutdown       = "SHUTDOWN"
)

// Allocation はあるユーザにあるイベントの Project を払い出した記録。
// Document ID は AllocationID() で作られ、1 ユーザ 1 イベントにつき 1 つしか作れない。
type Allocation struct {
	// ID は Firestore の Document ID。
	ID string `firestore:"-" json:"id"`
	// EventCode は払い出し元のイベントコード。
	EventCode string `firestore:"EventCode" json:"eventCode"`
	// UserEmail は IAP で認証されたユーザの email。
	UserEmail string `firestore:"UserEmail" json:"userEmail"`
	// UserID は IAP の JWT の sub。email が変わっても追跡できるように保存する。
	UserID string `firestore:"UserID" json:"userID"`
	// ProjectID は払い出した Google Cloud Project の ID。
	ProjectID string `firestore:"ProjectID" json:"projectID"`
	// ProjectName は払い出した Project のリソース名。例: "projects/415104041262"
	ProjectName string `firestore:"ProjectName" json:"projectName"`
	// FolderName は Project を格納したフォルダのリソース名。
	FolderName string `firestore:"FolderName" json:"folderName"`
	// Status は払い出しの状態。
	Status AllocationStatus `firestore:"Status" json:"status"`
	// Step は現在処理中のステップ。
	Step string `firestore:"Step" json:"step"`
	// Attempts は worker が処理を試みた回数。
	Attempts int `firestore:"Attempts" json:"attempts"`
	// Error は直近のエラーメッセージ。
	Error string `firestore:"Error" json:"error"`
	// CreatedAt は払い出しを受け付けた時刻。
	CreatedAt time.Time `firestore:"CreatedAt" json:"createdAt"`
	// UpdatedAt は最後に状態が変わった時刻。
	UpdatedAt time.Time `firestore:"UpdatedAt" json:"updatedAt"`
}

// AllocationID は EventCode と email から Firestore の Document ID を組み立てる。
// 同じユーザが同じイベントで 2 つ Project を受け取らないよう、決定的な ID にしている。
func AllocationID(eventCode, email string) string {
	return eventCode + ":" + strings.ToLower(email)
}

func sanitizeID(v string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(v) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// キー数は多くても数個なので単純な挿入ソートで十分。
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j] < keys[j-1]; j-- {
			keys[j], keys[j-1] = keys[j-1], keys[j]
		}
	}
	return keys
}
