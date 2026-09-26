package config

import "testing"

func TestNormalizeBillingAccount(t *testing.T) {
	cases := map[string]string{
		"":                                     "",
		"01F87B-7556CE-2E1852":                 "billingAccounts/01F87B-7556CE-2E1852",
		"billingAccounts/01F87B-7556CE-2E1852": "billingAccounts/01F87B-7556CE-2E1852",
	}
	for in, want := range cases {
		if got := normalizeBillingAccount(in); got != want {
			t.Errorf("normalizeBillingAccount(%q) = %q, want %q", in, got, want)
		}
	}
}
