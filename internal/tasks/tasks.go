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

// RetryCountHeader は Cloud Tasks がリトライ回数を入れて送るヘッダ。
const RetryCountHeader = "X-CloudTasks-TaskRetryCount"

// ProvisionRequest は worker に渡す払い出し処理の指示。
type ProvisionRequest struct {
	// AllocationID は処理対象の Allocation の Document ID。
	AllocationID string `json:"allocationID"`
}

// Queue は Cloud Tasks にタスクを投入する。
type Queue struct {
	client                *cloudtasks.Client
	parent                string
	workerBaseURL         string
	invokerServiceAccount string
}

// NewQueue は Cloud Tasks に接続した Queue を作る。
func NewQueue(ctx context.Context, projectID, location, queue, workerBaseURL, invokerServiceAccount string) (*Queue, error) {
	client, err := cloudtasks.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("tasks: failed to create cloudtasks client: %w", err)
	}
	return &Queue{
		client:                client,
		parent:                fmt.Sprintf("projects/%s/locations/%s/queues/%s", projectID, location, queue),
		workerBaseURL:         strings.TrimSuffix(workerBaseURL, "/"),
		invokerServiceAccount: invokerServiceAccount,
	}, nil
}

// Close は Cloud Tasks クライアントを閉じる。
func (q *Queue) Close() error { return q.client.Close() }

// EnqueueProvision は払い出し処理のタスクを投入する。
func (q *Queue) EnqueueProvision(ctx context.Context, allocationID string) error {
	body, err := json.Marshal(&ProvisionRequest{AllocationID: allocationID})
	if err != nil {
		return fmt.Errorf("tasks: failed to marshal provision request: %w", err)
	}
	_, err = q.client.CreateTask(ctx, &cloudtaskspb.CreateTaskRequest{
		Parent: q.parent,
		Task: &cloudtaskspb.Task{
			MessageType: &cloudtaskspb.Task_HttpRequest{
				HttpRequest: &cloudtaskspb.HttpRequest{
					Url:        q.workerBaseURL + ProvisionPath,
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
	if err != nil {
		return fmt.Errorf("tasks: failed to create provision task for %s: %w", allocationID, err)
	}
	return nil
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
