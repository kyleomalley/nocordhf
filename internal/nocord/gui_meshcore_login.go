package nocord

// gui_meshcore_login.go — private-repeater authentication.
// Right-click a repeater / room-server contact → "Login…"
// prompts for a password, fires CmdSendLogin, and surfaces the
// outcome (PushLoginSuccess / PushLoginFail → EventLoginResult)
// in the chat as a system message.
//
// Passwords are NOT persisted to disk. Operators paste from
// their password manager per session; the alternative (per-
// pubkey password storage in Fyne prefs) would land in the
// preferences.json plaintext, which leaks easily via crash
// dumps / diag bundles / shared screenshots. A future macOS
// Keychain integration could lift this restriction without
// changing the protocol layer.

import (
	"fmt"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/kyleomalley/nocordhf/lib/meshcore"
)

type mcPendingLogin struct {
	contact meshcore.Contact
	startAt time.Time
	expires time.Time
}

var (
	mcLoginMu       sync.Mutex
	mcLoginByPrefix = map[meshcore.PubKeyPrefix]*mcPendingLogin{}
	mcLoginMaxAge   = 30 * time.Second
)

// showMcLoginDialog opens a password prompt for the given
// repeater / room-server contact. Submit fires SendLogin via
// the live client; the result arrives as EventLoginResult and
// handleMcLoginResult renders the outcome.
func (g *GUI) showMcLoginDialog(ct meshcore.Contact) {
	if g.window == nil {
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	if ct.Type != meshcore.AdvTypeRepeater && ct.Type != meshcore.AdvTypeRoom {
		g.mcAppendSystem(fmt.Sprintf("login: %s isn't a repeater / room — login likely to fail or time out", display))
	}
	passwordEntry := widget.NewPasswordEntry()
	passwordEntry.SetPlaceHolder("repeater password (max 15 chars)")
	body := container.NewVBox(
		wrappedLabel(fmt.Sprintf("Authenticate against %s (%s).", display, ct.Type.String())),
		passwordEntry,
		wrappedLabel("Password is sent over the encrypted mesh DM channel and never stored on disk. Max 15 characters — the firmware truncates anything longer."),
	)
	dialog.ShowCustomConfirm("Login", "Send", "Cancel", body, func(ok bool) {
		if !ok {
			return
		}
		g.requestMcContactLogin(ct, passwordEntry.Text)
	}, g.window)
}

// requestMcContactLogin issues the SendLogin wire call and
// arms the pending-login correlation entry. Surfaces a system
// message either way.
func (g *GUI) requestMcContactLogin(ct meshcore.Contact, password string) {
	g.mcMu.Lock()
	client := g.mcClient
	g.mcMu.Unlock()
	if client == nil {
		g.mcAppendSystem("login: not connected")
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	pending := &mcPendingLogin{
		contact: ct,
		startAt: time.Now(),
		expires: time.Now().Add(mcLoginMaxAge),
	}
	prefix := ct.PubKey.Prefix()
	mcLoginMu.Lock()
	mcLoginByPrefix[prefix] = pending
	mcLoginMu.Unlock()
	go func() {
		sent, err := client.SendLogin(ct.PubKey, password)
		if err != nil {
			mcLoginMu.Lock()
			delete(mcLoginByPrefix, prefix)
			mcLoginMu.Unlock()
			g.mcAppendSystem(fmt.Sprintf("login %s: %s", display, err.Error()))
			return
		}
		if sent.EstTimeoutMilli > 0 {
			est := time.Duration(sent.EstTimeoutMilli) * time.Millisecond
			if est > mcLoginMaxAge {
				mcLoginMu.Lock()
				pending.expires = time.Now().Add(est)
				mcLoginMu.Unlock()
			}
		}
		g.mcAppendSystem(fmt.Sprintf("login %s queued (~%ds)", display, sent.EstTimeoutMilli/1000))
	}()
}

// handleMcLoginResult fires from the events goroutine on a
// PushLoginSuccess / PushLoginFail. Matches by sender prefix and
// dispatches to a follow-up dialog: a menu of admin actions on
// success, an error dialog on failure. Surfaces a brief system
// message either way so the chat preserves a record of the
// outcome even after the dialog is dismissed.
func (g *GUI) handleMcLoginResult(ev meshcore.EventLoginResult) {
	mcLoginMu.Lock()
	p, ok := mcLoginByPrefix[ev.SenderPrefix]
	if ok {
		delete(mcLoginByPrefix, ev.SenderPrefix)
	}
	mcLoginMu.Unlock()
	if !ok {
		gcMcLogin()
		return
	}
	display := p.contact.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", p.contact.PubKey[:6])
	}
	elapsed := time.Since(p.startAt).Round(time.Millisecond)
	if ev.Success {
		g.mcAppendSystem(fmt.Sprintf("login %s: success (%s) — admin actions available", display, elapsed))
		fyne.Do(func() { g.showMcAdminMenu(p.contact) })
		return
	}
	g.mcAppendSystem(fmt.Sprintf("login %s: FAIL (%s)", display, elapsed))
	if g.window != nil {
		fyne.Do(func() {
			dialog.ShowError(fmt.Errorf("login to %s failed — wrong password or not authorised", display), g.window)
		})
	}
}

// showMcAdminMenu opens an admin-actions modal for a contact the
// operator has just successfully logged into. The menu is a
// single-shot affair (re-login to bring it back) so the operator
// can pick an action without rummaging through the contact
// context menu, and so the elevated session feels distinct from
// the regular Settings → MeshCore plumbing.
//
// Available actions:
//   - Query repeater status — same as the right-click menu item;
//     surfaces here as the most likely follow-up to login.
//   - Send CLI command…    — TxtCliData DM; the repeater
//     interprets the body as a command line (firmware-specific
//     commands like "reboot", "set freq", etc.).
//   - Open DM thread       — switch the chat to this contact so
//     replies / status streams show up in the right pane.
//   - Done                 — dismiss the menu.
func (g *GUI) showMcAdminMenu(ct meshcore.Contact) {
	if g.window == nil {
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	statusBtn := widget.NewButtonWithIcon("Query repeater status", theme.SearchIcon(), func() {
		g.requestMcContactStatus(ct)
	})
	cliBtn := widget.NewButtonWithIcon("Send CLI command…", theme.DocumentCreateIcon(), func() {
		g.showMcCliCommandDialog(ct)
	})
	openBtn := widget.NewButtonWithIcon("Open DM thread", theme.MailComposeIcon(), func() {
		g.mcSwitchThread(mcContactThreadID(ct))
	})
	body := container.NewVBox(
		wrappedLabel(fmt.Sprintf("Authenticated against %s.", display)),
		wrappedLabel("Admin actions accepted on this session until the link drops or the repeater times the login out:"),
		statusBtn,
		cliBtn,
		openBtn,
		wrappedLabel("Re-login from the contact's right-click menu when the elevated session lapses."),
	)
	d := dialog.NewCustom(fmt.Sprintf("Logged in: %s", display), "Done", body, g.window)
	d.Resize(fyne.NewSize(420, 320))
	d.Show()
}

// showMcCliCommandDialog prompts for a free-form CLI command to
// send to the (already-logged-in) repeater as a TxtCliData DM.
// Commands are firmware-specific — for stock MeshCore repeater
// firmware: "reboot", "set freq <kHz>", "set bw <Hz>",
// "set name <text>", etc. The operator should know the
// repeater's command set; we make no attempt to validate.
func (g *GUI) showMcCliCommandDialog(ct meshcore.Contact) {
	if g.window == nil {
		return
	}
	display := ct.AdvName
	if display == "" {
		display = fmt.Sprintf("%x", ct.PubKey[:6])
	}
	cmdEntry := widget.NewEntry()
	cmdEntry.SetPlaceHolder("e.g. reboot")
	body := container.NewVBox(
		wrappedLabel(fmt.Sprintf("Send a CLI command to %s.", display)),
		cmdEntry,
		wrappedLabel("Commands are firmware-specific. Common stock MeshCore repeater commands: \"reboot\", \"set freq <kHz>\", \"set bw <Hz>\", \"set name <text>\". Replies (if any) arrive as ordinary DMs in the contact's thread."),
	)
	dialog.ShowCustomConfirm("Send CLI command", "Send", "Cancel", body, func(ok bool) {
		if !ok {
			return
		}
		text := cmdEntry.Text
		if text == "" {
			return
		}
		g.mcMu.Lock()
		client := g.mcClient
		g.mcMu.Unlock()
		if client == nil {
			g.mcAppendSystem("cli: not connected")
			return
		}
		go func() {
			if _, err := client.SendContactCliCommand(ct.PubKey.Prefix(), time.Now().UTC(), text); err != nil {
				g.mcAppendSystem(fmt.Sprintf("cli %s: %s", display, err.Error()))
				return
			}
			g.mcAppendSystem(fmt.Sprintf("cli %s sent: %s", display, text))
		}()
	}, g.window)
}

// gcMcLogin drops pending entries whose deadline has passed.
// Same pattern as the other pending-request tables.
func gcMcLogin() {
	now := time.Now()
	mcLoginMu.Lock()
	defer mcLoginMu.Unlock()
	for k, p := range mcLoginByPrefix {
		if now.After(p.expires) {
			delete(mcLoginByPrefix, k)
		}
	}
}
