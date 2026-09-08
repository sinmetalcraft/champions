// Package config はアプリケーションの起動設定を環境変数から読み込む。
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// Config は server / admin 双方で利用する設定。
// 必須項目はアプリケーションごとに異なるため、Validate 系メソッドを用途別に用意している。
type Config struct {
	// Port は Cloud Run が指定する待ち受けポート。
	Port string

	// ProjectID は champions 自身が動いている Google Cloud Project。Firestore と Cloud Tasks の所属先。
	ProjectID string

	// FirestoreDatabaseID は利用する Firestore のデータベース ID。
	// 既定のデータベースは "(default)" で、名前付きのデータベースを使う場合はその名前を指定する。
	FirestoreDatabaseID string

	// IAPAudience は IAP が発行する JWT の aud。
	// Cloud Run に直接 IAP を有効にした場合は "/projects/{PROJECT_NUMBER}/apps/{PROJECT_ID}"、
	// 外部 LB 経由の場合は "/projects/{PROJECT_NUMBER}/global/backendServices/{BACKEND_SERVICE_ID}"。
	// 空の場合 IAP の検証を行わず DevUserEmail のユーザとして扱う (ローカル開発用)。
	IAPAudience string

	// DevUserEmail は IAPAudience が空のときに使うユーザの email。
	DevUserEmail string

	// AdminEmails は admin アプリを利用できる email の許可リスト。空の場合は IAP の許可に委ねる。
	AdminEmails []string

	// FolderParent は champions のルートフォルダを作る親リソース。"organizations/123" もしくは "folders/456"。
	FolderParent string

	// RootFolderName は FolderParent の下に作る champions のルートフォルダ名。
	// イベント用のフォルダはこのフォルダの中に作る。
	RootFolderName string

	// BillingAccount は払い出した Project に紐付ける請求先アカウント。"billingAccounts/XXXXXX-XXXXXX-XXXXXX"。
	// 空の場合は紐付けを行わない。
	BillingAccount string

	// TasksLocation は Cloud Tasks キューのロケーション。例: "asia-northeast1"。
	TasksLocation string

	// TasksQueue は払い出し処理を投入する Cloud Tasks キュー名。
	TasksQueue string

	// WorkerBaseURL は Cloud Tasks が叩く worker のベース URL。例: "https://champions-worker-xxxx.a.run.app"。
	WorkerBaseURL string

	// WorkerInvokerServiceAccount は Cloud Tasks が OIDC トークンを発行するときに使う service account。
	WorkerInvokerServiceAccount string

	// TaskInvokerEmails は /tasks/ 配下を呼び出せる service account の email。
	// 空の場合は WorkerInvokerServiceAccount のみを許可する。
	TaskInvokerEmails []string

	// MaxProvisionAttempts はこの回数を超えて失敗した払い出しを FAILED として打ち切る。
	MaxProvisionAttempts int

	// LocalTasks が true の場合、Cloud Tasks を使わずにアプリケーション内で払い出し処理を実行する。
	// Cloud Tasks は localhost に届かないため、ローカル開発でだけ使う。
	LocalTasks bool
}

// Load は環境変数から Config を組み立てる。
func Load() (*Config, error) {
	c := &Config{
		Port:                        env("PORT", "8080"),
		ProjectID:                   env("GOOGLE_CLOUD_PROJECT", ""),
		FirestoreDatabaseID:         env("FIRESTORE_DATABASE_ID", "(default)"),
		IAPAudience:                 env("IAP_AUDIENCE", ""),
		DevUserEmail:                env("DEV_USER_EMAIL", ""),
		AdminEmails:                 splitList(env("ADMIN_EMAILS", "")),
		FolderParent:                env("FOLDER_PARENT", ""),
		RootFolderName:              env("ROOT_FOLDER_NAME", "champions"),
		BillingAccount:              env("BILLING_ACCOUNT", ""),
		TasksLocation:               env("TASKS_LOCATION", ""),
		TasksQueue:                  env("TASKS_QUEUE", "champions-provision"),
		WorkerBaseURL:               strings.TrimSuffix(env("WORKER_BASE_URL", ""), "/"),
		WorkerInvokerServiceAccount: env("WORKER_INVOKER_SERVICE_ACCOUNT", ""),
		TaskInvokerEmails:           splitList(env("TASK_INVOKER_EMAILS", "")),
		MaxProvisionAttempts:        envInt("MAX_PROVISION_ATTEMPTS", 5),
		LocalTasks:                  env("LOCAL_TASKS", "") == "true",
	}
	if c.ProjectID == "" {
		return nil, fmt.Errorf("config: GOOGLE_CLOUD_PROJECT is required")
	}
	if c.IAPAudience == "" && c.DevUserEmail == "" {
		return nil, fmt.Errorf("config: IAP_AUDIENCE is required (set DEV_USER_EMAIL to run without IAP)")
	}
	if len(c.TaskInvokerEmails) == 0 && c.WorkerInvokerServiceAccount != "" {
		c.TaskInvokerEmails = []string{c.WorkerInvokerServiceAccount}
	}
	return c, nil
}

// ValidateServer は一般公開アプリケーションに必要な設定が揃っているかを確認する。
func (c *Config) ValidateServer() error {
	if c.LocalTasks {
		// Cloud Tasks を使わないので、キューと worker の設定は要らない。
		return nil
	}
	if c.TasksLocation == "" {
		return fmt.Errorf("config: TASKS_LOCATION is required")
	}
	if c.WorkerBaseURL == "" {
		return fmt.Errorf("config: WORKER_BASE_URL is required")
	}
	if c.WorkerInvokerServiceAccount == "" {
		return fmt.Errorf("config: WORKER_INVOKER_SERVICE_ACCOUNT is required")
	}
	return nil
}

// ValidateAdmin は Admin アプリケーションに必要な設定が揃っているかを確認する。
func (c *Config) ValidateAdmin() error {
	if c.FolderParent == "" {
		return fmt.Errorf("config: FOLDER_PARENT is required")
	}
	if !strings.HasPrefix(c.FolderParent, "organizations/") && !strings.HasPrefix(c.FolderParent, "folders/") {
		return fmt.Errorf("config: FOLDER_PARENT must start with organizations/ or folders/, got %q", c.FolderParent)
	}
	return nil
}

// IsAdmin は email が admin アプリを利用できるかを返す。
func (c *Config) IsAdmin(email string) bool {
	if len(c.AdminEmails) == 0 {
		return true
	}
	for _, v := range c.AdminEmails {
		if strings.EqualFold(v, email) {
			return true
		}
	}
	return false
}

func env(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	v := env(key, "")
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return def
	}
	return n
}

func splitList(v string) []string {
	if v == "" {
		return nil
	}
	var list []string
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" {
			list = append(list, s)
		}
	}
	return list
}
