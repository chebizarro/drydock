// Package knowledge provides the versioned Nostr protocol security corpus.
package knowledge

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

//go:embed pack.v1.json
var packData []byte

type Entry struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

type Vulnerability struct {
	Entry
	Adversary string `json:"adversary"`
}

type Pack struct {
	Version          string          `json:"version"`
	Title            string          `json:"title"`
	Paper            string          `json:"paper"`
	ReviewerPreamble Entry           `json:"reviewer_preamble"`
	AdversaryModels  []Entry         `json:"adversary_models"`
	SecurityGoals    []Entry         `json:"security_goals"`
	Vulnerabilities  []Vulnerability `json:"vulnerabilities"`
	NIPCheatSheet    []Entry         `json:"nip_cheat_sheet"`
}

var (
	loadOnce sync.Once
	loaded   Pack
	loadErr  error
)

func Load() (Pack, error) {
	loadOnce.Do(func() {
		loadErr = json.Unmarshal(packData, &loaded)
		if loadErr == nil {
			loadErr = validate(loaded)
		}
	})
	return loaded, loadErr
}

func Version() string {
	pack, err := Load()
	if err != nil {
		return ""
	}
	return pack.Version
}

// Context renders the corpus for the nostr-protocol contextbuilder layer.
func Context() (string, error) {
	pack, err := Load()
	if err != nil {
		return "", err
	}
	return render(pack), nil
}

// ReviewerSystemPreamble returns the system-level instructions for a Nostr review.
func ReviewerSystemPreamble() (string, error) {
	pack, err := Load()
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s\nSource: %s", pack.ReviewerPreamble.Text, pack.ReviewerPreamble.Source), nil
}

func validate(pack Pack) error {
	if strings.TrimSpace(pack.Version) == "" {
		return fmt.Errorf("knowledge pack has no version")
	}
	groups := [][]Entry{pack.AdversaryModels, pack.SecurityGoals, pack.NIPCheatSheet, []Entry{pack.ReviewerPreamble}}
	for _, group := range groups {
		for _, entry := range group {
			if err := validateEntry(entry); err != nil {
				return err
			}
		}
	}
	for _, vulnerability := range pack.Vulnerabilities {
		if err := validateEntry(vulnerability.Entry); err != nil {
			return err
		}
		if vulnerability.Adversary != "MU" && vulnerability.Adversary != "MS" {
			return fmt.Errorf("entry %s has invalid adversary %q", vulnerability.ID, vulnerability.Adversary)
		}
	}
	return nil
}

func validateEntry(entry Entry) error {
	if strings.TrimSpace(entry.ID) == "" || strings.TrimSpace(entry.Title) == "" || strings.TrimSpace(entry.Text) == "" {
		return fmt.Errorf("knowledge entry is incomplete: %q", entry.ID)
	}
	if strings.TrimSpace(entry.Source) == "" {
		return fmt.Errorf("knowledge entry %s has no source", entry.ID)
	}
	return nil
}

func render(pack Pack) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nVersion: %s\nPrimary paper: %s\n\n", pack.Title, pack.Version, pack.Paper)
	renderEntries(&b, "Adversary models", pack.AdversaryModels)
	renderEntries(&b, "Security goals and specification gaps", pack.SecurityGoals)
	b.WriteString("## Vulnerability briefs\n\n")
	for _, entry := range pack.Vulnerabilities {
		fmt.Fprintf(&b, "### %s — %s [%s]\n%s\nSource: %s\n\n", entry.ID, entry.Title, entry.Adversary, entry.Text, entry.Source)
	}
	renderEntries(&b, "NIP cheat sheet", pack.NIPCheatSheet)
	return strings.TrimSpace(b.String())
}

func renderEntries(b *strings.Builder, heading string, entries []Entry) {
	fmt.Fprintf(b, "## %s\n\n", heading)
	for _, entry := range entries {
		fmt.Fprintf(b, "### %s — %s\n%s\nSource: %s\n\n", entry.ID, entry.Title, entry.Text, entry.Source)
	}
}
