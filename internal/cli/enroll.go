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

	"github.com/mach6/yore/internal/config"
	"github.com/mach6/yore/internal/cryptobox"
	"github.com/mach6/yore/internal/daemon"
	"github.com/mach6/yore/internal/redact"
	"github.com/mach6/yore/internal/secret"
	"github.com/mach6/yore/internal/store"
	"github.com/mach6/yore/internal/syncer"
	"github.com/mach6/yore/internal/wire"
)

// resolveToken returns the single-use enrollment token, from the flag, then
// $YORE_TOKEN, then a tty prompt. It is called only once a run has established
// that this machine actually needs to enroll: an already-enrolled machine
// re-running `yore setup` (to pin a certificate, say) must not be asked for a
// credential it has no use for. The prompt asks for the token and nothing else.
// It used to call itself "the server token for the first machine", which is only
// true for the one enrollment that forms the group and is wrong advice on every
// machine after it. The token is NOT persisted anywhere: it authorizes exactly
// one enrollment and is spent by it. Whatever credential a machine needs
// afterwards is its own device key.
func resolveToken(tokenFlag string) (string, error) {
	token := strings.TrimSpace(firstNonEmpty(tokenFlag, os.Getenv("YORE_TOKEN")))
	if token == "" {
		token = strings.TrimSpace(prompt("  Enrollment token: "))
	}
	if token == "" {
		return "", errors.New("no enrollment token given")
	}
	return token, nil
}

// serverStepTimeout bounds one stretch of talking to the server. It is a
// per-stretch budget, never one deadline for a whole run: a run stops to ask
// for a token, and fetching that token means walking to another machine. A
// deadline that started before the question was asked expired while the user
// was answering it, and the enrollment that followed failed instantly: which
// reads exactly like the server rejecting a token that was in fact perfectly
// good. A var, not a const, only so a test can shrink it to prove that the
// enrollment gets a fresh one (see
// TestRunSetupDoesNotSpendItsBudgetWaitingForTheToken).
var serverStepTimeout = 60 * time.Second

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
// storing one on first use (in the OS keyring, else a 0600 fallback file).
func ensureDeviceKey(dir string) error {
	if err := config.EnsureDir(dir); err != nil {
		return err
	}
	_, err := secret.Open(dir).EnsureDeviceKey()
	return err
}

// syncerFor builds a Syncer against an explicit server, for enrollment paths
// that must not persist configuration until the server has accepted them. The
// store it opened comes back with it: the store is held under an exclusive file
// lock, so the caller has to release it rather than leave it to process exit; a
// second setup in one process would otherwise find its own lock in the way.
func syncerFor(dir, url, pin string) (*syncer.Syncer, *store.Store, error) {
	cfg, _ := config.Load(dir)
	key, err := secret.Open(dir).LoadDeviceKey()
	if err != nil {
		return nil, nil, fmt.Errorf("device key: %w", err)
	}
	st, err := openStoreExclusive(dir)
	if err != nil {
		return nil, nil, err
	}
	return syncer.New(st, syncer.NewHTTPClient(url, pin), key, cfg.KeyEpochD()), st, nil
}

// isActiveMember reports whether id is an ACTIVE device in devs: the test for
// "this machine is already enrolled", which only an enrolled machine can make in
// the first place (a newcomer cannot read the device list at all).
func isActiveMember(devs []wire.Device, id string) bool {
	for _, d := range devs {
		if d.ID == id && d.Status == wire.DeviceActive {
			return true
		}
	}
	return false
}

// setupChanged reports whether a setup run altered any of the three fields it
// can touch. Nothing else in config.toml belongs to setup, and an enrolled
// machine re-running plain `yore setup` should leave the file (including
// anything hand-written in it) alone.
func setupChanged(prev, cur config.Config) bool {
	return prev.ServerURL != cur.ServerURL ||
		prev.ServerPin != cur.ServerPin ||
		prev.Integration != cur.Integration
}

// pinLabel abbreviates a pin for a one-line report: pins are 44 characters of
// base64 and only the leading few are read off a screen. A shorter value (a
// hand-edited config) is printed whole rather than sliced.
func pinLabel(pin string) string {
	if len(pin) <= 12 {
		return pin
	}
	return pin[:12] + "…"
}

// runSetup enrolls this machine using a single-use enrollment token.
//
// Nothing is persisted until the server has accepted the enrollment, so a wrong
// token or an unreachable server leaves no config behind. The first machine to
// enroll forms the group and is handed a recovery phrase; every later machine
// registers as pending and needs approval from one already enrolled.
func runSetup(server, token, name, integration string, pin, clearPin bool) int {
	dir := stateDir()
	u := newUI()
	if pin && clearPin {
		u.fail("--pin and --clear-pin are mutually exclusive")
		return 1
	}
	url, err := resolveServerURL(dir, server)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	u.title("yore setup")

	// Assemble the config in memory; it is written only after enrollment
	// succeeds. The token is never written anywhere: it is spent by this run.
	cfg, _ := config.Load(dir)
	prev := cfg
	cfg.ServerURL = url

	// Shell integration mode (how deeply yore takes over history). Ask only when
	// neither the flag nor an existing config already answers it.
	if integration == "" && cfg.Integration == "" {
		integration = strings.TrimSpace(prompt(
			"History integration: takeover (yore is the only history), coexist, or capture [takeover]: "))
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
		u.step("pinned server certificate", "SPKI "+pinLabel(p)+": sync will refuse any other cert")
	case clearPin:
		// Cleared BEFORE the reachability check below, which uses cfg.ServerPin:
		// the reason to clear a pin is usually that the pinned certificate is gone
		// and sync is refusing to connect, so the old pin must not gate the run
		// that removes it.
		if cfg.ServerPin == "" {
			u.step("no certificate pin set", "nothing to clear")
		} else {
			u.step("cleared certificate pin", "SPKI "+pinLabel(cfg.ServerPin)+": sync accepts any valid cert")
		}
		cfg.ServerPin = ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), serverStepTimeout)
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

	// Build the syncer from the in-memory values, so config.toml is written only
	// after the server has actually accepted this machine. A rejected token or
	// an unreachable server must leave no configuration behind to wedge the next
	// run.
	sy, st, err := syncerFor(dir, url, cfg.ServerPin)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	defer func() { _ = st.Close() }()
	if name == "" {
		name, _ = os.Hostname()
	}

	// If this machine is already an active member it can read the device list;
	// a newcomer cannot. That distinguishes "already enrolled" from a genuine
	// failure, which would otherwise surface as a bare 401.
	if devs, derr := sy.Devices(ctx); derr == nil && isActiveMember(devs, sy.DeviceID()) {
		// Nothing to ENROLL, but this run's flags (--pin/--clear-pin, --server,
		// --integration) still have to be persisted. Returning without saving made
		// `setup --pin` and `--clear-pin` print success and change nothing, which is
		// the worst way for a security control to behave: it is the already-enrolled
		// machine you run them on.
		if !setupChanged(prev, cfg) {
			u.step("already enrolled and active", "nothing to do")
			fmt.Fprintln(os.Stderr)
			return 0
		}
		if err := config.Save(dir, cfg); err != nil {
			u.fail(err.Error())
			return 1
		}
		u.step("already enrolled and active", "configuration updated")
		fmt.Fprintln(os.Stderr)
		return 0
	}

	tkt, err := resolveToken(token)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	// A fresh deadline, starting now: the token prompt above can stand for as
	// long as it takes to walk to another machine and mint one.
	ectx, ecancel := context.WithTimeout(context.Background(), serverStepTimeout)
	defer ecancel()
	formed, code, err := sy.Enroll(ectx, name, tkt)
	if err != nil {
		switch {
		case isUnauthorized(err):
			u.fail("enrollment refused by the server")
			u.note("Tokens are single-use and expire after 30 minutes.")
			u.note("The server's own token enrolls only while the group has no active device.")
			fmt.Fprintln(os.Stderr)
			u.next("mint a fresh token on a machine that is already enrolled:",
				"yore devices token")
		case errors.Is(err, context.DeadlineExceeded):
			// Not a refusal: the server never answered. Saying "refused" here sent
			// people off to mint another token for a problem no token can fix.
			u.fail("the server did not answer in time")
			u.note(err.Error())
			u.note("If the request did arrive, that token is spent: mint another to retry.")
		default:
			u.fail("enrollment failed")
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
			"you approve: it proves no key was substituted.")
		u.next("on a machine that is already enrolled:",
			"yore devices    (select this machine, press a)")
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
	// Deriving the recovery key is deliberately slow, and on a small machine it
	// is slow enough to be worth its own budget rather than eating the
	// enrollment's.
	bctx, bcancel := context.WithTimeout(context.Background(), serverStepTimeout)
	defer bcancel()
	if err := sy.Bootstrap(bctx, rk, salt); err != nil {
		u.fail("bootstrap: " + err.Error())
		return 1
	}

	u.step("enrolled "+strconv.Quote(name), "first device: history group created")
	u.panel("Recovery phrase", phrase,
		"",
		"Write this down and keep it somewhere safe.",
		"It is the ONLY way back if you lose every enrolled",
		"machine. Shown once, and never stored.")
	u.next("add another machine:",
		"yore devices token        (here)",
		"yore setup --token <t>    (there)")
	return 0
}

// isUnauthorized reports whether err is the server's generic 401. The server
// never says which check failed, so the client explains the likely causes.
func isUnauthorized(err error) bool {
	var ae *syncer.APIError
	return errors.As(err, &ae) && ae.Status == http.StatusUnauthorized
}

// seedRedact installs the editable secret-redaction rules on first setup. It
// never clobbers an existing redact.yml, and a failure must not fail setup:
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
// every time they touched their devices.
func devicesClient() (*daemon.Client, error) {
	return daemon.EnsureRunning(stateDir())
}

// --- small tty helpers ---

func prompt(msg string) string {
	fmt.Fprint(os.Stderr, msg)
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimRight(line, "\r\n")
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// runDevicesToken mints a single-use enrollment token for adding a machine.
func runDevicesToken() int {
	u := newUI()
	c, err := devicesClient()
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	defer func() { _ = c.Close() }()

	t, err := c.Token()
	if err != nil {
		u.fail(err.Error())
		return 1
	}

	// The token itself goes to STDOUT and nothing else does, so
	// `TOKEN=$(yore devices token)` captures exactly the token. Everything
	// below is decoration on stderr.
	fmt.Println(t.Token)

	u.panel("Single-use enrollment token", t.Token,
		"",
		"Valid until "+time.UnixMilli(t.ExpiresMs).Format("15:04 MST on Mon 2 Jan")+".",
		"It admits exactly one machine, then it is spent.")
	u.next("on the new machine:",
		"yore setup --token "+t.Token)
	return 0
}

// runRecover rebuilds access from the recovery phrase when no enrolled machine
// survives. It derives the recovery key, proves possession of it to fetch the
// wrapped History Key, then enrolls this machine and admits it directly: there
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
			// A 401 here means the derived key did not match the stored one;
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
	// The recovery key authorizes its own enrollment token instead.
	tktResp, err := rc.RecoveryToken(ctx)
	if err != nil {
		u.fail("mint enrollment token: " + err.Error())
		return 1
	}

	if err := ensureDeviceKey(dir); err != nil {
		u.fail(err.Error())
		return 1
	}
	sy, st, err := syncerFor(dir, url, cfg.ServerPin)
	if err != nil {
		u.fail(err.Error())
		return 1
	}
	defer func() { _ = st.Close() }()
	name, _ := os.Hostname()
	if _, _, err := sy.Enroll(ctx, name, tktResp.Token); err != nil {
		u.fail("enroll: " + err.Error())
		return 1
	}

	// Admit ourselves on the recovery key's authority: no surviving device exists
	// to approve us, and the ones that could are the ones we lost.
	key, err := secret.Open(dir).LoadDeviceKey()
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
		"yore devices    (select each one, press x)")
	return 0
}
