package gcp

import (
	"errors"
	"strings"
	"testing"

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
