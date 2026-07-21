package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"yore/internal/config"
	"yore/internal/cryptobox"
	"yore/internal/redact"
	"yore/internal/syncer"
	"yore/internal/wire"
)

// resolveServer returns the server URL and token from flags, then config, then
// environment ($YORE_TOKEN / $YORE_TOKEN_FILE), prompting on the tty for any
// still missing. It never returns empty values without an error.
func resolveServer(dir, serverFlag, tokenFlag string) (url, token string, err error) {
	cfg, _ := config.Load(dir)

	url = firstNonEmpty(serverFlag, cfg.ServerURL)
	if url == "" {
		url = prompt("Server URL (e.g. https://yore.example.com): ")
	}
	url = strings.TrimRight(strings.TrimSpace(url), "/")
	if url == "" {
		return "", "", errors.New("no server URL given")
	}

	token = firstNonEmpty(tokenFlag, os.Getenv("YORE_TOKEN"), cfg.Token)
	if token == "" {
		if tf := os.Getenv("YORE_TOKEN_FILE"); tf != "" {
			if b, rerr := os.ReadFile(tf); rerr == nil {
				token = string(trimNL(b))
			}
		}
	}
	if token == "" {
		token = strings.TrimSpace(prompt("Auth token: "))
	}
	if token == "" {
		return "", "", errors.New("no auth token given")
	}
	return url, token, nil
}

// loadOrCreateDeviceKey returns this machine's device key, generating and
// saving one (0600) on first use.
func loadOrCreateDeviceKey(dir string) (cryptobox.DeviceKey, error) {
	path := config.KeyPath(dir)
	if k, err := cryptobox.LoadDeviceKey(path); err == nil {
		return k, nil
	} else if !os.IsNotExist(err) {
		return cryptobox.DeviceKey{}, fmt.Errorf("reading device key: %w", err)
	}
	k, err := cryptobox.GenerateDeviceKey()
	if err != nil {
		return cryptobox.DeviceKey{}, err
	}
	if err := config.EnsureDir(dir); err != nil {
		return cryptobox.DeviceKey{}, err
	}
	if err := k.Save(path); err != nil {
		return cryptobox.DeviceKey{}, err
	}
	return k, nil
}

// buildSyncer wires a Syncer from persisted config for the enrollment commands
// and the daemon. It returns a friendly error when sync isn't configured yet.
func buildSyncer(dir string) (*syncer.Syncer, *syncer.HTTPClient, error) {
	cfg, _ := config.Load(dir)
	if cfg.ServerURL == "" {
		return nil, nil, errors.New("sync is not configured — run `yore setup`")
	}
	token := firstNonEmpty(cfg.Token, os.Getenv("YORE_TOKEN"))
	if token == "" {
		if tf := os.Getenv("YORE_TOKEN_FILE"); tf != "" {
			if b, err := os.ReadFile(tf); err == nil {
				token = string(trimNL(b))
			}
		}
	}
	key, err := cryptobox.LoadDeviceKey(config.KeyPath(dir))
	if err != nil {
		return nil, nil, fmt.Errorf("device key: %w (run `yore setup`)", err)
	}
	st, err := openStoreExclusive(dir)
	if err != nil {
		return nil, nil, err
	}
	http := syncer.NewHTTPClient(cfg.ServerURL, token, cfg.ServerPin)
	return syncer.New(st, http, key, cfg.KeyEpochD()), http, nil
}

// runSetup enrolls this machine: it VALIDATES the server URL + token against the
// server before persisting anything, then records them, ensures a device key,
// and either bootstraps a new history group (first machine) or registers as
// pending for approval from an already-enrolled machine. A failed setup never
// touches config.json, so a wrong token or unreachable server can't wedge future
// runs (resolveServer would otherwise reuse the bad token and keep 401-ing).
func runSetup(server, token, name, integration string, pin, clearPin bool) int {
	dir := stateDir()
	if pin && clearPin {
		fmt.Fprintln(os.Stderr, "yore setup: --pin and --clear-pin are mutually exclusive")
		return 1
	}
	url, tok, err := resolveServer(dir, server, token)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup:", err)
		return 1
	}

	// Assemble the config in memory; it is written only after the server accepts
	// the token below (see the config.Save further down).
	cfg, _ := config.Load(dir)
	cfg.ServerURL, cfg.Token = url, tok

	// Shell integration mode (how deeply yore takes over history).
	if integration == "" {
		integration = strings.TrimSpace(prompt(
			"History integration — takeover (yore is the only history), coexist, or capture [takeover]: "))
	}
	switch integration {
	case "coexist", "capture", "takeover":
		cfg.Integration = integration
	case "":
		cfg.Integration = "takeover"
	default:
		fmt.Fprintf(os.Stderr, "yore setup: unknown integration %q (want takeover|coexist|capture)\n", integration)
		return 1
	}
	switch {
	case pin:
		p, err := syncer.ServerPin(url)
		if err != nil {
			fmt.Fprintln(os.Stderr, "yore setup: cannot capture server certificate to pin:", err)
			return 1
		}
		cfg.ServerPin = p
		fmt.Printf("Pinned server certificate (SPKI %s…). Sync will refuse any other cert.\n", p[:12])
	case clearPin:
		cfg.ServerPin = "" // drop a previously pinned cert so the next Save removes it
	}

	// Validate BEFORE persisting: Health proves reachability, then the
	// authenticated ListDevices proves the token is accepted. Either failure
	// aborts WITHOUT a save — the invariant that a wrong token never lands in
	// config.json. Build the client straight from the in-memory values (no device
	// key needed just to check the token).
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	http := syncer.NewHTTPClient(url, tok, cfg.ServerPin)
	if err := http.Health(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "yore setup: cannot reach server:", err)
		return 1
	}
	devices, err := http.ListDevices(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup:", err)
		return 1
	}

	// The server accepted the token: only now is it safe to persist and to mint a
	// device key (so a failed setup also leaves no stray registration attempt).
	if err := config.Save(dir, cfg); err != nil {
		fmt.Fprintln(os.Stderr, "yore setup:", err)
		return 1
	}

	// Seed the editable secret-redaction rules on first setup. Seed never
	// clobbers an existing redact.yml, and a seed failure must not fail setup
	// (the built-in rules still apply as the fail-safe), so this is best-effort.
	redactPath := config.RedactPath(dir)
	_, statErr := os.Stat(redactPath)
	if err := redact.Seed(dir); err != nil {
		fmt.Fprintln(os.Stderr, "yore setup: could not seed redact.yml (built-in rules still apply):", err)
	} else if os.IsNotExist(statErr) {
		fmt.Printf("Seeded editable secret-redaction rules at %s.\n", redactPath)
	}

	if _, err := loadOrCreateDeviceKey(dir); err != nil {
		fmt.Fprintln(os.Stderr, "yore setup:", err)
		return 1
	}

	sy, _, err := buildSyncer(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup:", err)
		return 1
	}

	if name == "" {
		name, _ = os.Hostname()
	}

	// Reuse the device list already fetched during validation.
	for _, d := range devices {
		if d.ID == sy.DeviceID() && d.Status == wire.DeviceActive {
			fmt.Println("This machine is already enrolled and active.")
			return 0
		}
	}

	if !anyActive(devices) {
		// No active device anywhere: form the group.
		if err := sy.Bootstrap(ctx, name); err != nil {
			fmt.Fprintln(os.Stderr, "yore setup: bootstrap:", err)
			return 1
		}
		fmt.Printf("Enrolled %q as the first device and created the history group.\n", name)
		fmt.Println("Run `yore setup` on your other machines to add them.")
		return 0
	}

	code, err := sy.Register(ctx, name)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore setup: register:", err)
		return 1
	}
	fmt.Printf("Registered %q — pending approval.\n\n", name)
	fmt.Printf("  Verification code:  %s\n\n", code)
	fmt.Println("On a machine that's already enrolled, run:")
	fmt.Printf("    yore devices approve %s\n", sy.DeviceID())
	fmt.Println("and confirm the code above matches.")
	return 0
}

// runDevicesList lists the enrolled devices (bare `yore devices`).
func runDevicesList() int {
	dir := stateDir()
	sy, http, err := buildSyncer(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return listDevices(ctx, http, sy.DeviceID())
}

// runDevicesApprove approves a pending device by id (`yore devices approve`).
func runDevicesApprove(id string) int {
	dir := stateDir()
	sy, _, err := buildSyncer(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return approveDevice(ctx, sy, id)
}

// runDevicesRevoke revokes a device by id and rotates keys (`yore devices revoke`).
func runDevicesRevoke(id string) int {
	dir := stateDir()
	sy, _, err := buildSyncer(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sy.Revoke(ctx, id); err != nil {
		fmt.Fprintln(os.Stderr, "yore devices revoke:", err)
		return 1
	}
	fmt.Println("Revoked and rotated keys. The removed machine can no longer decrypt new history.")
	return 0
}

func listDevices(ctx context.Context, http *syncer.HTTPClient, selfID string) int {
	devices, err := http.ListDevices(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices:", err)
		return 1
	}
	if len(devices) == 0 {
		fmt.Println("No devices enrolled. Run `yore setup`.")
		return 0
	}
	fmt.Printf("%-28s %-10s %-16s %s\n", "ID", "STATUS", "NAME", "CODE (pending)")
	for _, d := range devices {
		code := ""
		if d.Status == wire.DevicePending {
			if pub, perr := cryptobox.PublicFromBytes(d.PubKey); perr == nil {
				code = syncer.VerificationCode(pub)
			}
		}
		self := ""
		if d.ID == selfID {
			self = "  <- this machine"
		}
		fmt.Printf("%-28s %-10s %-16s %s%s\n", d.ID, d.Status, truncate(d.Name, 16), code, self)
	}
	return 0
}

func approveDevice(ctx context.Context, sy *syncer.Syncer, id string) int {
	pending, err := sy.PendingDevices(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	var target *wire.Device
	for i := range pending {
		if pending[i].ID == id {
			target = &pending[i]
			break
		}
	}
	if target == nil {
		fmt.Fprintf(os.Stderr, "yore devices approve: no pending device %q\n", id)
		return 1
	}
	pub, err := cryptobox.PublicFromBytes(target.PubKey)
	if err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	fmt.Printf("Approving %q\n  Verification code: %s\n", target.Name, syncer.VerificationCode(pub))
	if !confirm("Does this match the code shown on that machine? [y/N] ") {
		fmt.Println("Aborted.")
		return 1
	}
	if err := sy.Approve(ctx, id); err != nil {
		fmt.Fprintln(os.Stderr, "yore devices approve:", err)
		return 1
	}
	fmt.Println("Approved. That machine will sync automatically.")
	return 0
}

func anyActive(devices []wire.Device) bool {
	for _, d := range devices {
		if d.Status == wire.DeviceActive {
			return true
		}
	}
	return false
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
