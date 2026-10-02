package inbound

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/emersion/go-msgauth/dmarc"
)

// Signal weights for the inbound spam score. A clean, authenticated message
// scores 0; a typical unsolicited bulk email lands at or above the default
// Junk threshold; only a message that fails several independent checks at
// once reaches the default Reject threshold.
const (
	WeightSPFFail     = 2.0
	WeightSPFSoftfail = 1.0
	WeightSPFAbsent   = 0.5 // no SPF record at all
	WeightSPFError    = 0.5 // permerror/temperror: unverifiable
	WeightDKIMInvalid = 2.0
	WeightDKIMAbsent  = 1.0
	WeightDMARCReject = 3.0
	WeightDMARCErrant = 1.5
	WeightPTRBad      = 2.0
	WeightHELOIP      = 1.0
	WeightRateLimit   = 3.0
)

// RateWindow is the counting window for per-source volume.
const RateWindow = 10 * time.Minute

// RateLimit is the number of accepted messages per source within RateWindow
// before the rate-limit signal fires.
const RateLimit = 20

// Signals are the per-message facts the scorer combines. The backend fills
// these from the connection, the DNS checks and the message itself.
type Signals struct {
	SPF         SPFResult
	HasDKIM     bool          // at least one DKIM-Signature header present
	DKIMValid   bool          // at least one signature verified
	DMARCPolicy *dmarc.Policy // nil: no policy applies
	DMARCFails  bool          // policy reject/quarantine with no aligned identifier
	PTRPresent  bool
	HELOIsIP    bool
	RateCount   int // messages from this source within RateWindow
}

// ScorePart is one scored signal, kept for logging.
type ScorePart struct {
	Name  string
	Score float64
	Note  string
}

// Score is the spam verdict for one message.
type Score struct {
	Parts []ScorePart
	Total float64
}

// ScoreMessage computes the inbound spam score for one message.
func ScoreMessage(s Signals) Score {
	var sc Score
	add := func(name string, w float64, note string) {
		sc.Parts = append(sc.Parts, ScorePart{Name: name, Score: w, Note: note})
		sc.Total += w
	}

	switch s.SPF {
	case SPFPass:
	case SPFFail:
		add("spf_fail", WeightSPFFail, "SPF check failed")
	case SPFSoftfail:
		add("spf_softfail", WeightSPFSoftfail, "SPF softfail")
	case SPFNone:
		add("spf_absent", WeightSPFAbsent, "no SPF record")
	case SPFNeutral:
	case SPFPermError, SPFTempError:
		add("spf_error", WeightSPFError, "SPF check errored: "+string(s.SPF))
	}

	switch {
	case s.HasDKIM && s.DKIMValid:
	case s.HasDKIM:
		add("dkim_invalid", WeightDKIMInvalid, "DKIM signature failed verification")
	default:
		add("dkim_absent", WeightDKIMAbsent, "no DKIM signature")
	}

	if s.DMARCPolicy != nil && s.DMARCFails {
		switch *s.DMARCPolicy {
		case dmarc.PolicyReject:
			add("dmarc_reject", WeightDMARCReject, "DMARC reject policy, identifiers not aligned")
		case dmarc.PolicyQuarantine:
			add("dmarc_quarantine", WeightDMARCErrant, "DMARC quarantine policy, identifiers not aligned")
		}
	}

	if !s.PTRPresent {
		add("ptr_bad", WeightPTRBad, "no reverse DNS for client")
	}
	if s.HELOIsIP {
		add("helo_ip", WeightHELOIP, "EHLO/HELO presented an IP literal")
	}
	if s.RateCount > RateLimit {
		add("rate_limit", WeightRateLimit, fmt.Sprintf("%d messages in last %s", s.RateCount, RateWindow))
	}

	return sc
}

// Rates reads and writes the rate_buckets table.
type Rates struct{}

// NewRates returns the rate bucket repository.
func NewRates() *Rates { return &Rates{} }

// WindowCount returns how many times key has been counted within the last
// window.
func (r *Rates) WindowCount(ctx context.Context, db *sqlx.DB, scope, key string, window time.Duration) (int, error) {
	var n int
	err := db.GetContext(ctx, &n, `
		SELECT COALESCE(SUM(count), 0)::int
		FROM rate_buckets
		WHERE scope = $1 AND key = $2 AND bucket_start >= $3`,
		scope, key, time.Now().UTC().Add(-window))
	return n, err
}

// Increment counts one event for key in the current fixed window.
func (r *Rates) Increment(ctx context.Context, db *sqlx.DB, scope, key string, window time.Duration) error {
	now := time.Now().UTC()
	bucketStart := now.Add(-window).Truncate(window)
	_, err := db.ExecContext(ctx, `
		INSERT INTO rate_buckets (scope, key, bucket_start, count)
		VALUES ($1, $2, $3, 1)
		ON CONFLICT (scope, key, bucket_start) DO UPDATE SET count = rate_buckets.count + 1`,
		scope, key, bucketStart)
	if err != nil {
		return err
	}
	// Drop buckets older than two windows so the table stays small.
	_, err = db.ExecContext(ctx,
		`DELETE FROM rate_buckets WHERE bucket_start < $1`,
		now.Add(-2*window))
	return err
}
