package mcp_test

import (
	"context"
	"encoding/json"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap/v2"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thehappieco/mailie/internal/auth"
	"github.com/thehappieco/mailie/internal/auth/authtest"
	"github.com/thehappieco/mailie/internal/mcp"
	"github.com/thehappieco/mailie/internal/provider/providertest"
	"github.com/thehappieco/mailie/internal/service"
	"github.com/thehappieco/mailie/internal/workspace"
)

func TestToolsListCarriesTheAnnotations(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		cs := h.connect(h.key(ana, "read"), protocol, nil)
		list, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		tools := map[string]*sdk.Tool{}
		for _, tool := range list.Tools {
			tools[tool.Name] = tool
		}
		want := []string{"list_accounts", "list_folders", "search_messages", "get_message", "get_attachment",
			"wait_for_new_mail", "mark_read", "flag_message", "move_message", "trash_message"}
		var names []string
		for name := range tools {
			names = append(names, name)
		}
		slices.Sort(names)
		sorted := slices.Clone(want)
		slices.Sort(sorted)
		if !slices.Equal(names, sorted) {
			// Sending comes with its own phase, confirmation and all.
			t.Fatalf("tools = %v, want exactly %v", names, sorted)
		}
		for _, name := range want[:6] {
			a := tools[name].Annotations
			// Open world is reaching the mail server: reading a message or an
			// attachment, and listing the folders of a mailbox not synced yet.
			openWorld := name == "get_message" || name == "get_attachment" || name == "list_folders"
			if a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || !a.IdempotentHint ||
				a.OpenWorldHint == nil || *a.OpenWorldHint != openWorld {
				t.Errorf("%s: annotations %+v; want read-only, not destructive, idempotent, open world %t",
					name, a, openWorld)
			}
			if tools[name].Meta[requiresUserInteraction] != nil {
				t.Errorf("%s asks for the person's confirmation; reading needs none", name)
			}
		}
		for _, name := range []string{"mark_read", "flag_message", "move_message"} {
			a := tools[name].Annotations
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || !a.IdempotentHint ||
				a.OpenWorldHint == nil || !*a.OpenWorldHint {
				t.Errorf("%s: annotations %+v; want a write that is not destructive, idempotent, open world", name, a)
			}
		}
		// The limits a model reads from the schemas.
		for _, c := range []struct{ tool, path, want string }{
			{"get_message", "format.enum", `["text","html","both"]`},
			{"get_message", "format.default", `"text"`},
			{"search_messages", "limit.maximum", `100`},
			{"get_attachment", "max_bytes.maximum", `1048576`},
			{"wait_for_new_mail", "timeout_seconds.maximum", `230`},
			{"wait_for_new_mail", "timeout_seconds.default", `45`},
			{"mark_read", "ids.maxItems", `100`},
			{"mark_read", "ids.minItems", `1`},
			{"list_folders", "account.pattern", `"^acc_[0-9a-f]{16}$"`},
			{"search_messages", "account.maxLength", `20`},
			{"wait_for_new_mail", "account.pattern", `"^acc_[0-9a-f]{16}$"`},
		} {
			if got := schemaAt(t, tools[c.tool].InputSchema, c.path); got != c.want {
				t.Errorf("%s input %s = %s, want %s", c.tool, c.path, got, c.want)
			}
		}
		if tools["get_message"].OutputSchema == nil {
			t.Error("get_message has no output schema; structured results should be described")
		}
	})
}

func TestTrashRequiresUserInteractionAndIsDestructive(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		cs := h.connect(h.key(ana, "write"), protocol, nil)
		list, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range list.Tools {
			if tool.Name != "trash_message" {
				continue
			}
			a := tool.Annotations
			if a == nil || a.ReadOnlyHint || a.DestructiveHint == nil || !*a.DestructiveHint {
				t.Errorf("trash_message annotations %+v; want destructive, so claude.ai and Desktop confirm it", a)
			}
			if tool.Meta[requiresUserInteraction] != true {
				t.Errorf("trash_message _meta = %v; Claude Code confirms only with %s", tool.Meta, requiresUserInteraction)
			}
			return
		}
		t.Fatal("no trash_message tool")
	})
}

const requiresUserInteraction = "anthropic/requiresUserInteraction"

// schemaAt reads a property's keyword out of a tool's input schema, as the
// client received it, as JSON.
func schemaAt(t *testing.T, schema any, path string) string {
	t.Helper()
	raw, err := json.Marshal(schema)
	if err != nil {
		t.Fatal(err)
	}
	var s struct {
		Properties map[string]map[string]json.RawMessage `json:"properties"`
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}
	prop, keyword, _ := strings.Cut(path, ".")
	return string(s.Properties[prop][keyword])
}

func TestReadToolsNeverMarkAMessageRead(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		const acc = "acc_00000000000000a1"
		box := h.mailbox(acc, ana.user.ID, "ana@work.example")
		id := h.deliver(acc, box, letter("Lunch", "Bea <bea@example.org>", "See you at noon.", "menu.pdf", "%PDF-1.4 menu"))
		cs := h.connect(h.key(ana, "read"), protocol, nil)

		page := ok[service.MessagePage](t, cs, "search_messages", map[string]any{"q": "lunch"})
		if len(page.Messages) != 1 || page.Messages[0].ID != id {
			t.Fatalf("search found %+v, want message %d", page.Messages, id)
		}
		m := ok[service.Message](t, cs, "get_message", map[string]any{"id": id, "format": "both"})
		if m.Body.Text == nil || !strings.Contains(*m.Body.Text, "See you at noon.") || m.Body.HTML == nil {
			t.Fatalf("get_message body = %+v", m.Body)
		}
		part := ""
		for _, p := range m.Parts {
			if p.IsAttachment {
				part = p.Path
			}
		}
		res := call(t, cs, "get_attachment", map[string]any{"id": id, "part": part})
		if res.IsError {
			t.Fatalf("get_attachment: %s", text(res))
		}
		var message service.Message
		if err := readResource(t, cs, "mail://"+acc+"/message/"+itoa(id), &message); err != nil {
			t.Fatalf("reading the message resource: %v", err)
		}

		if flags := box.Flags("INBOX", 1); hasFlag(flags, goimap.FlagSeen) {
			t.Errorf("reading through MCP marked the message read on the server: %v", flags)
		}
		if n := box.CallCount(providertest.MethodStoreFlags); n != 0 {
			t.Errorf("reading sent %d STORE commands", n)
		}
		for _, c := range box.Calls() {
			if c.Method == providertest.MethodSelect && !c.ReadOnly {
				t.Errorf("reading opened %s for writing (SELECT rather than EXAMINE)", c.Folder)
			}
		}
		if again := ok[service.MessagePage](t, cs, "search_messages", nil); again.Messages[0].Seen {
			t.Error("the index says the message is read after it was only read through MCP")
		}
	})
}

func TestAReadKeyCannotMarkMessagesReadThroughMCP(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		h.allowActions(ana)
		const acc = "acc_00000000000000a1"
		box := h.mailbox(acc, ana.user.ID, "ana@work.example")
		id := h.deliver(acc, box, letter("Invoice", "Shop <orders@shop.example>", "Total: 10", "invoice.pdf", "%PDF"))

		reader := h.connect(h.key(ana, "read"), protocol, nil)
		for _, c := range []struct {
			tool string
			args map[string]any
		}{
			{"mark_read", map[string]any{"ids": []int64{id}}},
			{"flag_message", map[string]any{"ids": []int64{id}}},
			{"move_message", map[string]any{"ids": []int64{id}, "to": "archive"}},
			{"trash_message", map[string]any{"ids": []int64{id}}},
		} {
			refused(t, reader, c.tool, c.args, service.CodeNotAuthorized)
		}
		for _, method := range []providertest.Method{providertest.MethodStoreFlags, providertest.MethodMove,
			providertest.MethodCopy} {
			if n := box.CallCount(method); n != 0 {
				t.Errorf("a read key's refused action still sent %d %s commands", n, method)
			}
		}

		// The same action with a write key goes through: what stopped the
		// read key was its scope.
		writer := h.connect(h.key(ana, "write"), protocol, nil)
		res := ok[service.ActionResult](t, writer, "mark_read", map[string]any{"ids": []int64{id}})
		if len(res.Messages) != 1 || !res.Messages[0].Seen {
			t.Fatalf("mark_read with a write key answered %+v", res)
		}
		if flags := box.Flags("INBOX", 1); !hasFlag(flags, goimap.FlagSeen) {
			t.Errorf("the server's flags are %v after mark_read", flags)
		}
	})
}

func TestActionsThroughMCPNeedTheOwnersConsent(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	const acc = "acc_00000000000000a1"
	box := h.mailbox(acc, ana.user.ID, "ana@work.example")
	id := h.deliver(acc, box, letter("Invoice", "Shop <orders@shop.example>", "Total: 10", "invoice.pdf", "%PDF"))
	cs := h.connect(h.key(ana, "write"), "", nil)
	refused(t, cs, "trash_message", map[string]any{"ids": []int64{id}}, service.CodeConflict)
	if n := box.CallCount(providertest.MethodMove); n != 0 {
		t.Errorf("an action without consent sent %d MOVE commands", n)
	}
	h.allowActions(ana)
	res := ok[service.ActionResult](t, cs, "trash_message", map[string]any{"ids": []int64{id}})
	if len(res.Messages) != 1 || res.Messages[0].FolderRole != "trash" {
		t.Fatalf("trash_message answered %+v", res)
	}
}

func TestWaitForNewMailReturnsTheMessageThatArrivesDuringTheWait(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		const acc = "acc_00000000000000a1"
		box := h.mailbox(acc, ana.user.ID, "ana@work.example")
		cs := h.connect(h.key(ana, "read"), protocol, nil)

		type answer struct {
			res  *sdk.CallToolResult
			took time.Duration
		}
		done := make(chan answer, 1)
		go func() {
			started := time.Now()
			res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "wait_for_new_mail",
				Arguments: map[string]any{"timeout_seconds": 30}})
			if err != nil {
				t.Error(err)
			}
			done <- answer{res, time.Since(started)}
		}()
		time.Sleep(300 * time.Millisecond)
		id := h.deliver(acc, box, letter("Hello", "Bea <bea@example.org>", "Hi!", "a.pdf", "%PDF"))
		h.announce(acc, id, "Hello")

		got := <-done
		if got.res == nil || got.res.IsError {
			t.Fatalf("wait_for_new_mail failed: %v", got.res)
		}
		if got.took > 10*time.Second {
			t.Errorf("the answer took %v; it should come as the mail does", got.took)
		}
		var first struct {
			TimedOut   bool              `json:"timed_out"`
			Messages   []service.NewMail `json:"messages"`
			NextCursor int64             `json:"next_cursor"`
		}
		raw, _ := json.Marshal(got.res.StructuredContent)
		if err := json.Unmarshal(raw, &first); err != nil {
			t.Fatal(err)
		}
		if first.TimedOut || len(first.Messages) != 1 || first.Messages[0].ID != id || first.Messages[0].AccountID != acc {
			t.Fatalf("wait answered %s, want message %d", raw, id)
		}
		if !strings.Contains(text(got.res), "Hello") {
			t.Errorf("the text answer does not say what arrived: %q", text(got.res))
		}
		// The message is readable by the id the wait gave.
		if m := ok[service.Message](t, cs, "get_message", map[string]any{"id": id}); m.Subject != "Hello" {
			t.Errorf("get_message by the waited id read %q", m.Subject)
		}
		// Resuming from the cursor brings nothing twice.
		second := ok[struct {
			TimedOut bool              `json:"timed_out"`
			Messages []service.NewMail `json:"messages"`
		}](t, cs, "wait_for_new_mail", map[string]any{"since_cursor": first.NextCursor, "timeout_seconds": 1})
		if !second.TimedOut || len(second.Messages) != 0 {
			t.Errorf("waiting again from the cursor answered %+v", second)
		}
	})
}

func TestAWaitTellsAClientThatAskedForProgressThatItIsStillWaiting(t *testing.T) {
	h := newHarness(t)
	mcp.SetWaitStep(h.mcp, time.Second)
	ana := h.person("ana@example.com", auth.RoleMember)
	var mu sync.Mutex
	var progress []float64
	cs := h.connect(h.key(ana, "read"), "2025-11-25", &sdk.ClientOptions{
		ProgressNotificationHandler: func(_ context.Context, req *sdk.ProgressNotificationClientRequest) {
			mu.Lock()
			progress = append(progress, req.Params.Progress)
			mu.Unlock()
		},
	})
	params := &sdk.CallToolParams{Name: "wait_for_new_mail", Arguments: map[string]any{"timeout_seconds": 4}}
	params.SetProgressToken("wait-1")
	res, err := cs.CallTool(t.Context(), params)
	if err != nil || res.IsError {
		t.Fatalf("wait: %v %v", err, res)
	}
	if !strings.Contains(text(res), "No new mail") {
		t.Errorf("a wait that timed out says %q", text(res))
	}
	seen := func() []float64 {
		mu.Lock()
		defer mu.Unlock()
		return slices.Clone(progress)
	}
	if got := seen(); len(got) < 2 || !slices.IsSorted(got) {
		t.Errorf("progress notifications %v; want one per step, increasing", got)
	}

	// Without a token the client asked for nothing, and gets nothing.
	before := len(seen())
	ok[map[string]any](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 2})
	if got := seen(); len(got) != before {
		t.Errorf("a wait without a progress token sent %v", got[before:])
	}
}

func TestAKeyRestrictedToOneMailboxSeesNothingElseThroughToolsResourcesOrEvents(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		bea := h.person("bea@example.com", auth.RoleMember)
		const work, home, beas = "acc_00000000000000a1", "acc_00000000000000a2", "acc_00000000000000b1"
		workBox := h.mailbox(work, ana.user.ID, "ana@work.example")
		homeBox := h.mailbox(home, ana.user.ID, "ana@home.example")
		beasBox := h.mailbox(beas, bea.user.ID, "bea@example.com")
		workID := h.deliver(work, workBox, letter("Work report", "Boss <boss@work.example>", "Q3", "q3.pdf", "%PDF"))
		homeID := h.deliver(home, homeBox, letter("Family dinner", "Mom <mom@home.example>", "Sunday", "map.pdf", "%PDF"))
		beasID := h.deliver(beas, beasBox, letter("Bea's secret", "Carl <carl@example.org>", "x", "c.pdf", "%PDF"))

		var mu sync.Mutex
		var updated []string
		key := h.key(ana, "read", work)
		cs := h.connect(key, protocol, &sdk.ClientOptions{
			ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
				mu.Lock()
				updated = append(updated, req.Params.URI)
				mu.Unlock()
			},
		})

		// Tools.
		accounts := ok[struct {
			Accounts []service.Account `json:"accounts"`
		}](t, cs, "list_accounts", nil)
		if len(accounts.Accounts) != 1 || accounts.Accounts[0].ID != work {
			t.Errorf("list_accounts = %+v, want only %s", accounts.Accounts, work)
		}
		page := ok[service.MessagePage](t, cs, "search_messages", nil)
		if len(page.Messages) != 1 || page.Messages[0].ID != workID {
			t.Errorf("search without an account found %+v, want only %d", page.Messages, workID)
		}
		for _, other := range []string{home, beas} {
			refused(t, cs, "list_folders", map[string]any{"account": other}, service.CodeNotFound)
			refused(t, cs, "search_messages", map[string]any{"account": other}, service.CodeNotFound)
			refused(t, cs, "wait_for_new_mail", map[string]any{"account": other, "timeout_seconds": 1},
				service.CodeNotFound)
		}
		for _, id := range []int64{homeID, beasID} {
			refused(t, cs, "get_message", map[string]any{"id": id}, service.CodeNotFound)
			refused(t, cs, "get_attachment", map[string]any{"id": id, "part": "2"}, service.CodeNotFound)
		}
		if n := homeBox.CallCount(providertest.MethodFetchPart) + beasBox.CallCount(providertest.MethodFetchPart); n != 0 {
			t.Errorf("refused reads still fetched %d parts from other mailboxes", n)
		}

		// Resources.
		var listed struct {
			Accounts []service.Account `json:"accounts"`
		}
		if err := readResource(t, cs, "mail://accounts", &listed); err != nil || len(listed.Accounts) != 1 {
			t.Errorf("mail://accounts = %+v (%v), want only %s", listed.Accounts, err, work)
		}
		for _, uri := range []string{
			"mail://" + home + "/message/" + itoa(homeID),
			"mail://" + beas + "/message/" + itoa(beasID),
			// An id of another mailbox under this one's name.
			"mail://" + work + "/message/" + itoa(homeID),
			"mail://" + home + "/folder/inbox",
			"mail://" + beas + "/folder/inbox",
		} {
			var v any
			if err := readResource(t, cs, uri, &v); err == nil {
				t.Errorf("read %s: %v; want not found", uri, v)
			}
		}
		// Under the newest protocol a subscription is a stream the client
		// opens and does not wait on, so the server's refusal reaches only
		// the log; under the older ones it is the answer.
		for _, other := range []string{home, beas} {
			err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + other + "/folder/inbox"})
			if protocol != "" && err == nil {
				t.Errorf("subscribed to %s's inbox", other)
			}
			h.waitForLog(`"msg":"mcp subscription refused"`, `"key":"`+prefixOf(key)+`"`, `"account":"`+other+`"`,
				`"outcome":"not_found"`)
		}

		// Events: mail arriving elsewhere is neither returned nor announced.
		if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + work + "/folder/inbox"}); err != nil {
			t.Fatalf("subscribing to the key's own inbox: %v", err)
		}
		h.waitForLog(`"msg":"mcp subscription"`, `"account":"`+work+`"`, `"outcome":"ok"`)
		waited := make(chan waitAnswer, 1)
		go func() {
			waited <- ok[waitAnswer](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 20})
		}()
		time.Sleep(300 * time.Millisecond)
		h.announce(home, homeID, "Family dinner")
		h.announce(beas, beasID, "Bea's secret")
		time.Sleep(300 * time.Millisecond)
		h.announce(work, workID, "Work report")
		got := <-waited
		if len(got.Messages) != 1 || got.Messages[0].ID != workID {
			t.Errorf("the wait answered %+v; only %d is this key's", got.Messages, workID)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			seen := slices.Clone(updated)
			mu.Unlock()
			if len(seen) > 0 || time.Now().After(deadline) {
				if !slices.Equal(seen, []string{"mail://" + work + "/folder/inbox"}) {
					t.Errorf("resource updates %v; want one, for %s's inbox", seen, work)
				}
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

type waitAnswer struct {
	TimedOut   bool              `json:"timed_out"`
	Lagged     bool              `json:"lagged"`
	Messages   []service.NewMail `json:"messages"`
	NextCursor int64             `json:"next_cursor"`
}

func TestAnInstanceKeyReachesOnlyOperatorMailboxes(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleOwner)
	const hers, operators = "acc_00000000000000a1", "acc_00000000000000c1"
	herBox := h.mailbox(hers, ana.user.ID, "ana@work.example")
	opBox := h.mailbox(operators, "", "ops@example.com")
	herID := h.deliver(hers, herBox, letter("Private", "Doctor <dr@clinic.example>", "Results", "r.pdf", "%PDF"))
	opID := h.deliver(operators, opBox, letter("Alerts", "Monitor <mon@example.com>", "Disk", "d.pdf", "%PDF"))

	instance := authtest.NewKey(t, h.store, auth.ScopeWrite, "")
	cs := h.connect(instance, "", nil)
	accounts := ok[struct {
		Accounts []service.Account `json:"accounts"`
	}](t, cs, "list_accounts", nil)
	if len(accounts.Accounts) != 1 || accounts.Accounts[0].ID != operators {
		t.Errorf("an instance key's tool sees %+v; a person's mailbox is reached only with their own key", accounts.Accounts)
	}
	refused(t, cs, "get_message", map[string]any{"id": herID}, service.CodeNotFound)
	ok[service.Message](t, cs, "get_message", map[string]any{"id": opID})
	if page := ok[service.MessagePage](t, cs, "search_messages", nil); len(page.Messages) != 1 {
		t.Errorf("search found %+v", page.Messages)
	}

	// Over REST the same key reaches the same: the operator workspace's
	// mailboxes, never a person's.
	p, err := h.svc.Authenticate(t.Context(), instance, nil)
	if err != nil {
		t.Fatal(err)
	}
	all, err := h.svc.ListAccounts(t.Context(), p, "")
	if err != nil || len(all) != 1 || all[0].ID != operators {
		t.Errorf("over REST the instance key lists %+v (%v), want only %s", all, err, operators)
	}
}

func TestAKeyAnAdministratorMadeForAPersonIsNotAToolCredential(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	minted := authtest.NewKey(t, h.store, auth.ScopeRead, ana.user.ID)
	_, err := h.svc.AuthenticateTool(t.Context(), minted, nil)
	if service.CodeOf(err) != service.CodeNotAuthorized {
		t.Fatalf("a key the person did not create authenticated a tool: %v", err)
	}
	if _, err := h.svc.AuthenticateTool(t.Context(), h.key(ana, "read"), nil); err != nil {
		t.Fatalf("the person's own key: %v", err)
	}
	if _, err := h.svc.AuthenticateTool(t.Context(), ana.token, nil); service.CodeOf(err) != service.CodeUnauthorized {
		t.Fatalf("a console session authenticated a tool: %v", err)
	}
}

func TestARevokedKeyLosesItsMCPSessionAtTheNextCall(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		key := h.key(ana, "read")
		cs := h.connect(key, protocol, nil)
		ok[map[string]any](t, cs, "list_accounts", nil)
		prefix, _, _ := strings.Cut(key, ".")
		if err := h.svc.RevokeMyAPIKey(t.Context(), ana.session, prefix); err != nil {
			t.Fatal(err)
		}
		refused(t, cs, "list_accounts", nil, service.CodeUnauthorized)
		var v any
		if err := readResource(t, cs, "mail://accounts", &v); err == nil {
			t.Error("a revoked key still reads resources")
		}
	})
}

func TestAnAttachmentTooLargeToEmbedIsNotFetched(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	const acc = "acc_00000000000000a1"
	box := h.mailbox(acc, ana.user.ID, "ana@work.example")
	content := strings.Repeat("0123456789", 300)
	id := h.deliver(acc, box, letter("Data", "Bea <bea@example.org>", "attached", "data.bin", content))
	cs := h.connect(h.key(ana, "read"), "", nil)

	small := ok[struct {
		TooLarge     bool   `json:"too_large"`
		Size         int64  `json:"size"`
		DownloadPath string `json:"download_path"`
	}](t, cs, "get_attachment", map[string]any{"id": id, "part": "2", "max_bytes": 100})
	if !small.TooLarge || small.Size < 3000 || small.DownloadPath != "/v1/messages/"+itoa(id)+"/attachments/2" {
		t.Errorf("a 3000-byte attachment with max_bytes 100 answered %+v", small)
	}
	if n := box.CallCount(providertest.MethodFetchPart); n != 0 {
		t.Errorf("an attachment too large to return was fetched %d times anyway", n)
	}

	res := call(t, cs, "get_attachment", map[string]any{"id": id, "part": "2"})
	if res.IsError {
		t.Fatal(text(res))
	}
	var blob []byte
	for _, c := range res.Content {
		if r, ok := c.(*sdk.EmbeddedResource); ok {
			blob = r.Resource.Blob
		}
	}
	if string(blob) != content {
		t.Errorf("embedded %d bytes, want the %d decoded ones", len(blob), len(content))
	}
}

func TestMCPNeverLogsSearchTextOrMessageContent(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	h.allowActions(ana)
	const acc = "acc_00000000000000a1"
	box := h.mailbox(acc, ana.user.ID, "ana@work.example")
	secrets := []string{"Zanzibarsubject", "Quetzalsender", "quetzal@", "Fjordbody", "Xylophonefile", "Mauveattachment",
		"Gondolaquery", "Nightingalefrom"}
	id := h.deliver(acc, box, letter("Zanzibarsubject", "Quetzalsender <quetzal@birds.example>",
		"Fjordbody text", "Xylophonefile.pdf", "Mauveattachment bytes"))
	key := h.key(ana, "write")
	cs := h.connect(key, "", nil)

	ok[service.MessagePage](t, cs, "search_messages", map[string]any{"q": "Gondolaquery", "from": "Nightingalefrom"})
	ok[service.MessagePage](t, cs, "search_messages", map[string]any{"q": "Zanzibarsubject"})
	refused(t, cs, "search_messages", map[string]any{"q": "Gondolaquery", "cursor": "Nightingalefrom"},
		service.CodeBadRequest)
	ok[service.Message](t, cs, "get_message", map[string]any{"id": id, "format": "both"})
	ok[map[string]any](t, cs, "get_attachment", map[string]any{"id": id, "part": "2"})
	var v any
	if err := readResource(t, cs, "mail://"+acc+"/message/"+itoa(id), &v); err != nil {
		t.Fatal(err)
	}
	if err := readResource(t, cs, "mail://"+acc+"/folder/inbox", &v); err != nil {
		t.Fatal(err)
	}
	waited := make(chan struct{})
	go func() {
		defer close(waited)
		ok[waitAnswer](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 10})
	}()
	time.Sleep(200 * time.Millisecond)
	h.announce(acc, id, "Zanzibarsubject")
	<-waited
	ok[service.ActionResult](t, cs, "mark_read", map[string]any{"ids": []int64{id}})

	logs := h.logs.String()
	for _, s := range secrets {
		if strings.Contains(logs, s) {
			t.Errorf("the log contains %q:\n%s", s, logs)
		}
	}
	prefix, _, _ := strings.Cut(key, ".")
	for _, want := range []string{`"tool":"search_messages"`, `"tool":"get_message"`, `"tool":"wait_for_new_mail"`,
		`"key":"` + prefix + `"`, `"outcome":"bad_request"`, `"message":` + itoa(id)} {
		if !strings.Contains(logs, want) {
			t.Errorf("the log lacks %s: which tool ran, for which key, how it ended and on what", want)
		}
	}
}

func TestResourcesNameAFolderByIdOrRole(t *testing.T) {
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	const acc = "acc_00000000000000a1"
	box := h.mailbox(acc, ana.user.ID, "ana@work.example")
	id := h.deliver(acc, box, letter("Hello", "Bea <bea@example.org>", "Hi", "a.pdf", "%PDF"))
	cs := h.connect(h.key(ana, "read"), "", nil)
	folders := ok[struct {
		Folders []service.Folder `json:"folders"`
	}](t, cs, "list_folders", map[string]any{"account": acc})
	var inbox int64
	for _, f := range folders.Folders {
		if f.Role == "inbox" {
			inbox = f.ID
		}
	}
	for _, name := range []string{"inbox", itoa(inbox)} {
		var got struct {
			FolderID int64                    `json:"folder_id"`
			Messages []service.MessageSummary `json:"messages"`
		}
		if err := readResource(t, cs, "mail://"+acc+"/folder/"+name, &got); err != nil {
			t.Fatalf("folder %s: %v", name, err)
		}
		if got.FolderID != inbox || len(got.Messages) != 1 || got.Messages[0].ID != id {
			t.Errorf("folder %s read %+v", name, got)
		}
	}
	var message service.Message
	if err := readResource(t, cs, "mail://"+acc+"/message/"+itoa(id), &message); err != nil || message.ID != id {
		t.Errorf("message resource: %+v %v", message.ID, err)
	}
	var v any
	if err := readResource(t, cs, "mail://"+acc+"/folder/nonexistent", &v); err == nil {
		t.Error("a folder role nobody has read as something")
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func hasFlag(flags []goimap.Flag, want goimap.Flag) bool {
	return slices.ContainsFunc(flags, func(f goimap.Flag) bool { return strings.EqualFold(string(f), string(want)) })
}

func prefixOf(key string) string {
	prefix, _, _ := strings.Cut(key, ".")
	return prefix
}

// waitForLog waits for a line holding every one of parts: what the server
// did on its own goroutine, with no reply to wait on.
func (h *harness) waitForLog(parts ...string) {
	h.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, line := range strings.Split(h.logs.String(), "\n") {
			if !slices.ContainsFunc(parts, func(p string) bool { return !strings.Contains(line, p) }) {
				return
			}
		}
		if time.Now().After(deadline) {
			h.t.Fatalf("the log never said %v:\n%s", parts, h.logs.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestAMisspeltArgumentIsRefusedRatherThanIgnored(t *testing.T) {
	// A filter that silently falls away widens a search, and a model acts on
	// what it finds.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	cs := h.connect(h.key(ana, "read"), "", nil)
	for _, c := range []struct {
		tool string
		args map[string]any
	}{
		{"search_messages", map[string]any{"query": "invoice"}},
		{"search_messages", map[string]any{"limit": 101}},
		{"get_message", map[string]any{"id": 1, "format": "markdown"}},
		{"wait_for_new_mail", map[string]any{"timeout_seconds": 231}},
		{"mark_read", map[string]any{"ids": []int64{}}},
	} {
		if res := call(t, cs, c.tool, c.args); !res.IsError {
			t.Errorf("%s(%v) was accepted: %s", c.tool, c.args, text(res))
		}
	}
}

func TestMailThatArrivesBetweenTwoStepsOfAWaitIsNotLost(t *testing.T) {
	// Each step is a subscription of its own; the cursor of one is where the
	// next starts, so mail committed between two is replayed, not missed.
	h := newHarness(t)
	mcp.SetWaitStep(h.mcp, time.Second)
	ana := h.person("ana@example.com", auth.RoleMember)
	const acc = "acc_00000000000000a1"
	box := h.mailbox(acc, ana.user.ID, "ana@work.example")
	id := h.deliver(acc, box, letter("Between", "Bea <bea@example.org>", "Hi", "a.pdf", "%PDF"))
	var once sync.Once
	mcp.SetBetweenSteps(h.mcp, func() { once.Do(func() { h.announce(acc, id, "Between") }) })
	cs := h.connect(h.key(ana, "read"), "", nil)
	got := ok[waitAnswer](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 4})
	if got.TimedOut || len(got.Messages) != 1 || got.Messages[0].ID != id {
		t.Fatalf("mail announced between two steps was lost: %+v", got)
	}
}

func TestOnlyTheInboxesASessionSubscribedToAreAnnounced(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		const work, home = "acc_00000000000000a1", "acc_00000000000000a2"
		workBox := h.mailbox(work, ana.user.ID, "ana@work.example")
		homeBox := h.mailbox(home, ana.user.ID, "ana@home.example")
		workID := h.deliver(work, workBox, letter("Report", "Boss <boss@work.example>", "Q3", "q.pdf", "%PDF"))
		homeID := h.deliver(home, homeBox, letter("Dinner", "Mom <mom@home.example>", "Sunday", "m.pdf", "%PDF"))
		var mu sync.Mutex
		var updated []string
		// A key for both of ana's mailboxes.
		cs := h.connect(h.key(ana, "read"), protocol, &sdk.ClientOptions{
			ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
				mu.Lock()
				updated = append(updated, req.Params.URI)
				mu.Unlock()
			},
		})
		// A message of one mailbox under the other's name names nothing.
		var v any
		if err := readResource(t, cs, "mail://"+work+"/message/"+itoa(homeID), &v); err == nil {
			t.Errorf("read %s's message under %s's name", home, work)
		}
		if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + work + "/folder/inbox"}); err != nil {
			t.Fatal(err)
		}
		h.waitForLog(`"msg":"mcp subscription"`, `"account":"`+work+`"`, `"outcome":"ok"`)
		time.Sleep(200 * time.Millisecond)
		h.announce(home, homeID, "Dinner")
		time.Sleep(300 * time.Millisecond)
		h.announce(work, workID, "Report")
		deadline := time.Now().Add(5 * time.Second)
		for {
			mu.Lock()
			seen := slices.Clone(updated)
			mu.Unlock()
			if len(seen) > 0 || time.Now().After(deadline) {
				if !slices.Equal(seen, []string{"mail://" + work + "/folder/inbox"}) {
					t.Errorf("resource updates %v; want one, for the inbox subscribed to", seen)
				}
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	})
}

func TestAnAccountThatIsNotAnAccountIDNeverReachesTheLog(t *testing.T) {
	// A model can pass anything as an account, free text included, and a
	// client can write anything into a resource's URI. The log names an
	// account only when it is one's id.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	h.mailbox("acc_00000000000000a1", ana.user.ID, "ana@work.example")
	cs := h.connect(h.key(ana, "read"), "2025-11-25", nil)
	const words = "Divorcelawyerinvoice"
	for _, tool := range []string{"list_folders", "search_messages", "wait_for_new_mail"} {
		for _, account := range []string{words, words + strings.Repeat(" and more", 1<<16)} {
			args := map[string]any{"account": account}
			if tool == "wait_for_new_mail" {
				args["timeout_seconds"] = 1
			}
			if res := call(t, cs, tool, args); !res.IsError {
				t.Errorf("%s took free text as an account: %s", tool, text(res))
			}
		}
	}
	var v any
	for _, uri := range []string{"mail://" + words + "/message/1", "mail://" + words + "/folder/inbox"} {
		if err := readResource(t, cs, uri, &v); err == nil {
			t.Errorf("read %s", uri)
		}
	}
	if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + words + "/folder/inbox"}); err == nil {
		t.Error("subscribed to an inbox named by free text")
	}
	// Refusals are still logged, and an account id still named.
	refused(t, cs, "list_folders", map[string]any{"account": "acc_00000000000000b1"}, service.CodeNotFound)
	h.waitForLog(`"tool":"list_folders"`, `"account":"acc_00000000000000b1"`, `"outcome":"not_found"`)
	h.waitForLog(`"resource":"message"`, `"outcome":"not_found"`)
	h.waitForLog(`"msg":"mcp subscription refused"`, `"outcome":"not_found"`)
	if logs := h.logs.String(); strings.Contains(logs, words) {
		t.Errorf("the log holds what a client passed as an account:\n%.2000s", logs)
	}
}

func TestAMessageResourceIsNeverMarkedPubliclyCacheable(t *testing.T) {
	// "public" lets any intermediary keep a result and hand it to anybody;
	// every result here is one key's mail.
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		const acc = "acc_00000000000000a1"
		box := h.mailbox(acc, ana.user.ID, "ana@work.example")
		id := h.deliver(acc, box, letter("Results", "Clinic <lab@clinic.example>", "All clear", "r.pdf", "%PDF"))
		cs := h.connect(h.key(ana, "read"), protocol, nil)
		for _, uri := range []string{"mail://" + acc + "/message/" + itoa(id), "mail://accounts",
			"mail://" + acc + "/folder/inbox"} {
			res, err := cs.ReadResource(t.Context(), &sdk.ReadResourceParams{URI: uri})
			if err != nil {
				t.Fatalf("read %s: %v", uri, err)
			}
			if res.CacheScope != "private" || res.TTLMs != 0 {
				t.Errorf("%s: cacheScope %q, ttlMs %d; want private and 0", uri, res.CacheScope, res.TTLMs)
			}
		}
		tools, err := cs.ListTools(t.Context(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if tools.CacheScope != "private" {
			t.Errorf("tools/list: cacheScope %q", tools.CacheScope)
		}
	})
}

func TestARevokedKeysSubscriptionIsNotToldOfNewMail(t *testing.T) {
	eachProtocol(t, func(t *testing.T, protocol string) {
		h := newHarness(t)
		ana := h.person("ana@example.com", auth.RoleMember)
		const acc = "acc_00000000000000a1"
		box := h.mailbox(acc, ana.user.ID, "ana@work.example")
		id := h.deliver(acc, box, letter("After", "Bea <bea@example.org>", "Hi", "a.pdf", "%PDF"))
		key := h.key(ana, "read")
		var mu sync.Mutex
		var updated []string
		cs := h.connect(key, protocol, &sdk.ClientOptions{
			ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
				mu.Lock()
				updated = append(updated, req.Params.URI)
				mu.Unlock()
			},
		})
		if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + acc + "/folder/inbox"}); err != nil {
			t.Fatal(err)
		}
		h.waitForLog(`"msg":"mcp subscription"`, `"account":"`+acc+`"`, `"outcome":"ok"`)
		// The watcher is waiting on the journal when the key is revoked, and
		// mail arrives during that wait.
		time.Sleep(300 * time.Millisecond)
		if err := h.svc.RevokeMyAPIKey(t.Context(), ana.session, prefixOf(key)); err != nil {
			t.Fatal(err)
		}
		h.announce(acc, id, "After")
		h.waitForLog(`"msg":"mcp subscriptions stopped: the key no longer works"`, `"key":"`+prefixOf(key)+`"`)
		time.Sleep(200 * time.Millisecond)
		mu.Lock()
		defer mu.Unlock()
		if len(updated) != 0 {
			t.Errorf("a revoked key's session was told %v changed", updated)
		}
	})
}

func TestAKeyRunsAtMostItsShareOfCallsAtOnce(t *testing.T) {
	h := newHarness(t)
	mcp.SetMaxCallsInFlight(h.mcp, 2)
	ana := h.person("ana@example.com", auth.RoleMember)
	key := h.key(ana, "read")
	// Two sessions of one key share its share.
	one, other := h.connect(key, "", nil), h.connect(key, "2025-11-25", nil)
	waiting, stop := context.WithCancel(t.Context())
	var waits sync.WaitGroup
	for _, cs := range []*sdk.ClientSession{one, other} {
		waits.Go(func() {
			// Cancelled below; how it ends is not the point.
			_, _ = cs.CallTool(waiting, &sdk.CallToolParams{Name: "wait_for_new_mail",
				Arguments: map[string]any{"timeout_seconds": 60}})
		})
	}
	running := func(n int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for mcp.CallsInFlight(h.mcp, prefixOf(key)) != n {
			if time.Now().After(deadline) {
				t.Fatalf("the key runs %d calls, want %d", mcp.CallsInFlight(h.mcp, prefixOf(key)), n)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	running(2)
	if msg := refused(t, one, "list_accounts", nil, service.CodeRateLimited); !strings.Contains(msg, "try again in") {
		t.Errorf("a call over the share says %q; it should say when to try again", msg)
	}
	var v any
	if err := readResource(t, other, "mail://accounts", &v); err == nil {
		t.Error("a resource read went past the key's share")
	}
	// Another key has a share of its own.
	ok[map[string]any](t, h.connect(h.key(ana, "read"), "", nil), "list_accounts", nil)

	stop()
	waits.Wait()
	running(0)
	ok[map[string]any](t, one, "list_accounts", nil)
}

func TestSessionsThatClosedLeaveNoMemoryBehind(t *testing.T) {
	// Every session registers the same tool definitions, so the SDK's schema
	// cache, which keeps what it is given for good, holds one set for the
	// process however many sessions come and go.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	p, err := h.svc.AuthenticateTool(t.Context(), h.key(ana, "read"), nil)
	if err != nil {
		t.Fatal(err)
	}
	session := func() {
		ct, st := sdk.NewInMemoryTransports()
		ss, err := h.mcp.NewSession(p).Connect(t.Context(), st, nil)
		if err != nil {
			t.Fatal(err)
		}
		cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "1"}, nil).Connect(t.Context(), ct, nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = cs.Close()
		_ = ss.Close()
	}
	heap := func() int64 {
		runtime.GC()
		runtime.GC()
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		return int64(m.HeapAlloc)
	}
	for range 20 {
		session()
	}
	before := heap()
	const sessions = 300
	for range sessions {
		session()
	}
	grew := heap() - before
	// The server, and the cache it holds, live on for the sessions to come.
	runtime.KeepAlive(h.mcp)
	// Before the definitions were shared, each session left about 90 KiB.
	if grew > 4<<20 {
		t.Errorf("%d sessions that closed left %d KiB behind", sessions, grew>>10)
	}
}

func TestAHeldSessionNeverReachesAMailboxItsKeyLostEvenOnceReadComesBack(t *testing.T) {
	// A client that launched the daemon holds the key it authenticated with
	// for the whole session. Bea's key was made for the team's mailbox and
	// her own; losing read on the team's takes it out of the key for good,
	// so the session stops at its next call rather than reaching the
	// mailbox again once read is granted back. The refusal says so, rather
	// than that the key is dead: a new session with the same key goes on
	// with her own mailbox.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	bea := h.person("bea@example.com", auth.RoleMember)
	team, ws := h.team(ana, bea)
	const shared, own = "acc_00000000000000aa", "acc_00000000000000bb"
	h.mailboxIn(shared, team, ana.user.ID, "support@mail.example")
	h.mailbox(own, bea.user.ID, "bea@mail.example")
	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	key := h.key(bea, "read", shared, own)
	cs := h.connect(key, "", nil)
	ok[service.MessagePage](t, cs, "search_messages", map[string]any{"account": shared})

	if _, err := ws.Revoke(t.Context(), shared, bea.user.ID, workspace.Flags{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	msg := refused(t, cs, "search_messages", map[string]any{"account": shared}, service.CodeConflict)
	if !strings.Contains(msg, "restart the session") {
		t.Errorf("the refusal does not say what to do: %q", msg)
	}
	refused(t, cs, "list_folders", map[string]any{"account": own}, service.CodeConflict)

	again := h.connect(key, "", nil)
	ok[service.MessagePage](t, again, "search_messages", map[string]any{"account": own})
	refused(t, again, "search_messages", map[string]any{"account": shared}, service.CodeNotFound)
}

func TestSubscribingToAnInboxNeedsReadAccessToIt(t *testing.T) {
	// Bea may send from the team's mailbox, and so sees it; being told when
	// mail arrives in it is reading it.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	bea := h.person("bea@example.com", auth.RoleMember)
	team, ws := h.team(ana, bea)
	const shared = "acc_00000000000000aa"
	h.mailboxIn(shared, team, ana.user.ID, "support@mail.example")
	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Send: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	// Under the older protocol the refusal is the answer.
	cs := h.connect(h.key(bea, "read"), "2025-11-25", nil)
	inbox := "mail://" + shared + "/folder/inbox"
	if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: inbox}); err == nil {
		t.Error("subscribed to an inbox without read access to it")
	}
	h.waitForLog(`"msg":"mcp subscription refused"`, `"account":"`+shared+`"`, `"outcome":"not_authorized"`)

	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true, Send: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: inbox}); err != nil {
		t.Errorf("subscribing with read: %v", err)
	}
}

func TestASubscriptionToAnInboxTheKeyCanNoLongerReadIsDropped(t *testing.T) {
	// Bea's key follows whatever she may read. She subscribed to the team's
	// inbox and her own, then lost read on the team's: the watch drops it
	// at its next turn, and never announces it again.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	bea := h.person("bea@example.com", auth.RoleMember)
	team, ws := h.team(ana, bea)
	const shared, own = "acc_00000000000000aa", "acc_00000000000000bb"
	sharedBox := h.mailboxIn(shared, team, ana.user.ID, "support@mail.example")
	ownBox := h.mailbox(own, bea.user.ID, "bea@mail.example")
	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true, Send: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var updated []string
	key := h.key(bea, "read")
	cs := h.connect(key, "", &sdk.ClientOptions{
		ResourceUpdatedHandler: func(_ context.Context, req *sdk.ResourceUpdatedNotificationRequest) {
			mu.Lock()
			updated = append(updated, req.Params.URI)
			mu.Unlock()
		},
	})
	for _, acc := range []string{shared, own} {
		if err := cs.Subscribe(t.Context(), &sdk.SubscribeParams{URI: "mail://" + acc + "/folder/inbox"}); err != nil {
			t.Fatal(err)
		}
		h.waitForLog(`"msg":"mcp subscription"`, `"account":"`+acc+`"`, `"outcome":"ok"`)
	}
	time.Sleep(300 * time.Millisecond)

	if _, err := ws.Revoke(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true}, nil); err != nil {
		t.Fatal(err)
	}
	// Mail in her own inbox turns the watch.
	h.announce(own, h.deliver(own, ownBox, letter("Hello", "Carl <carl@example.org>", "Hi", "h.pdf", "%PDF")), "Hello")
	h.waitForLog(`"msg":"mcp subscription dropped: the key can no longer read the inbox"`, `"key":"`+prefixOf(key)+`"`,
		`"account":"`+shared+`"`, `"outcome":"not_authorized"`)
	time.Sleep(200 * time.Millisecond)
	h.announce(shared, h.deliver(shared, sharedBox, letter("Refund", "Client <c@example.org>", "x", "r.pdf", "%PDF")), "Refund")
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(updated, []string{"mail://" + own + "/folder/inbox"}) {
		t.Errorf("resource updates %v; want one, for her own inbox", updated)
	}
}

func TestAMemberWithoutAGrantCannotSeeTheMailbox(t *testing.T) {
	// Bea belongs to the team Ana linked a mailbox into, and holds nothing on
	// it: through her key, tools, resources and the wait never reach it.
	// Then read is granted, and the same calls do, which is what makes the
	// refusals about the grant.
	h := newHarness(t)
	ana := h.person("ana@example.com", auth.RoleMember)
	bea := h.person("bea@example.com", auth.RoleMember)
	team, ws := h.team(ana, bea)
	const shared = "acc_00000000000000aa"
	box := h.mailboxIn(shared, team, ana.user.ID, "support@mail.example")
	id := h.deliver(shared, box, letter("Refund", "Client <c@example.org>", "Order 4471", "r.pdf", "%PDF"))
	cs := h.connect(h.key(bea, "read"), "", nil)

	accounts := ok[struct {
		Accounts []service.Account `json:"accounts"`
	}](t, cs, "list_accounts", nil)
	if len(accounts.Accounts) != 0 {
		t.Errorf("list_accounts = %+v", accounts.Accounts)
	}
	if page := ok[service.MessagePage](t, cs, "search_messages", nil); len(page.Messages) != 0 {
		t.Errorf("search found %+v", page.Messages)
	}
	refused(t, cs, "list_folders", map[string]any{"account": shared}, service.CodeNotFound)
	refused(t, cs, "search_messages", map[string]any{"account": shared}, service.CodeNotFound)
	refused(t, cs, "get_message", map[string]any{"id": id}, service.CodeNotFound)
	refused(t, cs, "wait_for_new_mail", map[string]any{"account": shared, "timeout_seconds": 1}, service.CodeNotFound)
	var v any
	if err := readResource(t, cs, "mail://"+shared+"/folder/inbox", &v); err == nil {
		t.Errorf("read the team's inbox: %v", v)
	}
	waited := make(chan waitAnswer, 1)
	go func() { waited <- ok[waitAnswer](t, cs, "wait_for_new_mail", map[string]any{"timeout_seconds": 2}) }()
	time.Sleep(300 * time.Millisecond)
	h.announce(shared, id, "Refund")
	if got := <-waited; len(got.Messages) != 0 {
		t.Errorf("the wait handed over %+v", got.Messages)
	}
	if n := box.CallCount(providertest.MethodFetchPart); n != 0 {
		t.Errorf("refused reads fetched %d parts", n)
	}

	if _, err := ws.SetGrant(t.Context(), shared, bea.user.ID, workspace.Flags{Read: true}, ana.user.ID, nil); err != nil {
		t.Fatal(err)
	}
	if page := ok[service.MessagePage](t, cs, "search_messages", map[string]any{"account": shared}); len(page.Messages) != 1 {
		t.Errorf("with read, search found %+v", page.Messages)
	}
	ok[service.Message](t, cs, "get_message", map[string]any{"id": id})
}
