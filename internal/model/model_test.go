package model_test

import (
	"testing"

	"github.com/sinmetalcraft/champions/internal/model"
)

func TestValidateEventCode(t *testing.T) {
	cases := []struct {
		code string
		want bool
	}{
		{"handson", true},
		{"go-handson-2026", true},
		{"a1", true},
		{"", false},
		{"a", false},                          // 1 文字は短すぎる
		{"1handson", false},                   // 数字始まり
		{"handson-", false},                   // ハイフン終わり
		{"Handson", false},                    // 大文字
		{"hands_on", false},                   // アンダースコア
		{"hands--on", false},                  // 連続ハイフン
		{"abcdefghijklmnopqrstuvwxy", true},   // 25 文字
		{"abcdefghijklmnopqrstuvwxyz", false}, // 26 文字は ProjectID が 30 文字を超える
	}
	for _, tt := range cases {
		err := model.ValidateEventCode(tt.code)
		if got := err == nil; got != tt.want {
			t.Errorf("ValidateEventCode(%q) valid=%v, want %v (err=%v)", tt.code, got, tt.want, err)
		}
	}
}

func TestQuotaPreferenceID(t *testing.T) {
	q := model.Quota{
		Service:    "compute.googleapis.com",
		QuotaID:    "CPUS-per-project-region",
		Dimensions: map[string]string{"region": "asia-northeast1"},
	}
	got := q.PreferenceID()
	want := "champions-compute-cpus-per-project-region-region-asia-northeast1"
	if len(want) > 63 {
		want = want[:63]
	}
	if got != want {
		t.Errorf("PreferenceID() = %q, want %q", got, want)
	}

	// Dimensions の順序が変わっても同じ ID になること。
	a := model.Quota{Service: "compute.googleapis.com", QuotaID: "X", Dimensions: map[string]string{"region": "us", "zone": "a"}}
	b := model.Quota{Service: "compute.googleapis.com", QuotaID: "X", Dimensions: map[string]string{"zone": "a", "region": "us"}}
	if a.PreferenceID() != b.PreferenceID() {
		t.Errorf("PreferenceID() is not deterministic: %q != %q", a.PreferenceID(), b.PreferenceID())
	}
}

func TestEventValidate(t *testing.T) {
	base := func() *model.Event {
		return &model.Event{Code: "handson", Roles: []string{"roles/owner"}, APIs: []string{"compute.googleapis.com"}}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("Validate() = %v, want nil", err)
	}

	e := base()
	e.Roles = []string{"owner"}
	if err := e.Validate(); err == nil {
		t.Error("Validate() should reject a role without the roles/ prefix")
	}

	e = base()
	e.APIs = []string{"compute"}
	if err := e.Validate(); err == nil {
		t.Error("Validate() should reject an api without a dot")
	}

	e = base()
	e.Quotas = []model.Quota{{Service: "compute.googleapis.com"}}
	if err := e.Validate(); err == nil {
		t.Error("Validate() should reject a quota without quotaID")
	}
}

func TestAllocationID(t *testing.T) {
	if got, want := model.AllocationID("handson", "Foo@Example.com"), "handson:foo@example.com"; got != want {
		t.Errorf("AllocationID() = %q, want %q", got, want)
	}
}
