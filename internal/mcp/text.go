package mcp

import (
	"fmt"
	"strings"
	"time"

	"github.com/thehappieco/mailie/internal/service"
)

// Every tool answers twice: structuredContent, for clients that read it, and
// this text, for the many that hand a model only a result's content. The text
// is compact, and complete enough to act on: a message's text is in it, since
// a model that sees only content would otherwise read nothing.

func accountsText(accounts []service.Account) string {
	if len(accounts) == 0 {
		return "This key reaches no mailbox."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", plural(len(accounts), "mailbox", "mailboxes"))
	for _, a := range accounts {
		sync := "sync off (not searchable)"
		if a.Sync.Enabled {
			sync = "sync on"
		}
		fmt.Fprintf(&b, "- %s: %s (%s), %s, %s", a.ID, a.Email, a.Provider, a.State, sync)
		// A team mailbox an owner or an admin manages by their role is
		// listed without read: what a model would otherwise try on it is
		// refused, one tool call at a time.
		if !a.Access.Read {
			b.WriteString(", no read access (its messages cannot be searched or read)")
		}
		var actions []string
		if a.Access.Act && a.Actions.Archive {
			actions = append(actions, "archive")
		}
		if a.Access.Act && a.Actions.Trash {
			actions = append(actions, "trash")
		}
		if len(actions) > 0 {
			fmt.Fprintf(&b, ", can %s", strings.Join(actions, " and "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func foldersText(folders []service.Folder) string {
	if len(folders) == 0 {
		return "No folders."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", plural(len(folders), "folder", "folders"))
	for _, f := range folders {
		b.WriteString("- ")
		if f.ID != 0 {
			fmt.Fprintf(&b, "%d: ", f.ID)
		}
		b.WriteString(f.DisplayName)
		if f.Role != "" {
			fmt.Fprintf(&b, " [%s]", f.Role)
		}
		if f.Synced {
			fmt.Fprintf(&b, ", %d messages, %d unread", f.Messages, f.Unseen)
		} else {
			b.WriteString(", not synced")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func pageText(page service.MessagePage) string {
	if len(page.Messages) == 0 {
		return "No messages."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s, newest first:\n", plural(len(page.Messages), "message", "messages"))
	for _, m := range page.Messages {
		fmt.Fprintf(&b, "- %d · %s · %s · %s", m.ID, dateText(m.InternalDate), addressesText(m.From), subjectText(m.Subject))
		var marks []string
		if !m.Seen {
			marks = append(marks, "unread")
		}
		if m.Flagged {
			marks = append(marks, "starred")
		}
		if m.HasAttachments {
			marks = append(marks, "attachments")
		}
		if len(marks) > 0 {
			fmt.Fprintf(&b, " [%s]", strings.Join(marks, ", "))
		}
		fmt.Fprintf(&b, " (%s, %s)\n", m.AccountID, m.FolderName)
	}
	if page.NextCursor != "" {
		fmt.Fprintf(&b, "More: pass cursor %q.\n", page.NextCursor)
	}
	return b.String()
}

func messageText(m service.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Message %d (%s, %s)\n", m.ID, m.AccountID, m.FolderName)
	fmt.Fprintf(&b, "From: %s\n", addressesText(m.From))
	fmt.Fprintf(&b, "To: %s\n", addressesText(m.To))
	if len(m.Cc) > 0 {
		fmt.Fprintf(&b, "Cc: %s\n", addressesText(m.Cc))
	}
	if len(m.ReplyTo) > 0 {
		fmt.Fprintf(&b, "Reply-To: %s\n", addressesText(m.ReplyTo))
	}
	date := m.Date
	if date == 0 {
		date = m.InternalDate
	}
	fmt.Fprintf(&b, "Date: %s\n", dateText(date))
	fmt.Fprintf(&b, "Subject: %s\n", subjectText(m.Subject))
	state := "read"
	if !m.Seen {
		state = "unread"
	}
	if m.Flagged {
		state += ", starred"
	}
	fmt.Fprintf(&b, "State: %s\n", state)
	var attachments []service.MessagePart
	for _, p := range m.Parts {
		if p.IsAttachment {
			attachments = append(attachments, p)
		}
	}
	if len(attachments) > 0 {
		b.WriteString("Attachments (get_attachment takes the part):\n")
		for _, p := range attachments {
			name := p.Filename
			if name == "" {
				name = "unnamed"
			}
			fmt.Fprintf(&b, "- part %s: %s (%s, %s)\n", p.Path, name, p.MIMEType, sizeText(p.Size))
		}
	}
	switch {
	case m.Body.Text != nil:
		b.WriteString("\n")
		b.WriteString(*m.Body.Text)
		b.WriteString("\n")
		if m.Body.HTML != nil {
			b.WriteString("\n--- HTML ---\n")
			b.WriteString(*m.Body.HTML)
			b.WriteString("\n")
		}
	case m.Body.HTML != nil:
		b.WriteString("\n(HTML, as the sender wrote it)\n")
		b.WriteString(*m.Body.HTML)
		b.WriteString("\n")
	default:
		b.WriteString("\n(no body in the format asked for)\n")
	}
	if m.Body.Truncated {
		b.WriteString("\n[truncated: ask again with a larger max_bytes to read more]\n")
	}
	return b.String()
}

func waitText(r waitResult) string {
	var b strings.Builder
	if len(r.Messages) == 0 {
		b.WriteString("No new mail arrived.")
	} else {
		fmt.Fprintf(&b, "%s arrived:\n", plural(len(r.Messages), "new message", "new messages"))
		for _, m := range r.Messages {
			from := "(no sender)"
			if m.From != nil {
				from = addressText(*m.From)
			}
			fmt.Fprintf(&b, "- %d · %s · %s · %s (%s)\n", m.ID, dateText(m.InternalDate), from, subjectText(m.Subject), m.AccountID)
		}
	}
	if r.Lagged {
		b.WriteString(" The cursor was older than the server keeps: some mail may be missing here; search for it.")
	}
	fmt.Fprintf(&b, "\nNext since_cursor: %d", r.NextCursor)
	return b.String()
}

// actionText says what an action did. verb has one %s, for how many.
func actionText(verb string, r service.ActionResult) string {
	n := len(r.Messages) + len(r.Removed)
	s := fmt.Sprintf(verb, plural(n, "message", "messages"))
	if len(r.Removed) > 0 {
		s += fmt.Sprintf(" These left the index (their new place is not synced): %v.", r.Removed)
	}
	return s
}

func addressesText(list []service.Address) string {
	if len(list) == 0 {
		return "(none)"
	}
	out := make([]string, 0, len(list))
	for _, a := range list {
		out = append(out, addressText(a))
	}
	return strings.Join(out, ", ")
}

func addressText(a service.Address) string {
	switch {
	case a.Name == "":
		return a.Email
	case a.Email == "":
		return a.Name
	}
	return a.Name + " <" + a.Email + ">"
}

func subjectText(s string) string {
	if s == "" {
		return "(no subject)"
	}
	return s
}

func dateText(unix int64) string {
	if unix == 0 {
		return "(no date)"
	}
	return time.Unix(unix, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func sizeText(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}
