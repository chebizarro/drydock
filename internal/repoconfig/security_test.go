package repoconfig

import (
	"reflect"
	"strings"
	"testing"
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
