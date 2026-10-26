package mcp

import (
	"strings"
	"testing"

	"github.com/thehappieco/mailie/internal/service"
)

func TestListAccountsSaysAPausedMailboxIsSearchableButNotUpdated(t *testing.T) {
	read := service.AccountAccess{Read: true}
	text := accountsText([]service.Account{
		{ID: "acc_1", Email: "on@mail.example", Provider: "imap", State: "active", Access: read,
			Sync: service.AccountSync{Enabled: true}},
		{ID: "acc_2", Email: "paused@mail.example", Provider: "imap", State: "active", Access: read,
			Sync: service.AccountSync{Enabled: true, Paused: true, PausedSince: 1}},
		{ID: "acc_3", Email: "off@mail.example", Provider: "imap", State: "active", Access: read},
	})
	for _, want := range []string{
		"acc_1: on@mail.example (imap), active, sync on\n",
		"acc_2: paused@mail.example (imap), active, sync paused (searchable; new mail is not indexed until it is resumed)\n",
		"acc_3: off@mail.example (imap), active, sync off (not searchable)\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("list_accounts says\n%s\nwithout %q", text, want)
		}
	}
}
