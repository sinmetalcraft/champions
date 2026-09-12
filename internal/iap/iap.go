// Package iap は Identity-Aware Proxy が付与する JWT を検証し、認証済みユーザを取り出す。
package iap

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"google.golang.org/api/idtoken"

	"github.com/sinmetalcraft/champions/internal/httpx"
)

// AssertionHeader は IAP が付与する JWT のヘッダ名。
const AssertionHeader = "X-Goog-IAP-JWT-Assertion"

// DevUserHeader はローカル開発時にログインするユーザを切り替えるヘッダ名。
// IAP の検証を行わない設定 (IAP_AUDIENCE が空) のときだけ見る。
const DevUserHeader = "X-Dev-User-Email"

// User は IAP で認証されたユーザ。
type User struct {
	// Email は Google Account の email。
	Email string
	// ID は JWT の sub。IAP では "accounts.google.com:{numeric id}" の形式。
	ID string
}

type contextKey struct{}

// WithUser は ctx に User を格納する。
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, contextKey{}, u)
}

// FromContext は ctx から User を取り出す。Authenticator を通っていれば必ず存在する。
func FromContext(ctx context.Context) (*User, error) {
	u, ok := ctx.Value(contextKey{}).(*User)
	if !ok || u == nil {
		return nil, httpx.Errorf(http.StatusUnauthorized, "unauthenticated")
	}
	return u, nil
}

// Authenticator は IAP の JWT を検証するミドルウェア。
type Authenticator struct {
	audience string
	devUser  *User
}

// NewAuthenticator は audience を検証する Authenticator を作る。
//
// devUserEmail を指定すると IAP の検証を行わず、常にそのユーザとして扱う (ローカル開発用)。
//
// audience は IAP を有効にして Deploy するまで値が分からないため、空でも起動できるようにしている。
// その場合はリクエストをすべて拒否したうえで、JWT が実際に持っている aud をログに出す。
// その値を IAP_AUDIENCE に設定して Deploy し直せば認証が通るようになる。
func NewAuthenticator(audience, devUserEmail string) (*Authenticator, error) {
	if devUserEmail != "" {
		slog.Warn("iap verification is disabled. all requests are authenticated as the dev user", "devUserEmail", devUserEmail)
		return &Authenticator{devUser: newDevUser(devUserEmail)}, nil
	}
	if audience == "" {
		slog.Warn("IAP_AUDIENCE is not set. all requests are rejected until it is configured")
	}
	return &Authenticator{audience: audience}, nil
}

// Middleware は認証済みユーザを request context に入れてから next を呼ぶ。
func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, err := a.authenticate(r)
		if err != nil {
			httpx.Handler(func(http.ResponseWriter, *http.Request) error { return err }).ServeHTTP(w, r)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithUser(r.Context(), u)))
	})
}

// newDevUser は IAP を通さないときの疑似ユーザを作る。
func newDevUser(email string) *User {
	email = strings.ToLower(email)
	return &User{Email: email, ID: "dev:" + email}
}

func (a *Authenticator) authenticate(r *http.Request) (*User, error) {
	if a.devUser != nil {
		// ヘッダで別のユーザとしてログインしたことにできる。参加者を切り替えた動作確認に使う。
		if email := strings.TrimSpace(r.Header.Get(DevUserHeader)); email != "" {
			return newDevUser(email), nil
		}
		return a.devUser, nil
	}
	token := r.Header.Get(AssertionHeader)
	if token == "" {
		return nil, httpx.Errorf(http.StatusUnauthorized, "%s header is not found", AssertionHeader)
	}
	if a.audience == "" {
		// 設定すべき値をログに出して次の Deploy に繋げる。認証は通さない。
		if unverified, err := idtoken.ParsePayload(token); err == nil {
			slog.Warn("IAP_AUDIENCE is not set", "actualAudience", unverified.Audience)
		}
		return nil, httpx.Errorf(http.StatusUnauthorized, "IAP_AUDIENCE is not configured")
	}
	payload, err := idtoken.Validate(r.Context(), token, a.audience)
	if err != nil {
		// IAP_AUDIENCE の設定ミスは切り分けが難しいので、JWT が実際に持っている aud をログに残す。
		// 署名を検証していない値なので認証には使わず、ログ出力だけに使う。
		if unverified, perr := idtoken.ParsePayload(token); perr == nil {
			slog.Warn("iap assertion is rejected", "expectedAudience", a.audience, "actualAudience", unverified.Audience)
		}
		return nil, httpx.WrapError(http.StatusUnauthorized, err, "invalid iap assertion")
	}
	email, _ := payload.Claims["email"].(string)
	if email == "" {
		return nil, httpx.Errorf(http.StatusUnauthorized, "iap assertion does not have email claim")
	}
	// IAP の email claim は "accounts.google.com:user@example.com" のように prefix が付くことがある。
	if i := strings.LastIndex(email, ":"); i >= 0 {
		email = email[i+1:]
	}
	return &User{Email: strings.ToLower(email), ID: payload.Subject}, nil
}
