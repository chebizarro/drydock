package repoconfig

import (
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

type NotificationsConfig struct {
	Concord *ConcordConfig `yaml:"concord"`
}

// ConcordConfig contains public routing material only. community is the
// community_id, not a NIP-29 group or a NIP-59 recipient pubkey.
type ConcordConfig struct {
	Community string   `yaml:"community"`
	Channel   string   `yaml:"channel"`
	Epoch     *int64   `yaml:"epoch"` // int64 so a negative value is a validation error, not a decode error that fails the whole file
	Relays    []string `yaml:"relays"`
}

func (c ConcordConfig) CredentialID() string {
	if c.Epoch == nil {
		return ""
	}
	return c.Community + "/" + c.Channel + "/" + strconv.FormatInt(*c.Epoch, 10)
}
func concordID(s string) bool {
	if len(s) != 64 || s != strings.ToLower(s) {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
func (c NotificationsConfig) Validate() error {
	if c.Concord == nil {
		return nil
	}
	n := c.Concord
	if !concordID(n.Community) || !concordID(n.Channel) {
		return errors.New("concord community and channel must be 64 lowercase hex characters")
	}
	if n.Epoch != nil && *n.Epoch < 0 {
		return errors.New("notifications.concord.epoch must be >= 0")
	}
	if n.Epoch == nil {
		return errors.New("concord epoch is required (including epoch 0)")
	}
	if len(n.Relays) == 0 || len(n.Relays) > 5 {
		return errors.New("concord requires 1 to 5 explicit relays")
	}
	seen := map[string]bool{}
	for _, raw := range n.Relays {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "wss" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.TrimSpace(raw) != raw || seen[raw] {
			return errors.New("concord relays must be unique wss URLs without credentials, query, or fragment")
		}
		seen[raw] = true
	}
	return nil
}
