package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/thehappieco/mailie/internal/config"
	"github.com/thehappieco/mailie/internal/service"
)

// Workspaces, their members, and who holds what on their mailboxes, for the
// operator (docs/workspaces.md, "Command line"). Every command is a client of
// the daemon with MAIL_ADMIN_KEY, an unrestricted instance admin key, and the
// daemon decides what the operator may do: it administers every team, and
// holds no flag on anyone's mailbox, so it grants manage and nothing else —
// read, act and send pass from a member who holds them, in the console.
//
// People are named by address, as everywhere on the command line; the routes
// take ids, which these look up in the workspace's member list.

func workspaceCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: workspace needs a subcommand: list, create or rename", errUsage)
	}
	switch args[0] {
	case "list":
		return workspaceList(ctx, cfg, args[1:])
	case "create":
		return workspaceCreate(ctx, cfg, args[1:])
	case "rename":
		return workspaceRename(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown workspace subcommand %q", errUsage, args[0])
	}
}

func memberCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: member needs a subcommand: list, role or remove", errUsage)
	}
	switch args[0] {
	case "list":
		return memberList(ctx, cfg, args[1:])
	case "role":
		return memberRole(ctx, cfg, args[1:])
	case "remove":
		return memberRemove(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown member subcommand %q", errUsage, args[0])
	}
}

func accessCommand(ctx context.Context, cfg config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: access needs a subcommand: list, grant or revoke", errUsage)
	}
	switch args[0] {
	case "list":
		return accessList(ctx, cfg, args[1:])
	case "grant":
		return accessGrant(ctx, cfg, args[1:])
	case "revoke":
		return accessRevoke(ctx, cfg, args[1:])
	default:
		return fmt.Errorf("%w: unknown access subcommand %q", errUsage, args[0])
	}
}

func workspaceList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("workspace list", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("workspace list: unexpected argument %q", fs.Arg(0))
	}
	var workspaces []service.Workspace
	if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces", nil, &workspaces); err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "ID\tKIND\tNAME\tMEMBERS\tMAILBOXES\tSOURCE")
	for _, ws := range workspaces {
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n", ws.ID, ws.Kind, orDash(ws.Name),
			count(ws.Members), count(ws.Mailboxes), ws.Source)
	}
	return w.Flush()
}

func count(n *int) string {
	if n == nil {
		return "-"
	}
	return fmt.Sprint(*n)
}

func workspaceCreate(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("workspace create", flag.ContinueOnError)
	name := fs.String("name", "", "the team's name, 1 to 80 characters")
	owner := fs.String("owner", "", "the address of the existing person who will own the team"+emailFlagUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("workspace create: unexpected argument %q", fs.Arg(0))
	}
	email, err := emailArg("workspace create", *owner)
	if err != nil {
		return fmt.Errorf("%w (--owner)", err)
	}
	var created service.Workspace
	if err := adminJSON(ctx, cfg, http.MethodPost, "/v1/workspaces",
		map[string]any{"name": *name, "owner_email": email}, &created); err != nil {
		return err
	}
	fmt.Printf("created the team %s (%s), owned by %s\n", created.ID, created.Name, email)
	return nil
}

func workspaceRename(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("workspace rename", flag.ContinueOnError)
	id := fs.String("workspace", "", "the team's id")
	name := fs.String("name", "", "the new name, 1 to 80 characters")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("workspace rename: --workspace is required")
	}
	var renamed service.Workspace
	if err := adminJSON(ctx, cfg, http.MethodPatch, "/v1/workspaces/"+url.PathEscape(*id),
		map[string]any{"name": *name}, &renamed); err != nil {
		return err
	}
	fmt.Printf("renamed %s to %s\n", renamed.ID, renamed.Name)
	return nil
}

func memberList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("member list", flag.ContinueOnError)
	id := fs.String("workspace", "", "the workspace's id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("member list: --workspace is required")
	}
	members, err := membersOf(ctx, cfg, *id)
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "USER\tEMAIL\tROLE\tSTATUS\tNOTE")
	for _, m := range members {
		var notes []string
		if m.LastOwner {
			notes = append(notes, "last owner")
		}
		if m.Links > 0 {
			notes = append(notes, "linked "+plural(m.Links, "mailbox"))
		}
		if m.PersonDisabled {
			notes = append(notes, "disabled on this server")
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", m.UserID, m.Email, m.Role, m.Status, orDash(strings.Join(notes, "; ")))
	}
	return w.Flush()
}

func memberRole(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("member role", flag.ContinueOnError)
	id := fs.String("workspace", "", "the team's id")
	email := fs.String("email", "", "the member's address"+emailFlagUsage)
	role := fs.String("role", "", "owner, admin or member")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" || *role == "" {
		return errors.New("member role: --workspace and --role are required")
	}
	m, err := memberByEmail(ctx, cfg, "member role", *id, *email)
	if err != nil {
		return err
	}
	var changed service.Member
	if err := adminJSON(ctx, cfg, http.MethodPatch,
		"/v1/workspaces/"+url.PathEscape(*id)+"/members/"+url.PathEscape(m.UserID),
		map[string]any{"role": *role}, &changed); err != nil {
		return err
	}
	fmt.Printf("%s is now %s of %s\n", changed.Email, changed.Role, *id)
	return nil
}

func memberRemove(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("member remove", flag.ContinueOnError)
	id := fs.String("workspace", "", "the team's id")
	email := fs.String("email", "", "the member's address"+emailFlagUsage)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("member remove: --workspace is required")
	}
	m, err := memberByEmail(ctx, cfg, "member remove", *id, *email)
	if err != nil {
		return err
	}
	if err := adminJSON(ctx, cfg, http.MethodDelete,
		"/v1/workspaces/"+url.PathEscape(*id)+"/members/"+url.PathEscape(m.UserID), nil, nil); err != nil {
		return err
	}
	fmt.Printf("removed %s from %s, with their access to its mailboxes\n", m.Email, *id)
	return nil
}

func accessList(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("access list", flag.ContinueOnError)
	id := fs.String("workspace", "", "the workspace's id")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *id == "" {
		return errors.New("access list: --workspace is required")
	}
	var directory []service.MailboxAccess
	if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces/"+url.PathEscape(*id)+"/access", nil, &directory); err != nil {
		return err
	}
	if len(directory) == 0 {
		fmt.Fprintln(os.Stderr, "no mailboxes in this workspace")
		return nil
	}
	members, err := membersOf(ctx, cfg, *id)
	if err != nil {
		return err
	}
	who := map[string]string{}
	for _, m := range members {
		who[m.UserID] = m.Email
	}
	name := func(userID string) string {
		if email, ok := who[userID]; ok {
			return email
		}
		return orDash(userID)
	}
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	//nolint:errcheck // checked by Flush
	fmt.Fprintln(w, "ACCOUNT\tEMAIL\tSTATE\tLINKED BY\tGRANTS")
	for _, mb := range directory {
		grants := make([]string, 0, len(mb.Grants))
		for _, g := range mb.Grants {
			grants = append(grants, name(g.UserID)+" "+flagList(g))
		}
		linkedBy := "-"
		if mb.LinkedBy != "" {
			linkedBy = name(mb.LinkedBy)
		}
		//nolint:errcheck // checked by Flush
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", mb.AccountID, mb.Email, mb.State, linkedBy,
			orDash(strings.Join(grants, ", ")))
	}
	return w.Flush()
}

// flagList is a grant's flags as the command line names them.
func flagList(g service.Grant) string {
	var flags []string
	for _, f := range []struct {
		on   bool
		name string
	}{{g.Read, "read"}, {g.Act, "act"}, {g.Send, "send"}, {g.Manage, "manage"}} {
		if f.on {
			flags = append(flags, f.name)
		}
	}
	return "(" + strings.Join(flags, ",") + ")"
}

func accessGrant(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("access grant", flag.ContinueOnError)
	accountID := fs.String("account", "", "the mailbox's account id")
	email := fs.String("email", "", "the member's address"+emailFlagUsage)
	manage := fs.Bool("manage", false, "grant manage: re-authorizing and removing the mailbox, and who has access to it "+
		"(required: the operator grants nothing else)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == "" {
		return errors.New("access grant: --account is required")
	}
	if !*manage {
		return errors.New("access grant: the operator grants manage only, so --manage is required; " +
			"read, act and send pass from a member who holds them, in the console")
	}
	mb, workspaceID, err := mailboxAccess(ctx, cfg, *accountID)
	if err != nil {
		return err
	}
	m, err := memberByEmail(ctx, cfg, "access grant", workspaceID, *email)
	if err != nil {
		return err
	}
	// The grant is set to exactly what the request names: what the person
	// holds already stays, and manage joins it.
	var held service.Grant
	for _, g := range mb.Grants {
		if g.UserID == m.UserID {
			held = g
		}
	}
	var set service.Grant
	if err := adminJSON(ctx, cfg, http.MethodPut,
		"/v1/accounts/"+url.PathEscape(mb.AccountID)+"/access/"+url.PathEscape(m.UserID),
		map[string]any{"read": held.Read, "act": held.Act, "send": held.Send, "manage": true}, &set); err != nil {
		return err
	}
	fmt.Printf("%s now holds %s on %s (%s)\n", m.Email, flagList(set), mb.Email, mb.AccountID)
	return nil
}

func accessRevoke(ctx context.Context, cfg config.Config, args []string) error {
	fs := flag.NewFlagSet("access revoke", flag.ContinueOnError)
	accountID := fs.String("account", "", "the mailbox's account id")
	email := fs.String("email", "", "the member's address"+emailFlagUsage)
	var drop []string
	for _, name := range []string{"read", "act", "send", "manage"} {
		fs.Var(boolFunc{name: name, add: func(n string) { drop = append(drop, n) }}, name,
			"take "+name+" away (with none of the four, every flag goes)")
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *accountID == "" {
		return errors.New("access revoke: --account is required")
	}
	mb, workspaceID, err := mailboxAccess(ctx, cfg, *accountID)
	if err != nil {
		return err
	}
	m, err := memberByEmail(ctx, cfg, "access revoke", workspaceID, *email)
	if err != nil {
		return err
	}
	path := "/v1/accounts/" + url.PathEscape(mb.AccountID) + "/access/" + url.PathEscape(m.UserID)
	if len(drop) > 0 {
		path += "?flags=" + url.QueryEscape(strings.Join(drop, ","))
	}
	if err := adminJSON(ctx, cfg, http.MethodDelete, path, nil, nil); err != nil {
		return err
	}
	what := "every flag"
	if len(drop) > 0 {
		what = strings.Join(drop, ", ")
	}
	fmt.Printf("took %s away from %s on %s (%s)\n", what, m.Email, mb.Email, mb.AccountID)
	return nil
}

// boolFunc is a flag that takes no value and calls add with its name when
// given.
type boolFunc struct {
	name string
	add  func(string)
}

func (b boolFunc) String() string   { return "" }
func (b boolFunc) IsBoolFlag() bool { return true }
func (b boolFunc) Set(v string) error {
	if v != "true" && v != "" {
		return fmt.Errorf("--%s takes no value", b.name)
	}
	b.add(b.name)
	return nil
}

// membersOf lists a workspace's members.
func membersOf(ctx context.Context, cfg config.Config, workspaceID string) ([]service.Member, error) {
	var members []service.Member
	err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces/"+url.PathEscape(workspaceID)+"/members", nil, &members)
	return members, err
}

// memberByEmail finds a member of a workspace by the address they sign in
// with.
func memberByEmail(ctx context.Context, cfg config.Config, command, workspaceID, flagValue string) (service.Member, error) {
	email, err := emailArg(command, flagValue)
	if err != nil {
		return service.Member{}, err
	}
	members, err := membersOf(ctx, cfg, workspaceID)
	if err != nil {
		return service.Member{}, err
	}
	for _, m := range members {
		if strings.EqualFold(m.Email, strings.TrimSpace(email)) {
			return m, nil
		}
	}
	return service.Member{}, fmt.Errorf("%s: nobody with that address is a member of %s", command, workspaceID)
}

// mailboxAccess finds a mailbox in the access directory of the workspace
// that holds it: the operator sees every workspace, and no mailbox outside
// its own through the account routes.
func mailboxAccess(ctx context.Context, cfg config.Config, accountID string) (service.MailboxAccess, string, error) {
	var workspaces []service.Workspace
	if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces", nil, &workspaces); err != nil {
		return service.MailboxAccess{}, "", err
	}
	for _, ws := range workspaces {
		if ws.Mailboxes != nil && *ws.Mailboxes == 0 {
			continue
		}
		var directory []service.MailboxAccess
		if err := adminJSON(ctx, cfg, http.MethodGet, "/v1/workspaces/"+url.PathEscape(ws.ID)+"/access", nil,
			&directory); err != nil {
			return service.MailboxAccess{}, "", err
		}
		for _, mb := range directory {
			if mb.AccountID == accountID {
				return mb, ws.ID, nil
			}
		}
	}
	return service.MailboxAccess{}, "", fmt.Errorf("no mailbox has the account id %s", accountID)
}

// adminJSON sends a request to the daemon and decodes its answer into out,
// when out is not nil.
func adminJSON(ctx context.Context, cfg config.Config, method, path string, body, out any) error {
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			return err
		}
	}
	payload, err := adminDo(ctx, cfg, method, path, encoded)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("%s: unexpected response: %w", path, err)
	}
	return nil
}
