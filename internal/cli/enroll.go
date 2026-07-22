package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/daemon"
	"yore/internal/proto"
	"yore/internal/redact"
	"yore/internal/secret"
	"yore/internal/syncer"
	"yore/internal/wire"
)

// resolveServer returns the server URL and the single-use enrollment ticket for
// this setup, from flags, then config (URL only), then $YORE_TICKET, prompting
// on the tty for anything still missing.
//
// The ticket is NOT persisted anywhere: it authorizes exactly one enrollment
// and is spent by it. Whatever credential a machine needs afterwards is its own
// device key.
func resolveServer(dir, serverFlag, ticketFlag string) (url, ticket string, err error) {
	url, err = resolveServerURL(dir, serverFlag)
	if err != nil {
		return "", "", err
	}

	ticket = strings.TrimSpace(firstNonEmpty(ticketFlag, os.Getenv("YORE_TICKET")))
	if ticket == "" {
		ticket = strings.TrimSpace(prompt("Enrollment ticket (server token for the first machine): "))
	}
	if ticket == "" {
		return "", "", errors.New("no enrollment ticket given")
	}
	return url, ticket, nil
}

// resolveServerURL returns the server URL from the flag, then config, then a
// prompt.
func resolveServerURL(dir, serverFlag string) (string, error) {
	cfg, _ := config.Load(dir)
	url := firstNonEmpty(serverFlag, cfg.ServerURL)
	if url == "" {
		url = prompt("Server URL (e.g. https://yore.example.com): ")
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return "", errors.New("no server URL given")
	}
	return url, nil
}

// ensureDeviceKey makes sure this machine has a device key, generating and
// saving one (0600) on first use.
func ensureDeviceKey(dir string) error {
	path := config.KeyPath(dir)
	if _, err := cryptobox.LoadDeviceKey(path); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("reading device key: %w", err)
	}
	k, err := cryptobox.GenerateDeviceKey()
	if err != nil {
		return err
	}
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	return k.Save(path)
}

// syncerFor builds a Syncer against an explicit server, for enrollment paths
// that must not persist configuration until the server has accepted them.
func syncerFor(dir, url, pin string) (*syncer.Syncer, error) {
	cfg, _ := config.Load(dir)
	key, err := cryptobox.LoadDeviceKey(config.KeyPath(dir))
	if err != nil {
		return nil, fmt.Errorf("device key: %w", err)
	}
	st, err := openStoreExclusive(dir)
	if err != nil {
		return nil, err
	}
	return syncer.New(st, syncer.NewHTTPClient(url, pin), key, cfg.KeyEpochD()), nil
}

// runSetup enrolls this machine using a single-use enrollment ticket.
//
// Nothing is persisted until the server has accepted the enrollment, so a wrong
// ticket or an unreachable server leaves no config behind. The first machine to
// enroll forms the group and is handed a recovery phrase; every later machine
// registers as pending and needs approval from one already enrolled.
func runSetup(server, ticket, name, integration string, pin, clearPin bool) int {
	dir := stateDir()
	u := newUI()
	if pin && clearPin {
		u.fail("--pin and --clear-pin are mutually exclusive")
		return 1
	}
	url, tkt, err := resolveServer(dir, server, ticket)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	u.title("yore setup")

	// Assemble the config in memory; it is written only after enrollment
	// succeeds. The ticket is never written anywhere — it is spent by this run.
	cfg, _ := config.Load(dir)
	cfg.ServerURL = url

	// Shell integration mode (how deeply yore takes over history). Ask only when
	// neither the flag nor an existing config already answers it.
	if integration == "" && cfg.Integration == "" {
		integration = strings.TrimSpace(prompt(
			"History integration — takeover (yore is the only history), coexist, or capture [takeover]: "))
	}
	if integration == "" {
		integration = cfg.Integration
	}
	switch integration {
	case "coexist", "capture", "takeover":
		cfg.Integration = integration
	case "":
		cfg.Integration = "takeover"
	default:
		u.fail(fmt.Sprintf("unknown integration %q (want takeover|coexist|capture)", integration))
		return 1
	}
	switch {
	case pin:
		p, perr := syncer.ServerPin(url)
		if perr != nil {
			u.fail("cannot capture server certificate to pin: " + perr.Error())
			return 1
		}
		cfg.ServerPin = p
		u.step("pinned server certificate", "SPKI "+p[:12]+"… — sync will refuse any other cert")
	case clearPin:
		cfg.ServerPin = ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Reachability first: a clear "cannot reach server" beats a signature error.
	if err := syncer.NewHTTPClient(url, cfg.ServerPin).Health(ctx); err != nil {
		u.fail("cannot reach server " + url)
		u.note(err.Error())
		return 1
	}
	u.step("server reachable", url)

	if err := config.EnsureDir(dir); err != nil {
		u.fail(err.Error())
		return 1
	}
	if err := ensureDeviceKey(dir); err != nil {
		u.fail(err.Error())
		return 1
	}
	u.step("device key ready", fmt.Sprintf("secrets kept in the %s", secret.Open(dir).Backend()))
	seedRedact(dir)

	// Build the syncer from the in-memory values, so config.json is written only
	// after the server has actually accepted this machine. A rejected ticket or
	// an unreachable server must leave no configuration behind to wedge the next
	// run.
	sy, err := syncerFor(dir, url, cfg.ServerPin)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	if name == "" {
		name, _ = os.Hostname()
	}

	// If this machine is already an active member it can read the device list;
	// a newcomer cannot. That distinguishes "already enrolled" from a genuine
	// failure, which would otherwise surface as a bare 401.
	if devs, derr := sy.Devices(ctx); derr == nil {
		for _, d := range devs {
			if d.ID == sy.DeviceID() && d.Status == wire.DeviceActive {
				u.step("already enrolled and active", "nothing to do")
				fmt.Fprintln(os.Stderr)
				return 0
			}
		}
	}

	formed, code, err := sy.Enroll(ctx, name, tkt)
	if err != nil {
		u.fail("enrollment refused by the server")
		if isUnauthorized(err) {
			u.note("Tickets are single-use and expire after 30 minutes.")
			u.note("The server's own token enrolls only while the group has no active device.")
			fmt.Fprintln(os.Stderr)
			u.next("mint a fresh ticket on a machine that is already enrolled:",
				"yore devices ticket")
		} else {
			u.note(err.Error())
		}
		return 1
	}

	if err := config.Save(dir, cfg); err != nil {
		u.fail(err.Error())
		return 1
	}

	if formed {
		u.step("registered "+strconv.Quote(name), "pending approval")
		u.panel("Verification code", code,
			"",
			"Confirm this matches on the approving machine before",
			"you approve — it proves no key was substituted.")
		u.next("on a machine that is already enrolled:",
			"yore devices approve "+sy.DeviceID())
		return 0
	}

	// First machine: form the group, which also installs the recovery key.
	phrase, err := cryptobox.NewRecoveryPhrase()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup: generating recovery phrase:", err)
		return 1
	}
	salt, err := cryptobox.NewRecoverySalt()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup: generating recovery salt:", err)
		return 1
	}
	u.working("deriving your recovery key (this takes a moment)")
	rk, err := cryptobox.DeriveRecoveryKey(phrase, salt)
	if err != nil {
		u.fail("deriving recovery key: " + err.Error())
		return 1
	}
	if err := sy.Bootstrap(ctx, rk, salt); err != nil {
		u.fail("bootstrap: " + err.Error())
		return 1
	}

	u.step("enrolled "+strconv.Quote(name), "first device — history group created")
	u.panel("Recovery phrase", phrase,
		"",
		"Write this down and keep it somewhere safe.",
		"It is the ONLY way back if you lose every enrolled",
		"machine. Shown once, and never stored.")
	u.next("add another machine:",
		"yore devices ticket        (here)",
		"yore setup --ticket <t>    (there)")
	return 0
}

// isUnauthorized reports whether err is the server's generic 401. The server
// never says which check failed, so the client explains the likely causes.
func isUnauthorized(err error) bool {
	var ae *syncer.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

// seedRedact installs the editable secret-redaction rules on first setup. It
// never clobbers an existing redact.yml, and a failure must not fail setup —
// the built-in rules still apply as the fail-safe.
func seedRedact(dir string) {
	u := newUI()
	path := config.RedactPath(dir)
	_, statErr := os.Stat(path)
	if err := redact.Seed(dir); err != nil {
		u.warn("could not seed redact.yml (built-in rules still apply): " + err.Error())
	} else if os.IsNotExist(statErr) {
		u.step("secret-redaction rules seeded", path)
	}
}

// devicesClient returns a client to the running daemon (spawning one if
// needed). Device management goes THROUGH the daemon rather than opening the
// store directly: the daemon owns the store, so a direct open would have to ask
// it to shut down first, costing the user a warm daemon (and its RAM corpus)
// every time they listed their devices.
func devicesClient() (*daemon.Client, error) {
	return daemon.EnsureRunning(stateDir())
}

// runDevicesList lists the enrolled devices (bare `yore devices`).
func runDevicesList() int {
	c, err := devicesClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	info, err := c.Devices()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	if len(info.Devices) == 0 {
		fmt.Println("No devices enrolled. Run `yore setup`.")
		return 0
	}
	fmt.Printf("%-28s %-10s %-16s %s\n", "ID", "STATUS", "NAME", "CODE (pending)")
	for _, d := range info.Devices {
		self := ""
		if d.Self {
			self = "  <- this machine"
		}
		fmt.Printf("%-28s %-10s %-16s %s%s\n", d.ID, d.Status, truncate(d.Name, 16), d.Code, self)
	}
	return 0
}

// runDevicesApprove approves a pending device by id (`yore devices approve`),
// after showing its verification code for out-of-band confirmation.
func runDevicesApprove(id string) int {
	c, err := devicesClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	info, err := c.Devices()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	var target *proto.DeviceInfo
	for i := range info.Devices {
		if info.Devices[i].ID == id && info.Devices[i].Status == wire.DevicePending {
			target = &info.Devices[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "yore devices approve: no pending device %q\n", id)
		return 1
	}
	fmt.Printf("Approving %q\n  Verification code: %s\n", target.Name, target.Code)
	if !confirm("Does this match the code shown on that machine? [y/N] ") {
		fmt.Println("Aborted.")
		return 1
	}
	if err := c.Approve(id); err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	fmt.Println("Approved. That machine will sync automatically.")
	return 0
}

// runDevicesRevoke revokes a device by id and rotates keys (`yore devices revoke`).
func runDevicesRevoke(id string) int {
	c, err := devicesClient()
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices revoke:", err)
		return 1
	}
	defer func() { _ = c.Close() }()

	if err := c.Revoke(id); err != nil {
		fmt.Fprintln(os.Stderr, "yore devices revoke:", err)
		return 1
	}
	fmt.Println("Revoked and rotated keys. The removed machine can no longer decrypt new history.")
	return 0
}

// --- small tty helpers ---

func prompt(msg string) string {
	fmt.Fprint(os.Stderr, msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func confirm(msg string) bool {
	ans := strings.ToLower(strings.TrimSpace(prompt(msg)))
	return ans == "y" || ans == "yes"
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

// runDevicesTicket mints a single-use enrollment ticket for adding a machine.
func runDevicesTicket() int {
	u := newUI()
	c, err := devicesClient()
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	defer func() { _ = c.Close() }()

	t, err := c.Ticket()
	if err != nil {
		u.fail(err.Error())
		return 1
	}

	// The ticket itself goes to STDOUT and nothing else does, so
	// `TICKET=$(yore devices ticket)` captures exactly the ticket. Everything
	// below is decoration on stderr.
	fmt.Println(t.Ticket)

	u.panel("Single-use enrollment ticket", t.Ticket,
		"",
		"Valid until "+time.UnixMilli(t.ExpiresMs).Format("15:04 MST on Mon 2 Jan")+".",
		"It admits exactly one machine, then it is spent.")
	u.next("on the new machine:",
		"yore setup --ticket "+t.Ticket)
	return 0
}

// runRecover rebuilds access from the recovery phrase when no enrolled machine
// survives. It derives the recovery key, proves possession of it to fetch the
// wrapped History Key, then enrolls this machine and admits it directly — there
// is no other device left to approve it.
func runRecover(server string) int {
	dir := stateDir()
	u := newUI()
	url, err := resolveServerURL(dir, server)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	u.title("yore recover")

	phrase := strings.TrimSpace(prompt("  Recovery phrase: "))
	if phrase == "" {
		u.fail("no recovery phrase given")
		return 1
	}
	fmt.Fprintln(os.Stderr)

	cfg, _ := config.Load(dir)
	cfg.ServerURL = url
	if err := config.EnsureDir(dir); err != nil {
		u.fail(err.Error())
		return 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	u.working("deriving your recovery key (this takes a moment)")
	rc := syncer.NewHTTPClient(url, cfg.ServerPin)
	hk, wrapVersion, err := syncer.RecoverHK(ctx, rc, phrase)
	if err != nil {
		u.fail("could not recover with that phrase")
		if isUnauthorized(err) {
			// A 401 here means the derived key did not match the stored one —
			// i.e. the phrase is wrong. The raw error would add nothing.
			u.note("Check for typos: 8 groups of 4 characters.")
			u.note("Dashes, spaces, and letter case are all ignored.")
		} else {
			u.note(err.Error())
		}
		return 1
	}
	u.step("history key recovered", "the phrase was correct")

	// The machines we lost are still ACTIVE on the server, so no surviving
	// device can vouch for this one and the bootstrap allowance does not apply.
	// The recovery key authorizes its own enrollment ticket instead.
	tktResp, err := rc.RecoveryTicket(ctx)
	if err != nil {
		u.fail("mint enrollment ticket: " + err.Error())
		return 1
	}

	if err := ensureDeviceKey(dir); err != nil {
		u.fail(err.Error())
		return 1
	}
	sy, err := syncerFor(dir, url, cfg.ServerPin)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	name, _ := os.Hostname()
	if _, _, err := sy.Enroll(ctx, name, tktResp.Ticket); err != nil {
		u.fail("enroll: " + err.Error())
		return 1
	}

	// Admit ourselves on the recovery key's authority: no surviving device exists
	// to approve us, and the ones that could are the ones we lost.
	key, err := cryptobox.LoadDeviceKey(config.KeyPath(dir))
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	blob, err := cryptobox.WrapHK(hk, key.Public())
	if err != nil {
		u.fail("wrap history key: " + err.Error())
		return 1
	}
	if err := rc.RecoveryActivate(ctx, sy.DeviceID(), wire.ActivateReq{Wrap: wire.HKWrap{
		DeviceID:  sy.DeviceID(),
		Blob:      blob,
		HKVersion: wrapVersion,
	}}); err != nil {
		u.fail("activate: " + err.Error())
		return 1
	}

	if err := config.Save(dir, cfg); err != nil {
		u.fail(err.Error())
		return 1
	}
	seedRedact(dir)
	u.step("enrolled "+strconv.Quote(name), "admitted on the recovery key's authority")
	fmt.Fprintln(os.Stderr)
	u.next("clean up the machines you lost:",
		"yore devices",
		"yore devices revoke <id>")
	return 0
}
