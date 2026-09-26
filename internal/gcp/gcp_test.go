package gcp

import (
	"errors"
	"strings"
	"testing"

	"cloud.google.com/go/iam/apiv1/iampb"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestPrintableTokens(t *testing.T) {
	// protobuf のワイヤ形式を模したバイト列から、読める文字列だけを取り出せること。
	in := []byte("\x12\x0fuser:foo@ex.jp\x1a\x0broles/owner\x22\x1eORG_MUST_INVITE_EXTERNAL_OWNERS")
	got := printableTokens(in)
	for _, want := range []string{"user:foo@ex.jp", "roles/owner", "ORG_MUST_INVITE_EXTERNAL_OWNERS"} {
		if !strings.Contains(got, want) {
			t.Errorf("printableTokens() = %q, want it to contain %q", got, want)
		}
	}
}

func TestStatusDetails(t *testing.T) {
	if got := statusDetails(errors.New("plain error")); got != "" {
		t.Errorf("statusDetails(plain error) = %q, want empty", got)
	}

	st, err := status.New(codes.InvalidArgument, "Request contains an invalid argument.").
		WithDetails(&errdetails.ErrorInfo{Reason: "ORG_MUST_INVITE_EXTERNAL_OWNERS"})
	if err != nil {
		t.Fatal(err)
	}
	got := statusDetails(st.Err())
	if !strings.Contains(got, "ORG_MUST_INVITE_EXTERNAL_OWNERS") {
		t.Errorf("statusDetails() = %q, want it to contain the reason", got)
	}
}

func TestSyncBindings(t *testing.T) {
	t.Run("grant new roles to empty policy", func(t *testing.T) {
		policy := &iampb.Policy{}
		changed := syncBindings(policy, "user:alice@example.com", []string{"roles/viewer", "roles/editor"})
		if !changed {
			t.Fatal("expected changed to be true")
		}
		if len(policy.Bindings) != 2 {
			t.Fatalf("expected 2 bindings, got %d", len(policy.Bindings))
		}
		if policy.Bindings[0].Role != "roles/viewer" || policy.Bindings[0].Members[0] != "user:alice@example.com" {
			t.Errorf("unexpected binding 0: %+v", policy.Bindings[0])
		}
		if policy.Bindings[1].Role != "roles/editor" || policy.Bindings[1].Members[0] != "user:alice@example.com" {
			t.Errorf("unexpected binding 1: %+v", policy.Bindings[1])
		}
	})

	t.Run("remove unneeded role and add new role", func(t *testing.T) {
		policy := &iampb.Policy{
			Bindings: []*iampb.Binding{
				{Role: "roles/viewer", Members: []string{"user:alice@example.com"}},
				{Role: "roles/owner", Members: []string{"user:admin@example.com"}},
			},
		}
		changed := syncBindings(policy, "user:alice@example.com", []string{"roles/editor"})
		if !changed {
			t.Fatal("expected changed to be true")
		}
		// roles/viewer は alice のみだったため削除され、roles/owner は admin のまま残り、roles/editor が追加される
		if len(policy.Bindings) != 2 {
			t.Fatalf("expected 2 bindings, got %d", len(policy.Bindings))
		}
		if policy.Bindings[0].Role != "roles/owner" || policy.Bindings[0].Members[0] != "user:admin@example.com" {
			t.Errorf("unexpected binding 0: %+v", policy.Bindings[0])
		}
		if policy.Bindings[1].Role != "roles/editor" || policy.Bindings[1].Members[0] != "user:alice@example.com" {
			t.Errorf("unexpected binding 1: %+v", policy.Bindings[1])
		}
	})

	t.Run("shared binding with another member", func(t *testing.T) {
		policy := &iampb.Policy{
			Bindings: []*iampb.Binding{
				{Role: "roles/viewer", Members: []string{"user:alice@example.com", "user:bob@example.com"}},
			},
		}
		changed := syncBindings(policy, "user:alice@example.com", []string{})
		if !changed {
			t.Fatal("expected changed to be true")
		}
		if len(policy.Bindings) != 1 {
			t.Fatalf("expected 1 binding, got %d", len(policy.Bindings))
		}
		if policy.Bindings[0].Role != "roles/viewer" {
			t.Errorf("unexpected role: %s", policy.Bindings[0].Role)
		}
		if len(policy.Bindings[0].Members) != 1 || policy.Bindings[0].Members[0] != "user:bob@example.com" {
			t.Errorf("unexpected members: %+v", policy.Bindings[0].Members)
		}
	})

	t.Run("no change returns false", func(t *testing.T) {
		policy := &iampb.Policy{
			Bindings: []*iampb.Binding{
				{Role: "roles/editor", Members: []string{"user:alice@example.com"}},
				{Role: "roles/owner", Members: []string{"user:admin@example.com"}},
			},
		}
		changed := syncBindings(policy, "user:alice@example.com", []string{"roles/editor"})
		if changed {
			t.Fatal("expected changed to be false")
		}
	})
}

