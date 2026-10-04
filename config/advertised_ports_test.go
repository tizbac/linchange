package config

import (
	"strconv"
	"strings"
	"testing"
)

// withAdvertised injects an advertised block into the exchange section of the
// minimal config.
func withAdvertised(block string) string {
	return strings.Replace(minimalConfig,
		"  organization: Example Ltd\n",
		"  organization: Example Ltd\n"+block, 1)
}

// An advertised port is what Autodiscover tells a client to connect to, so a
// deployment behind a container or a proxy has to be able to state it. Zero
// keeps meaning "the standard port", which is what an untouched config says.
func TestLoadReadsAdvertisedPorts(t *testing.T) {
	cfg, err := Load(writeConfig(t, withAdvertised(`
  advertised:
    imap: 5143
    imaps: 5993
    pop3: 5110
    pop3s: 5995
    smtp: 5025
    smtps: 5465
    submission: 5587
    http: 58080
    https: 58443
`)))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := Ports{IMAP: 5143, IMAPS: 5993, POP3: 5110, POP3S: 5995, SMTP: 5025, SMTPS: 5465, Submit: 5587, HTTP: 58080, HTTPS: 58443}
	if cfg.Exchange.Advertised != want {
		t.Errorf("advertised ports = %+v, want %+v", cfg.Exchange.Advertised, want)
	}
}

func TestAdvertisedPortsDefaultToStandard(t *testing.T) {
	cfg, err := Load(writeConfig(t, minimalConfig))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Exchange.Advertised != (Ports{}) {
		t.Fatalf("an absent advertised block should decode to zero, got %+v", cfg.Exchange.Advertised)
	}
	if got := cfg.Exchange.Advertised.Or(cfg.Exchange.Advertised.IMAP, 143); got != 143 {
		t.Errorf("Or should fall back to the standard port, got %d", got)
	}
	if got := (Ports{IMAP: 5143}).Or(5143, 143); got != 5143 {
		t.Errorf("Or should prefer the advertised port, got %d", got)
	}
}

func TestValidateRejectsAdvertisedPortOutOfRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		port int
	}{
		{"above the range", 70000},
		{"negative", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, withAdvertised(`
  advertised:
    imap: `+strconv.Itoa(tc.port)+`
`)))
			if err == nil {
				t.Fatal("expected an error for an out of range advertised port")
			}
			if !strings.Contains(err.Error(), "exchange.advertised.imap") {
				t.Errorf("error should name the offending field, got: %v", err)
			}
		})
	}
}
