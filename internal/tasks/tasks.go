// Package tasks は払い出し処理を Cloud Tasks に投入し、worker 側で呼び出し元を検証する。
package tasks

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	cloudtasks "cloud.google.com/go/cloudtasks/apiv2"
	"cloud.google.com/go/cloudtasks/apiv2/cloudtaskspb"
	"google.golang.org/api/idtoken"

	"github.com/sinmetalcraft/champions/internal/httpx"
)

// ProvisionPath は払い出し処理を行う worker のパス。
const ProvisionPath = "/tasks/provision"

// ShutdownPath はハンズオン後の Project の片付けを行う worker のパス。
const ShutdownPath = "/tasks/shutdown"

// RetryCountHeader は Cloud Tasks がリトライ回数を入れて送るヘッダ。
const RetryCountHeader = "X-CloudTasks-TaskRetryCount"

// ProvisionRequest は worker に渡す払い出し処理の指示。
type ProvisionRequest struct {
	// AllocationID は処理対象の Allocation の Document ID。
	AllocationID string `json:"allocationID"`
}

// ShutdownRequest は worker に渡す Shutdown の指示。
type ShutdownRequest struct {
	// EventCode は片付ける対象のイベントコード。
	EventCode string `json:"eventCode"`
}

// Config は Cloud Tasks にタスクを投入するための設定。
type Config struct {
	// ProjectID はキューが属する Google Cloud Project。
	ProjectID string
	// Location はキューのロケーション。
	Location string
	// ProvisionQueue は払い出し処理を積むキュー。
	ProvisionQueue string
	// ShutdownQueue は後片付けを積むキュー。
	// 払い出しと片付けでは適した並列度もリトライ間隔も違うため、キューを分けている。
	// worker は同じ service account で動くので、IAM の設定は 2 つのキューで共通でよい。
	ShutdownQueue string
	// WorkerBaseURL はタスクの宛先になる worker のベース URL。
	WorkerBaseURL string
	// InvokerServiceAccount は Cloud Tasks が OIDC トークンを発行するときに使う service account。
	InvokerServiceAccount string
}

// Queue は Cloud Tasks にタスクを投入する。
type Queue struct {
	client                *cloudtasks.Client
	locationParent        string
	provisionQueue        string
	shutdownQueue         string
	workerBaseURL         string
	invokerServiceAccount string
}

// NewQueue は Cloud Tasks に接続した Queue を作る。
func NewQueue(ctx context.Context, cfg Config) (*Queue, error) {
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks: failed to create cloudtasks client: %w", err)
	}
	return &Queue{
		client:                client,
		locationParent:        fmt.Sprintf("projects/%s/locations/%s", cfg.ProjectID, cfg.Location),
		provisionQueue:        cfg.ProvisionQueue,
		shutdownQueue:         cfg.ShutdownQueue,
		workerBaseURL:         strings.TrimSuffix(cfg.WorkerBaseURL, "/"),
		invokerServiceAccount: cfg.InvokerServiceAccount,
	}, nil
}

// Close は Cloud Tasks クライアントを閉じる。
func (q *Queue) Close() error { return q.client.Close() }

// EnqueueProvision は払い出し処理のタスクを投入する。
func (q *Queue) EnqueueProvision(ctx context.Context, allocationID string) error {
	if err := q.enqueue(ctx, q.provisionQueue, ProvisionPath, &ProvisionRequest{AllocationID: allocationID}); err != nil {
		return fmt.Errorf("tasks: failed to create provision task for %s: %w", allocationID, err)
	}
	return nil
}

// EnqueueShutdown はイベントの Project を片付けるタスクを投入する。
func (q *Queue) EnqueueShutdown(ctx context.Context, eventCode string) error {
	if err := q.enqueue(ctx, q.shutdownQueue, ShutdownPath, &ShutdownRequest{EventCode: eventCode}); err != nil {
		return fmt.Errorf("tasks: failed to create shutdown task for %s: %w", eventCode, err)
	}
	return nil
}

// enqueue は worker の path に body を POST するタスクを queue に積む。
func (q *Queue) enqueue(ctx context.Context, queue, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("failed to marshal request: %w", err)
	}
	_, err = q.client.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: q.locationParent + "/queues/" + queue,
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{
				HttpRequest: &cloudtaskspb.HttpRequest{
					Url:        q.workerBaseURL + path,
					HttpMethod: cloudtaskspb.HttpMethod_POST,
					Headers:    map[string]string{"Content-Type": "application/json"},
					Body:       body,
					AuthorizationHeader: &cloudtaskspb.HttpRequest_OidcToken{
						OidcToken: &cloudtaskspb.OidcToken{
							ServiceAccountEmail: q.invokerServiceAccount,
							Audience:            q.workerBaseURL,
						},
					},
				},
			},
		},
	})
	return err
}

// Verifier は Cloud Tasks から送られてくる OIDC トークンを検証する。
type Verifier struct {
	audience      string
	allowedEmails []string
}

// NewVerifier は audience と呼び出しを許可する service account を指定して Verifier を作る。
// audience が空の場合は検証を行わない (ローカル開発用)。
func NewVerifier(audience string, allowedEmails []string) *Verifier {
	return &Verifier{audience: strings.TrimSuffix(audience, "/"), allowedEmails: allowedEmails}
}

// Middleware は OIDC トークンを検証してから next を呼ぶ。
func (v *Verifier) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := v.verify(r); err != nil {
			httpx.Handler(func(http.ResponseWriter, *http.Request) error { return err }).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (v *Verifier) verify(r *http.Request) error {
	if v.audience == "" {
		return nil
	}
	auth := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return httpx.Errorf(http.StatusUnauthorized, "Authorization header is not found")
	}
	payload, err := idtoken.Validate(r.Context(), token, v.audience)
	if err != nil {
		return httpx.WrapError(http.StatusUnauthorized, err, "invalid oidc token")
	}
	email, _ := payload.Claims["email"].(string)
	if verified, _ := payload.Claims["email_verified"].(bool); !verified || email == "" {
		return httpx.Errorf(http.StatusUnauthorized, "oidc token does not have a verified email")
	}
	for _, allowed := range v.allowedEmails {
		if strings.EqualFold(allowed, email) {
			return nil
		}
	}
	return httpx.Errorf(http.StatusForbidden, "%s is not allowed to invoke tasks", email)
}

// RetryCount はリクエストからリトライ回数を取り出す。Cloud Tasks 以外からの呼び出しでは 0 を返す。
func RetryCount(r *http.Request) int {
	n, err := strconv.Atoi(r.Header.Get(RetryCountHeader))
	if err != nil || n < 0 {
		return 0
	}
	return n
}
