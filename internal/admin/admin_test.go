package admin

import (
	"testing"

	"github.com/sinmetalcraft/champions/internal/model"
)

func TestHasSettingsChanged(t *testing.T) {
	tests := []struct {
		name string
		r1   []string
		a1   []string
		q1   []model.Quota
		r2   []string
		a2   []string
		q2   []model.Quota
		want bool
	}{
		{
			name: "no changes",
			r1:   []string{"roles/editor"},
			a1:   []string{"compute.googleapis.com"},
			q1:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", PreferredValue: 10}},
			r2:   []string{"roles/editor"},
			a2:   []string{"compute.googleapis.com"},
			q2:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", PreferredValue: 10}},
			want: false,
		},
		{
			name: "roles changed",
			r1:   []string{"roles/viewer"},
			a1:   []string{"compute.googleapis.com"},
			q1:   nil,
			r2:   []string{"roles/editor"},
			a2:   []string{"compute.googleapis.com"},
			q2:   nil,
			want: true,
		},
		{
			name: "apis changed",
			r1:   []string{"roles/editor"},
			a1:   []string{"compute.googleapis.com"},
			q1:   nil,
			r2:   []string{"roles/editor"},
			a2:   []string{"compute.googleapis.com", "storage.googleapis.com"},
			q2:   nil,
			want: true,
		},
		{
			name: "quotas changed preferred value",
			r1:   []string{"roles/editor"},
			a1:   []string{"compute.googleapis.com"},
			q1:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", PreferredValue: 10}},
			r2:   []string{"roles/editor"},
			a2:   []string{"compute.googleapis.com"},
			q2:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", PreferredValue: 20}},
			want: true,
		},
		{
			name: "quotas changed dimensions",
			r1:   []string{"roles/editor"},
			a1:   []string{"compute.googleapis.com"},
			q1:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", Dimensions: map[string]string{"region": "us-central1"}}},
			r2:   []string{"roles/editor"},
			a2:   []string{"compute.googleapis.com"},
			q2:   []model.Quota{{Service: "compute.googleapis.com", QuotaID: "CPUS", Dimensions: map[string]string{"region": "asia-northeast1"}}},
			want: true,
		},
		{
			name: "empty to non-empty",
			r1:   []string{},
			a1:   []string{},
			q1:   []model.Quota{},
			r2:   []string{"roles/editor"},
			a2:   []string{},
			q2:   []model.Quota{},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := hasSettingsChanged(tt.r1, tt.a1, tt.q1, tt.r2, tt.a2, tt.q2)
			if got != tt.want {
				t.Errorf("hasSettingsChanged() = %v, want %v", got, tt.want)
			}
		})
	}
}
