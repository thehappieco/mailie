package provider_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"

	"github.com/thehappieco/mailie/internal/provider"
)

func TestGmailNeverSyncsTheArchiveOrTheFlagViews(t *testing.T) {
	// [Gmail]/All Mail holds every message a second time, and Starred and
	// Important are views of flags rather than places a message lives.
	// Indexing them would double the download and triple the row count.
	p := provider.ProfileFor(provider.KindGmail)
	for _, role := range []provider.FolderRole{provider.RoleAll, provider.RoleFlagged, provider.RoleImportant} {
		if p.SyncsRole(role) {
			t.Errorf("gmail should not sync %s", role)
		}
	}
	if !p.SyncsRole(provider.RoleInbox) || !p.SyncsRole(provider.RoleSent) {
		t.Error("gmail must sync the inbox and sent mail")
	}
}

func TestGmailNeverSyncsAllMailWhateverRoleItEndsUpWith(t *testing.T) {
	// The promise is about the folder: an override calling All Mail the
	// archive, or a role lost to another folder, must not start indexing the
	// whole mailbox a second time.
	gmail := provider.ProfileFor(provider.KindGmail)
	allMail := provider.Folder{Name: "[Gmail]/All Mail", Attrs: []imap.MailboxAttr{imap.MailboxAttrAll}, Selectable: true}
	for _, role := range []provider.FolderRole{provider.RoleAll, provider.RoleArchive, provider.RoleNone} {
		if gmail.SyncsFolder(allMail, role) {
			t.Errorf("gmail syncs All Mail when its role is %q", role)
		}
	}
	starred := provider.Folder{Name: "[Gmail]/Starred", Selectable: true} // no SPECIAL-USE sent
	if gmail.SyncsFolder(starred, provider.RoleNone) {
		t.Error("gmail syncs Starred found by name")
	}
	label := provider.Folder{Name: "Receipts", Selectable: true}
	if !gmail.SyncsFolder(label, provider.RoleNone) {
		t.Error("gmail does not sync a label")
	}
	if gmail.SyncsFolder(provider.Folder{Name: "[Gmail]", Selectable: false}, provider.RoleNone) {
		t.Error("gmail syncs a folder that cannot be selected")
	}
	// Everywhere else every selectable folder is synced, an archive included.
	for _, kind := range []provider.Kind{provider.KindMicrosoft, provider.KindIMAP} {
		p := provider.ProfileFor(kind)
		archive := provider.Folder{Name: "Archive", Attrs: []imap.MailboxAttr{imap.MailboxAttrArchive}, Selectable: true}
		if !p.SyncsFolder(archive, provider.RoleArchive) || !p.SyncsFolder(allMail, provider.RoleAll) {
			t.Errorf("%s does not sync every selectable folder", kind)
		}
	}
}

func TestStarringOnGmailIsAFlagNotAMove(t *testing.T) {
	p := provider.ProfileFor(provider.KindGmail)
	if !p.IsFlagOnly(provider.RoleFlagged) {
		t.Error("moving a message into Starred would be wrong; setting \\Flagged is what was meant")
	}
	if p.IsFlagOnly(provider.RoleTrash) {
		t.Error("the trash is a real folder: moving into it is exactly right")
	}
}

func TestArchivingOnGmailTargetsAllMail(t *testing.T) {
	// Never \Deleted plus EXPUNGE in the inbox: the account's own
	// expungeBehavior setting can turn that into the trash or a permanent
	// delete.
	if got := provider.ProfileFor(provider.KindGmail).ArchiveRole; got != provider.RoleAll {
		t.Fatalf("gmail archive target = %q, want the all-mail folder", got)
	}
}

func TestNeitherGmailNorMicrosoftAppendsASentCopy(t *testing.T) {
	// Both file it themselves; a second copy is the duplicate every user
	// notices.
	for _, kind := range []provider.Kind{provider.KindGmail, provider.KindMicrosoft} {
		if provider.ProfileFor(kind).SaveSentDefault {
			t.Errorf("%s should not append a sent copy by default", kind)
		}
	}
	if !provider.ProfileFor(provider.KindIMAP).SaveSentDefault {
		t.Error("a generic server does not file the copy, so we must")
	}
}

func TestGmailFlagsBelongToTheMessageNotTheLabelCopy(t *testing.T) {
	if !provider.ProfileFor(provider.KindGmail).FlagsSharedAcrossFolders {
		t.Error("marking the inbox copy read marks every label copy read; the index has to follow")
	}
	if provider.ProfileFor(provider.KindIMAP).FlagsSharedAcrossFolders {
		t.Error("on a normal server each folder's copy carries its own flags")
	}
}

func TestExchangeFolderRolesAreFoundByLocalisedName(t *testing.T) {
	// Exchange Online advertises no SPECIAL-USE at all, so the name table is
	// the only way to find the Sent folder — and the names are translated per
	// mailbox.
	p := provider.ProfileFor(provider.KindMicrosoft)
	for name, want := range map[string]provider.FolderRole{
		"Sent Items":         provider.RoleSent,
		"Itens Enviados":     provider.RoleSent,
		"Éléments envoyés":   provider.RoleSent,
		"Gesendete Elemente": provider.RoleSent,
		"Deleted Items":      provider.RoleTrash,
		"Itens Excluídos":    provider.RoleTrash,
		"Junk Email":         provider.RoleJunk,
		"Rascunhos":          provider.RoleDrafts,
	} {
		if got := p.RoleForName(name); got != want {
			t.Errorf("RoleForName(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestANestedFolderIsMatchedByItsLeaf(t *testing.T) {
	// Some tenants nest the defaults under the inbox.
	p := provider.ProfileFor(provider.KindMicrosoft)
	if got := p.RoleForName("INBOX/Sent Items"); got != provider.RoleSent {
		t.Fatalf("RoleForName = %q, want sent", got)
	}
}

func TestAnUnknownFolderNameHasNoRole(t *testing.T) {
	p := provider.ProfileFor(provider.KindMicrosoft)
	if got := p.RoleForName("Projeto Fênix"); got != provider.RoleNone {
		t.Fatalf("a user folder should have no role, got %q", got)
	}
}

func TestSpecialUseAttributesMapToRoles(t *testing.T) {
	for attr, want := range map[imap.MailboxAttr]provider.FolderRole{
		imap.MailboxAttrAll:       provider.RoleAll,
		imap.MailboxAttrSent:      provider.RoleSent,
		imap.MailboxAttrDrafts:    provider.RoleDrafts,
		imap.MailboxAttrTrash:     provider.RoleTrash,
		imap.MailboxAttrJunk:      provider.RoleJunk,
		imap.MailboxAttrArchive:   provider.RoleArchive,
		imap.MailboxAttrFlagged:   provider.RoleFlagged,
		imap.MailboxAttrImportant: provider.RoleImportant,
		imap.MailboxAttrNoSelect:  provider.RoleNone,
	} {
		if got := provider.RoleFromAttr(attr); got != want {
			t.Errorf("RoleFromAttr(%q) = %q, want %q", attr, got, want)
		}
	}
}

func TestMicrosoftRenewsIdleFarMoreOftenThanTheRFCRequires(t *testing.T) {
	// Exchange has shipped periods where EXISTS was only flushed after DONE,
	// which makes the renewal interval the real notification latency.
	ms := provider.ProfileFor(provider.KindMicrosoft).IdleRenew
	gmail := provider.ProfileFor(provider.KindGmail).IdleRenew
	if ms >= gmail {
		t.Errorf("microsoft renew = %v, gmail = %v; microsoft must be shorter", ms, gmail)
	}
	// Under the 28 minutes at which go-imap restarts IDLE on its own.
	if gmail.Minutes() >= 28 {
		t.Errorf("gmail renew = %v, must be under the client's own 28 minute restart", gmail)
	}
}

func TestTheConnectionBudgetStaysWellUnderGmailsCap(t *testing.T) {
	// Fifteen is the cap across every client the person runs, not ours alone.
	if n := provider.ProfileFor(provider.KindGmail).MaxConnections; n > 4 {
		t.Fatalf("gmail budget = %d; leave room for the user's own clients", n)
	}
}

func TestExchangeSubmissionIsPacedToWhatItAccepts(t *testing.T) {
	if n := provider.ProfileFor(provider.KindMicrosoft).SMTPMaxPerMinute; n > 30 {
		t.Fatalf("microsoft accepts thirty messages a minute per mailbox, profile says %d", n)
	}
	// The advertised SIZE is 150 MB and the real limit is about 35.
	if n := provider.ProfileFor(provider.KindMicrosoft).SMTPMaxSize; n > 40<<20 {
		t.Fatalf("microsoft size limit = %d, want the enforced ~35 MB not the advertised 150", n)
	}
}

func TestGoogleForbidsTheDeviceFlowForMailScopes(t *testing.T) {
	if provider.ProfileFor(provider.KindGmail).DeviceCodeSupported {
		t.Fatal("https://mail.google.com/ is not on Google's device-flow scope list")
	}
}

func TestOnlyGmailAnswersARefusedTokenWithAContinuation(t *testing.T) {
	if got := provider.ProfileFor(provider.KindGmail).AuthFailureShape; got != provider.AuthFailureJSONContinuation {
		t.Errorf("gmail failure shape = %q", got)
	}
	if got := provider.ProfileFor(provider.KindMicrosoft).AuthFailureShape; got != provider.AuthFailureTagged {
		t.Errorf("microsoft failure shape = %q", got)
	}
}

func TestAnUnknownProviderIsRejected(t *testing.T) {
	if _, err := provider.ParseKind("yahoo"); err == nil {
		t.Fatal("an unknown provider must not parse into a silent default")
	}
	for _, name := range []string{"gmail", "microsoft", "imap"} {
		if _, err := provider.ParseKind(name); err != nil {
			t.Errorf("ParseKind(%q): %v", name, err)
		}
	}
}

func TestAPartPathRendersTheWayIMAPWritesIt(t *testing.T) {
	for _, tc := range []struct {
		path []int
		want string
	}{
		{nil, ""},
		{[]int{1}, "1"},
		{[]int{1, 2}, "1.2"},
		{[]int{2, 1, 3}, "2.1.3"},
	} {
		if got := provider.PathString(tc.path); got != tc.want {
			t.Errorf("PathString(%v) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRetryableParksOnlyOnFailuresAHumanMustFix(t *testing.T) {
	for err, want := range map[error]bool{
		provider.ErrNeedsReauth:        false, // stop; only consent fixes it
		provider.ErrUnsupported:        false,
		provider.ErrTerminal:           false,
		provider.ErrConnClosed:         true, // Gmail does this every hour
		provider.ErrTemporary:          true,
		provider.ErrRateLimited:        true,
		provider.ErrNotConnected:       true, // mailbox-side; retry slowly
		provider.ErrTooManyConnections: true,
		provider.ErrUIDValidityChanged: true,
	} {
		if got := provider.Retryable(err); got != want {
			t.Errorf("Retryable(%v) = %v, want %v", err, got, want)
		}
	}
	if provider.Retryable(nil) {
		t.Error("Retryable(nil) should be false")
	}
}

func TestEveryFailureHasAShortClassForTheLog(t *testing.T) {
	// A refusal that survived a refresh started as an authentication
	// failure; the log has to call it what it became.
	refreshed := fmt.Errorf("%w: the server rejected a freshly refreshed access token", provider.ErrNeedsReauth)
	for err, want := range map[error]string{
		refreshed:                   "needs_reauth",
		provider.ErrAuthFailed:      "auth_failed",
		provider.ErrNotConnected:    "not_connected",
		provider.ErrConnClosed:      "connection_closed",
		provider.ErrTemporary:       "temporary",
		&provider.RecipientError{}:  "terminal",
		context.DeadlineExceeded:    "timeout",
		errors.New("something new"): "other",
		provider.ErrAuthUnsupported: "auth_unsupported", // named for what it is, not for the class it belongs to
		provider.ErrInsecure:        "insecure",
	} {
		if got := provider.Class(err); got != want {
			t.Errorf("Class(%v) = %q, want %q", err, got, want)
		}
	}
	if provider.Class(nil) != "" {
		t.Error("Class(nil) should be empty")
	}
}

func TestARejectedRecipientIsTerminalAndNamesTheAddressOnlyToTheCaller(t *testing.T) {
	// The caller reads the address from Rejected to fix it; the error text
	// is what reaches a log, and a recipient's address never goes there.
	err := &provider.RecipientError{Rejected: []provider.RejectedRecipient{
		{Address: "nobody@example.com", Code: 550, Message: "no such user"},
	}}
	if provider.Retryable(err) {
		t.Error("retrying a refused recipient just refuses again")
	}
	if got := err.Error(); strings.Contains(got, "nobody") || strings.Contains(got, "example.com") {
		t.Errorf("the error text names the address: %q", got)
	}
	if err.Rejected[0].Address != "nobody@example.com" {
		t.Errorf("rejected = %+v", err.Rejected)
	}
}

// iCloud names its folders the way Apple Mail does, and without SPECIAL-USE
// the generic name table is all that finds them.
func TestICloudFolderNamesAreFoundByTheGenericTable(t *testing.T) {
	p := provider.ProfileFor(provider.KindIMAP)
	for name, want := range map[string]provider.FolderRole{
		"Sent Messages":    provider.RoleSent,
		"Deleted Messages": provider.RoleTrash,
		"Drafts":           provider.RoleDrafts,
		"Junk":             provider.RoleJunk,
		"Archive":          provider.RoleArchive,
	} {
		if got := p.RoleForName(name); got != want {
			t.Errorf("%q: role %q, want %q", name, got, want)
		}
	}
}

func TestAnOverrideBeatsTheServerAndTheNameTable(t *testing.T) {
	// A person who has said which folder is which must not be overruled by a
	// guess, and not by the server's attribute either.
	p := provider.ProfileFor(provider.KindMicrosoft)
	f := provider.Folder{Name: "Archive", Attrs: []imap.MailboxAttr{imap.MailboxAttrSent}}
	role, source := provider.ResolveRole(f, p, map[string]string{"trash": "archive"})
	if role != provider.RoleTrash || source != provider.RoleSourceOverride {
		t.Fatalf("got %q from %q, want trash from the override", role, source)
	}
}

func TestTheInboxIsFoundByNameWhateverItsCase(t *testing.T) {
	role, source := provider.ResolveRole(provider.Folder{Name: "Inbox"}, provider.ProfileFor(provider.KindIMAP), nil)
	if role != provider.RoleInbox || source != provider.RoleSourceInbox {
		t.Fatalf("got %q from %q, want the inbox", role, source)
	}
}

func TestSpecialUseBeatsTheNameTable(t *testing.T) {
	// A folder called "Sent" that the server marks \Trash is the trash.
	f := provider.Folder{Name: "Sent", Attrs: []imap.MailboxAttr{imap.MailboxAttrTrash}}
	role, source := provider.ResolveRole(f, provider.ProfileFor(provider.KindIMAP), nil)
	if role != provider.RoleTrash || source != provider.RoleSourceSpecialUse {
		t.Fatalf("got %q from %q, want trash from SPECIAL-USE", role, source)
	}
}

func TestAnOverrideForAnUnknownRoleIsIgnored(t *testing.T) {
	// The folders.role CHECK would refuse it, and failing the whole
	// discovery over a typo in a setting is worse than ignoring the typo.
	f := provider.Folder{Name: "Receipts"}
	role, source := provider.ResolveRole(f, provider.ProfileFor(provider.KindIMAP), map[string]string{"receipts": "Receipts"})
	if role != provider.RoleNone || source != "" {
		t.Fatalf("got %q from %q, want no role", role, source)
	}
}

func TestTwoOverridesNamingOneFolderResolveTheSameWayEveryTime(t *testing.T) {
	overrides := map[string]string{"sent": "Mixed", "drafts": "Mixed", "trash": "Mixed"}
	first, _ := provider.ResolveRole(provider.Folder{Name: "Mixed"}, provider.ProfileFor(provider.KindIMAP), overrides)
	for range 50 {
		got, _ := provider.ResolveRole(provider.Folder{Name: "Mixed"}, provider.ProfileFor(provider.KindIMAP), overrides)
		if got != first {
			t.Fatalf("resolved to %q after %q: map order leaked into the answer", got, first)
		}
	}
}

func TestAGmailServerUnderAnotherKindKeepsGmailsNeverSyncedFolders(t *testing.T) {
	// The account says generic IMAP; the server says it is Gmail, by its
	// capability or its host. All Mail, Starred and Important stay out.
	generic := provider.ProfileFor(provider.KindIMAP)
	allMail := provider.Folder{Name: "[Gmail]/All Mail", Selectable: true, Attrs: []imap.MailboxAttr{imap.MailboxAttrAll}}
	starred := provider.Folder{Name: "[Gmail]/Starred", Selectable: true}
	for _, tc := range []struct {
		name string
		caps provider.Caps
		host string
	}{
		{"capability", provider.Caps{Raw: []string{"IMAP4rev1", "x-gm-ext-1"}}, "mail.example.net"},
		{"host", provider.Caps{}, "imap.googlemail.com"},
	} {
		p := generic.ForServer(tc.caps, tc.host)
		if p.SyncsFolder(allMail, provider.RoleAll) {
			t.Errorf("%s: All Mail is synced", tc.name)
		}
		role, _ := provider.ResolveRole(starred, p, nil)
		if role != provider.RoleFlagged || p.SyncsFolder(starred, role) {
			t.Errorf("%s: Starred without an attribute resolves to %q and is synced=%t", tc.name, role, p.SyncsFolder(starred, role))
		}
		if p.SaveSentDefault != generic.SaveSentDefault || p.Kind != provider.KindIMAP {
			t.Errorf("%s: the rest of the profile changed", tc.name)
		}
		// Archiving leaves the inbox for All Mail, and marking one label's
		// copy read marks the message read, whatever the account is called.
		if p.ArchiveRole != provider.RoleAll || !p.FlagsSharedAcrossFolders {
			t.Errorf("%s: archive role %q, shared flags %t", tc.name, p.ArchiveRole, p.FlagsSharedAcrossFolders)
		}
	}
	p := generic.ForServer(provider.Caps{Raw: []string{"IMAP4rev1"}}, "mail.example.net")
	if !p.SyncsFolder(allMail, provider.RoleAll) {
		t.Error("a server that is not Gmail lost its All Mail")
	}
	if p.ArchiveRole != provider.RoleArchive || p.FlagsSharedAcrossFolders {
		t.Errorf("a server that is not Gmail archives to %q with shared flags %t", p.ArchiveRole, p.FlagsSharedAcrossFolders)
	}
}
