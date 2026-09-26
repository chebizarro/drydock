package repoconfig

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSecurityConfigParsing(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want SecurityConfig
	}{
		{
			name: "defaults",
			yaml: "version: 1\n",
			want: Default().Security,
		},
		{
			name: "full schema",
			yaml: `version: 1
security:
  enabled: true
  gate_severity: critical
  min_confidence: 0.75
  reviewer_route: custom-reviewer
  classify_route: custom-classifier
  verify_votes: 2
  cwe_taxonomy: false
  sast: false
  taint: false
  surface: false
  sca: true
  secret_scan: true
  audit:
    localizer: antares
    depth: deep
    verify_votes: 5
    auto_on_snapshot: true
    sarif: false
`,
			want: SecurityConfig{
				Enabled:       true,
				GateSeverity:  "critical",
				MinConfidence: 0.75,
				ReviewerRoute: "custom-reviewer",
				ClassifyRoute: "custom-classifier",
				VerifyVotes:   2,
				CWETaxonomy:   false,
				SAST:          false,
				Taint:         false,
				Surface:       false,
				SCA:           true,
				SecretScan:    true,
				Audit: SecurityAuditConfig{
					Localizer:      "antares",
					Depth:          "deep",
					VerifyVotes:    5,
					AutoOnSnapshot: true,
					SARIF:          false,
				},
				Nostr: Default().Security.Nostr,
			},
		},
		{
			name: "confidence boundaries",
			yaml: "version: 1\nsecurity:\n  min_confidence: 0\n",
			want: func() SecurityConfig {
				cfg := Default().Security
				cfg.MinConfidence = 0
				return cfg
			}(),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if !reflect.DeepEqual(cfg.Security, tt.want) {
				t.Fatalf("Security = %#v, want %#v", cfg.Security, tt.want)
			}
		})
	}
}

func TestSecurityNostrConfigParsing(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
security:
  nostr:
    enabled: true
    min_detect_confidence: 0.75
    roles: [client, signer]
    rules:
      exclude: [nostr-v6]
    knowledge_pack: false
    absence_analysis: false
    verify_votes: 3
    probe:
      enabled: true
      active: true
      i_understand: true
      timeout: 12s
`))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := NostrConfig{
		Enabled:             "true",
		MinDetectConfidence: 0.75,
		Roles:               NostrRolesConfig{Roles: []string{"client", "signer"}},
		Rules:               NostrRulesConfig{Exclude: []string{"NOSTR-V6"}},
		KnowledgePack:       false,
		AbsenceAnalysis:     false,
		VerifyVotes:         3,
		Probe: NostrProbeConfig{
			Enabled:     true,
			Active:      true,
			IUnderstand: true,
			Timeout:     12 * time.Second,
		},
	}
	if !reflect.DeepEqual(cfg.Security.Nostr, want) {
		t.Fatalf("Security.Nostr = %#v, want %#v", cfg.Security.Nostr, want)
	}
}

func TestSecurityNostrAuthorizedTargetsRejected(t *testing.T) {
	cfg, err := Parse([]byte(`version: 1
security:
  nostr:
    probe:
      enabled: true
      active: true
      i_understand: true
      authorized_targets: [wss://third-party.example]
`))
	if err == nil || !strings.Contains(err.Error(), "authorized_targets is operator-only") {
		t.Fatalf("Parse() error = %v, want operator-only rejection", err)
	}
	if !reflect.DeepEqual(cfg.Security.Nostr.Probe, Default().Security.Nostr.Probe) {
		t.Fatalf("rejected repo targets changed probe config: %#v", cfg.Security.Nostr.Probe)
	}
	if cfg.Security.Nostr.Probe.Enabled || cfg.Security.Nostr.Probe.Active || cfg.Security.Nostr.Probe.IUnderstand {
		t.Fatalf("rejected repo probe config did not fail closed: %#v", cfg.Security.Nostr.Probe)
	}
}

func TestSecurityConfigValidation(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name:    "unknown security field",
			yaml:    "version: 1\nsecurity:\n  unknown: true\n",
			wantErr: "field unknown not found",
		},
		{
			name:    "unknown audit field",
			yaml:    "version: 1\nsecurity:\n  audit:\n    unknown: true\n",
			wantErr: "field unknown not found",
		},
		{
			name:    "invalid severity",
			yaml:    "version: 1\nsecurity:\n  gate_severity: extreme\n",
			wantErr: "invalid security.gate_severity",
		},
		{
			name:    "confidence below range",
			yaml:    "version: 1\nsecurity:\n  min_confidence: -0.01\n",
			wantErr: "security.min_confidence must be in [0,1]",
		},
		{
			name:    "confidence above range",
			yaml:    "version: 1\nsecurity:\n  min_confidence: 1.01\n",
			wantErr: "security.min_confidence must be in [0,1]",
		},
		{
			name:    "invalid nostr enabled",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    enabled: sometimes\n",
			wantErr: "security.nostr.enabled must be auto, true, or false",
		},
		{
			name:    "invalid nostr role",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    roles: [client, server]\n",
			wantErr: "invalid role",
		},
		{
			name:    "invalid nostr rule mode",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    rules: none\n",
			wantErr: "rules must be",
		},
		{
			name:    "invalid nostr confidence",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    min_detect_confidence: 1.01\n",
			wantErr: "security.nostr.min_detect_confidence must be in [0,1]",
		},
		{
			name:    "invalid nostr verify votes",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    verify_votes: 0\n",
			wantErr: "security.nostr.verify_votes must be at least 1",
		},
		{
			name:    "invalid probe timeout",
			yaml:    "version: 1\nsecurity:\n  nostr:\n    probe:\n      enabled: true\n      timeout: 0s\n",
			wantErr: "security.nostr.probe.timeout must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.yaml))
			if err == nil {
				t.Fatal("Parse() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Parse() error = %q, want substring %q", err, tt.wantErr)
			}
			if cfg.Security.Enabled {
				t.Fatal("invalid security policy must fail closed with security disabled")
			}
			if !reflect.DeepEqual(cfg.Security, Default().Security) {
				t.Fatalf("invalid policy returned Security = %#v, want defaults %#v", cfg.Security, Default().Security)
			}
		})
	}
}

// TestSecurityAuditAutoOnSnapshotDeprecatedKeyTolerated pins the backward-
// compatibility property: a .drydock.yaml still carrying the deprecated
// auto_on_snapshot key must PARSE and RETAIN its other real settings. Because
// Parse uses KnownFields(true) and the hot callers fall back to Default() on any
// parse error, deleting the field would silently downgrade a live repository's
// entire security/review/ignore policy. This test fails against that deletion.
func TestSecurityAuditAutoOnSnapshotDeprecatedKeyTolerated(t *testing.T) {
	data := []byte("version: 1\n" +
		"review:\n  severity_floor: high\n" +
		"security:\n  enabled: true\n  gate_severity: critical\n  audit:\n    auto_on_snapshot: true\n")

	cfg, err := Parse(data)
	if err != nil {
		t.Fatalf("deprecated auto_on_snapshot key must parse without error, got: %v", err)
	}
	// Non-default values below prove the config did NOT fall back to Default().
	if cfg.Review.SeverityFloor != "high" {
		t.Fatalf("real review setting lost: severity_floor = %q, want high (fell back to defaults)", cfg.Review.SeverityFloor)
	}
	if !cfg.Security.Enabled || cfg.Security.GateSeverity != "critical" {
		t.Fatalf("real security settings lost: %+v (fell back to defaults)", cfg.Security)
	}
}
