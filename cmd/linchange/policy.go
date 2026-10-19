package main

import (
	"github.com/linchange/linchange/config"
	"github.com/linchange/linchange/internal/auth/policy"
)

// policyFromConfig builds the policy object the administration service holds.
//
// It mirrors what the HTTP server does, so that a decision taken through the
// command line and one taken through the panel read the same configuration.
// Nothing in the commands asks the policy a question today; constructing it
// correctly anyway means the day one of them does, it does not silently get
// the zero value.
func policyFromConfig(cfg *config.Config) policy.Policy {
	return policy.Policy{
		Impersonation: policy.Impersonation{
			Allowed:       cfg.Auth.Impersonation.Allowed,
			RequireReason: cfg.Auth.Impersonation.RequireReason,
			RequireGrant:  cfg.Auth.Impersonation.RequireGrant,
		},
	}
}
