package iap

import (
	"context"
	"testing"
)

func TestCloudRunAudienceWithoutKService(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	if _, err := CloudRunAudience(context.Background()); err == nil {
		t.Error("CloudRunAudience() should fail when K_SERVICE is not set")
	}
}

// Cloud Run 以外では audience を検出できないので、Authenticator は作れても全リクエストを拒否する。
func TestNewAuthenticatorWithoutAudience(t *testing.T) {
	t.Setenv("K_SERVICE", "")
	a, err := NewAuthenticator(context.Background(), "", "")
	if err != nil {
		t.Fatalf("NewAuthenticator() = %v, want nil", err)
	}
	if a.audience != "" {
		t.Errorf("audience = %q, want empty", a.audience)
	}
	if a.devUser != nil {
		t.Error("devUser should be nil")
	}
}

func TestNewAuthenticatorWithDevUser(t *testing.T) {
	a, err := NewAuthenticator(context.Background(), "", "Foo@Example.com")
	if err != nil {
		t.Fatalf("NewAuthenticator() = %v, want nil", err)
	}
	if a.devUser == nil || a.devUser.Email != "foo@example.com" {
		t.Errorf("devUser = %+v, want foo@example.com", a.devUser)
	}
}

// 明示的に指定した audience はメタデータより優先される。
func TestNewAuthenticatorWithExplicitAudience(t *testing.T) {
	want := "/projects/123/global/backendServices/456"
	a, err := NewAuthenticator(context.Background(), want, "")
	if err != nil {
		t.Fatalf("NewAuthenticator() = %v, want nil", err)
	}
	if a.audience != want {
		t.Errorf("audience = %q, want %q", a.audience, want)
	}
}
