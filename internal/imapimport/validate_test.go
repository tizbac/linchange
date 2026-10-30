package imapimport

import (
	"context"
	"testing"
)

func TestValidateRemoteHost(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		host         string
		allowPrivate bool
		wantErr      bool
	}{
		{"203.0.113.10", false, false},
		{"127.0.0.1", false, true},
		{"127.0.0.1", true, false},
		{"10.0.0.5", false, true},
		{"10.0.0.5", true, false},
		{"169.254.169.254", false, true},
		{"169.254.169.254", true, true},
		{"::1", false, true},
		{"::1", true, false},
	}
	for _, tc := range cases {
		err := validateRemoteHost(ctx, tc.host, tc.allowPrivate)
		if tc.wantErr && err == nil {
			t.Errorf("validateRemoteHost(%q, %v) = nil, want an error", tc.host, tc.allowPrivate)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("validateRemoteHost(%q, %v) = %v, want nil", tc.host, tc.allowPrivate, err)
		}
	}
}
