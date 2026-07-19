package repoconfig

import "testing"

func TestPaymentsAcceptZapsDefaultsWhenEnabled(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\npayments:\n  enabled: true\n  price_sats: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Payments.AcceptsZaps() {
		t.Fatal("zaps should be accepted by default when payments are enabled")
	}
}

func TestPaymentsAcceptZapsCanBeDisabled(t *testing.T) {
	cfg, err := Parse([]byte("version: 1\npayments:\n  enabled: true\n  price_sats: 100\n  accept_zaps: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Payments.AcceptsZaps() {
		t.Fatal("accept_zaps=false should disable zap authorization")
	}
}

func TestPaymentsAcceptZapsFalseWhenPaymentsDisabled(t *testing.T) {
	cfg := Default()
	if cfg.Payments.AcceptsZaps() {
		t.Fatal("disabled payments must not accept zaps")
	}
}
