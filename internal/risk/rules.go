package risk

import (
	"regexp"
	"strings"
)

// The rule table. Every entry names a level, a category, and a reason, because
// a verdict nobody can explain is a verdict nobody will trust.
//
// Levels mean something specific, and the meaning is what keeps the ramp
// useful rather than uniformly alarming:
//
//   - critical: irreversible. Data, history, or infrastructure that no undo
//     brings back, plus remote code arriving with root.
//   - high:     reversible only with effort, or it changes what code runs:
//     installs, permissions, executing fetched or local scripts.
//   - medium:   real side effects, ordinarily recoverable: privilege,
//     containers, processes, service and account state.
//   - low:      reaches off the machine, or writes somewhere shared.
//
// Rules are scanned in full and the highest severity wins, so `sudo npm
// install` is high (package-install), not medium (privilege); order within a
// level only settles ties.

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

var (
	// interpreters execute whatever text they are handed, which is why their
	// quoted arguments are re-examined as commands in their own right.
	interpreters = set(
		"sh", "bash", "zsh", "ksh", "dash", "ash", "fish", "eval", "source", ".",
		"python", "python2", "python3", "perl", "ruby", "node", "php", "deno",
		"bun", "Rscript", "osascript", "powershell", "pwsh",
	)
	// shells are the subset that pipe input straight into execution.
	shells = set("sh", "bash", "zsh", "ksh", "dash", "ash", "fish")

	fetchers  = set("curl", "wget", "aria2c", "httpie", "http")
	dbClients = set("psql", "mysql", "mariadb", "sqlite3", "mongo", "mongosh",
		"redis-cli", "clickhouse-client", "cockroach", "duckdb", "pgcli", "mycli")
)

var rules = []rule{
	// --- Critical: irreversible; no undo brings this back ---
	{Critical, "destructive", "recursive force delete", matchRmRF},
	{Critical, "destructive", "force-push rewrites remote history", matchGitPushForce},
	{Critical, "destructive", "deletes a remote branch", matchGitPushDelete},
	{Critical, "destructive", "hard reset discards work", gitFlag("reset", "--hard")},
	{Critical, "destructive", "rewrites every commit in the repo", gitSub("filter-branch")},
	{Critical, "destructive", "expires the reflog, the last way back", gitSub("reflog", "expire")},
	{Critical, "destructive", "drops a database object", sqlRe(`\bdrop\s+(table|database|schema|index|collection)\b`)},
	{Critical, "destructive", "empties a table", sqlRe(`\btruncate\s+(table\b|\w)`)},
	{Critical, "destructive", "unfiltered DELETE or UPDATE", matchSQLNoWhere},
	{Critical, "destructive", "flushes the entire keyspace", sqlRe(`\bflush(all|db)\b`)},
	{Critical, "destructive", "drops a collection", sqlRe(`\.drop\(\s*\)`)},
	{Critical, "destructive", "overwrites a raw disk device", matchDiskWrite},
	{Critical, "destructive", "formats a filesystem", matchMkfs},
	{Critical, "destructive", "erases filesystem signatures", cmd("wipefs")},
	{Critical, "destructive", "shreds files beyond recovery", cmd("shred")},
	{Critical, "infra", "destroys managed infrastructure", matchIaCDestroy},
	{Critical, "infra", "applies infrastructure changes unattended", matchIaCAutoApprove},
	{Critical, "infra", "deletes a whole namespace", matchKubectlNamespace},
	{Critical, "cloud", "deletes cloud resources", matchCloudDelete},
	{Critical, "cloud", "empties a bucket recursively", matchBucketWipe},
	{Critical, "script-exec", "fork bomb", raw(`:\s*\(\s*\)\s*\{.*\|.*&.*\}\s*;`)},
	{Critical, "script-exec", "binds a shell to a socket", matchReverseShell},

	// --- High: costly to undo, or it changes what code runs ---
	{High, "package-install", "installs packages", matchPackageInstall},
	{High, "supply-chain", "publishes an artifact the world can pull", matchPublish},
	{High, "script-exec", "pipes a download straight into a shell", matchPipeToShell},
	{High, "script-exec", "runs code fetched over the network", matchRunFetched},
	{High, "script-exec", "pipes data into a shell", matchPipeIntoShell},
	{High, "script-exec", "executes a local script", matchLocalScript},
	{High, "permission", "grants group- or world-writable access", matchChmodWritable},
	{High, "permission", "sets the setuid or setgid bit", matchChmodSetid},
	{High, "permission", "recursive ownership change", matchChownRecursive},
	{High, "destructive", "force-cleans untracked files", matchGitClean},
	{High, "destructive", "deletes matched files in place", matchFindDelete},
	{High, "destructive", "removes packages", matchPackageRemove},
	{High, "destructive", "removes a container volume", matchVolumeRemove},
	{High, "destructive", "reclaims unreferenced docker state", matchDockerPrune},
	{High, "destructive", "deletes cluster resources", matchKubectlDelete},
	{High, "destructive", "removes a release and its resources", matchHelmRemove},
	{High, "destructive", "rewrites the partition table", matchPartitionEdit},
	{High, "destructive", "deletes every scheduled job", matchCrontabWipe},
	{High, "destructive", "mirrors a deletion to the destination", matchRsyncDelete},
	{High, "script-exec", "runs a package fetched on the spot", matchEphemeralRun},
	{High, "account", "changes who can log in", cmd("useradd", "userdel", "usermod", "adduser", "deluser", "groupadd", "groupdel", "passwd", "chpasswd", "visudo")},
	{High, "secret", "reads private key material", matchReadsSecret},
	{High, "secret", "copies private key material", matchCopiesSecret},
	{High, "secret", "exports secret keys", matchExportsSecret},
	{High, "permission", "changes access control lists", cmd("setfacl", "chacl")},
	{High, "network", "tears down host firewalling", matchFirewallDown},
	{High, "container", "runs a container with host privileges", matchPrivilegedContainer},
	{High, "system", "halts or reboots the machine", matchHaltReboot},

	// --- Medium: real side effects, ordinarily recoverable ---
	{Medium, "privilege", "runs as root", matchSudo},
	{Medium, "privilege", "switches user", cmd("su", "pkexec")},
	{Medium, "container", "removes or kills containers", matchContainerRemove},
	{Medium, "process", "kills processes", cmd("kill", "killall", "pkill")},
	{Medium, "destructive", "discards git state", matchGitDiscard},
	{Medium, "destructive", "rewrites local history", matchGitRebase},
	{Medium, "destructive", "truncates a file", matchTruncate},
	{Medium, "system", "changes service state", matchServiceState},
	{Medium, "system", "edits the schedule", matchScheduleEdit},
	{Medium, "network", "uploads piped data off the machine", matchUpload},
	{Medium, "script-exec", "runs an executable from this directory", matchLocalBinary},
	{Medium, "infra", "applies infrastructure changes", matchIaCApply},

	// --- Low: reaches off the machine, or writes somewhere shared ---
	{Low, "network", "reaches the network", matchNetwork},
	{Low, "vcs", "pushes to a remote", matchGitPush},
	{Low, "secret", "puts a secret in the environment", matchSecretEnv},
}

// --- rule constructors ---

// cmd matches when any acting segment runs one of these commands.
func cmd(names ...string) func(*cmdline) bool {
	return func(c *cmdline) bool { return c.cmd(names...) }
}

// raw matches the command text. Reserved for shapes that are about content
// rather than structure, where a command word would tell you nothing.
func raw(pattern string) func(*cmdline) bool {
	re := regexp.MustCompile(`(?i)` + pattern)
	return func(c *cmdline) bool { return re.MatchString(c.raw) }
}

// gitSub matches a git subcommand.
func gitSub(words ...string) func(*cmdline) bool {
	return func(c *cmdline) bool { return c.sub("git", words...) }
}

// gitFlag matches a git subcommand carrying a long flag.
func gitFlag(word, flag string) func(*cmdline) bool {
	return func(c *cmdline) bool {
		return c.each(func(s *segment) bool { return s.is("git", word) && s.longFlag(flag) })
	}
}

// sqlRe matches SQL, but only where SQL would actually be executed: handed to a
// database client, or typed as the whole line. Otherwise `git commit -m "drop
// table support"` would read as dropping a table.
func sqlRe(pattern string) func(*cmdline) bool {
	re := regexp.MustCompile(`(?i)` + pattern)
	return func(c *cmdline) bool {
		if !re.MatchString(c.raw) {
			return false
		}
		return c.each(func(s *segment) bool { return dbClients[s.head] }) || looksLikeSQL(c.raw)
	}
}

var sqlLead = regexp.MustCompile(`(?is)^\s*(drop|truncate|delete\s+from|update|flushall|flushdb)\b`)

func looksLikeSQL(s string) bool { return sqlLead.MatchString(s) }

// --- destructive ---

// matchRmRF flags an rm that is both recursive and forced, in either order and
// whether combined or split. The flags must be the rm's own: a `-r` belonging
// to a grep three words away does not make a deletion.
func matchRmRF(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "rm" {
			return false
		}
		recursive := s.shortFlag('r') || s.shortFlag('R') || s.longFlag("--recursive")
		force := s.shortFlag('f') || s.longFlag("--force")
		return recursive && force
	})
}

// matchGitPushForce flags a force push but spares --force-with-lease, which
// refuses to clobber work it has not seen: the safe form must not be rated the
// most dangerous thing on the list.
func matchGitPushForce(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if !s.is("git", "push") || s.longFlag("--force-with-lease") || s.longFlag("--force-if-includes") {
			return false
		}
		return s.longFlag("--force") || s.longFlag("--mirror") || s.shortFlag('f')
	})
}

// matchGitPushDelete flags remote branch deletion, in both spellings: the
// explicit --delete and the older colon refspec.
func matchGitPushDelete(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if !s.is("git", "push") {
			return false
		}
		if s.longFlag("--delete") || s.shortFlag('d') {
			return true
		}
		for _, w := range s.words[1:] {
			if strings.HasPrefix(w, ":") && len(w) > 1 {
				return true
			}
		}
		return false
	})
}

func matchGitPush(c *cmdline) bool { return c.sub("git", "push") }

func matchGitClean(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		return s.is("git", "clean") && (s.shortFlag('f') || s.longFlag("--force"))
	})
}

// matchGitDiscard covers the everyday ways working state disappears.
func matchGitDiscard(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch {
		case s.is("git", "reset"), s.is("git", "restore"), s.is("git", "stash", "drop"),
			s.is("git", "stash", "clear"), s.is("git", "tag", "-d"):
			return true
		case s.is("git", "checkout") && s.hasArg("--"):
			return true
		case s.is("git", "branch") && (s.shortFlag('d') || s.shortFlag('D')):
			return true
		case s.is("git", "tag") && (s.shortFlag('d') || s.longFlag("--delete")):
			return true
		case s.is("git", "submodule", "deinit"), s.is("git", "gc"), s.is("git", "prune"):
			return true
		}
		return false
	})
}

func matchGitRebase(c *cmdline) bool {
	return c.sub("git", "rebase") || c.sub("git", "cherry-pick") || c.sub("git", "commit", "--amend")
}

// matchDiskWrite flags writes that go past the filesystem to the device.
func matchDiskWrite(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		for _, r := range s.redirs {
			if isDevice(r) {
				return true
			}
		}
		if s.head != "dd" {
			return false
		}
		for _, a := range s.args {
			if strings.HasPrefix(a.val, "of=") && isDevice(strings.TrimPrefix(a.val, "of=")) {
				return true
			}
		}
		return false
	})
}

var devicePath = regexp.MustCompile(`^/dev/(sd|nvme|disk|hd|vd|mmcblk|xvd)`)

func isDevice(p string) bool { return devicePath.MatchString(p) }

// matchMkfs catches both spellings; `mkfs.ext4` and `mkfs -t ext4`.
func matchMkfs(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		return s.head == "mkfs" || strings.HasPrefix(s.head, "mkfs.") || s.head == "mke2fs" || s.head == "newfs"
	})
}

var (
	sqlMutate = regexp.MustCompile(`(?i)\b(delete\s+from\s+\S+|update\s+\S+\s+set\b)`)
	sqlWhere  = regexp.MustCompile(`(?i)\bwhere\b`)
)

// matchSQLNoWhere flags a DELETE or UPDATE carrying no WHERE: the shape that
// takes every row instead of the intended few. RE2 has no negative lookahead,
// so the absence is checked in code rather than in the pattern.
func matchSQLNoWhere(c *cmdline) bool {
	if !sqlMutate.MatchString(c.raw) || sqlWhere.MatchString(c.raw) {
		return false
	}
	return c.each(func(s *segment) bool { return dbClients[s.head] }) || looksLikeSQL(c.raw)
}

func matchFindDelete(c *cmdline) bool {
	return c.each(func(s *segment) bool { return s.head == "find" && s.longFlag("-delete") })
}

// matchTruncate flags emptying a file: `truncate -s 0`, or a bare `> file`
// redirect with no command in front of it to produce the content.
func matchTruncate(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head == "truncate" {
			return true
		}
		return s.head == "" && len(s.redirs) > 0
	})
}

// --- infrastructure and cloud ---

func matchIaCDestroy(c *cmdline) bool {
	return c.sub("terraform", "destroy") || c.sub("tofu", "destroy") ||
		c.sub("pulumi", "destroy") || c.sub("terragrunt", "destroy") ||
		c.sub("cdk", "destroy") || c.sub("serverless", "remove")
}

func matchIaCAutoApprove(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "terraform", "tofu", "terragrunt", "pulumi":
			return s.longFlag("--auto-approve") || s.longFlag("-auto-approve") || s.shortFlag('y')
		}
		return false
	})
}

func matchIaCApply(c *cmdline) bool {
	return c.sub("terraform", "apply") || c.sub("tofu", "apply") ||
		c.sub("pulumi", "up") || c.sub("terragrunt", "apply") ||
		c.sub("kubectl", "apply") || c.sub("helm", "upgrade") || c.sub("helm", "install") ||
		c.sub("ansible-playbook")
}

// matchKubectlNamespace separates deleting a namespace (which takes everything
// inside it) from deleting one resource.
func matchKubectlNamespace(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if !s.is("kubectl", "delete") && !s.is("oc", "delete") {
			return false
		}
		for _, w := range s.words[1:] {
			switch w {
			case "namespace", "namespaces", "ns", "all", "pvc", "crd":
				return true
			}
		}
		return s.longFlag("--all") || s.longFlag("--all-namespaces")
	})
}

func matchKubectlDelete(c *cmdline) bool {
	return c.sub("kubectl", "delete") || c.sub("oc", "delete") ||
		c.sub("kubectl", "drain") || c.sub("kubectl", "cordon")
}

func matchHelmRemove(c *cmdline) bool {
	return c.sub("helm", "uninstall") || c.sub("helm", "delete") || c.sub("helm", "rollback")
}

// cloudDelete are the verbs each provider's CLI spells destruction with.
var cloudDelete = set("delete", "terminate-instances", "rb", "destroy", "remove",
	"delete-user", "delete-bucket", "deregister", "purge")

func matchCloudDelete(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "aws", "gcloud", "az", "doctl", "flyctl", "fly", "heroku", "gsutil", "s3cmd":
		default:
			return false
		}
		for _, w := range s.words {
			if cloudDelete[w] || strings.HasPrefix(w, "delete-") || strings.HasPrefix(w, "terminate-") {
				return true
			}
		}
		return false
	})
}

// matchBucketWipe flags recursive object-store deletion, which empties a
// prefix rather than removing one key.
func matchBucketWipe(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "aws", "gsutil", "s3cmd", "rclone", "mc":
		default:
			return false
		}
		del := false
		for _, w := range s.words {
			if w == "rm" || w == "rb" || w == "delete" || w == "purge" {
				del = true
			}
		}
		return del && (s.longFlag("--recursive") || s.shortFlag('r') || s.longFlag("--force"))
	})
}

// --- containers ---

func matchContainerRemove(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "docker", "podman", "nerdctl", "docker-compose":
		default:
			return false
		}
		for _, w := range s.words {
			switch w {
			case "rm", "kill", "stop", "prune", "down":
				return true
			}
		}
		return false
	})
}

func matchDockerPrune(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "docker", "podman", "nerdctl":
		default:
			return false
		}
		for _, w := range s.words {
			if w == "prune" {
				return true
			}
		}
		return false
	})
}

func matchVolumeRemove(c *cmdline) bool {
	return c.sub("docker", "volume", "rm") || c.sub("podman", "volume", "rm") ||
		c.sub("docker", "volume", "prune") || c.sub("docker", "compose", "down")
}

// matchPrivilegedContainer flags a container handed the host: --privileged, the
// host namespaces, or the root filesystem mounted in.
func matchPrivilegedContainer(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "docker" && s.head != "podman" && s.head != "nerdctl" {
			return false
		}
		if !s.is(s.head, "run") && !s.is(s.head, "create") && !s.is(s.head, "exec") {
			return false
		}
		if s.longFlag("--privileged") || s.longFlag("--pid") || s.longFlag("--net") ||
			s.longFlag("--network") && s.hasArg("host") {
			return true
		}
		for _, a := range s.args {
			if strings.HasPrefix(a.val, "/:/") || a.val == "/:/host" ||
				strings.HasPrefix(a.val, "/var/run/docker.sock") {
				return true
			}
		}
		return false
	})
}

// --- packages and supply chain ---

var pkgInstall = map[string][]string{
	"npm":      {"install", "i", "add", "ci"},
	"yarn":     {"add", "install"},
	"pnpm":     {"add", "install", "i"},
	"bun":      {"add", "install"},
	"pip":      {"install"},
	"pip3":     {"install"},
	"uv":       {"add", "sync"},
	"poetry":   {"add", "install"},
	"cargo":    {"add", "install"},
	"go":       {"install", "get"},
	"gem":      {"install"},
	"brew":     {"install"},
	"apt":      {"install"},
	"apt-get":  {"install"},
	"dnf":      {"install"},
	"yum":      {"install"},
	"apk":      {"add"},
	"zypper":   {"install"},
	"nix-env":  {"-i"},
	"composer": {"require", "install"},
}

func matchPackageInstall(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head == "uv" && len(s.words) >= 2 && s.words[0] == "pip" && s.words[1] == "install" {
			return true
		}
		if s.head == "pacman" && (s.shortFlag('S') || s.longFlag("--sync")) {
			return true
		}
		for _, w := range pkgInstall[s.head] {
			if len(s.words) > 0 && s.words[0] == w {
				return true
			}
		}
		return false
	})
}

var pkgRemove = map[string][]string{
	"npm":     {"uninstall", "remove", "rm"},
	"yarn":    {"remove"},
	"pnpm":    {"remove"},
	"pip":     {"uninstall"},
	"pip3":    {"uninstall"},
	"uv":      {"remove"},
	"cargo":   {"uninstall"},
	"gem":     {"uninstall"},
	"brew":    {"uninstall", "remove"},
	"apt":     {"remove", "purge", "autoremove"},
	"apt-get": {"remove", "purge", "autoremove"},
	"dnf":     {"remove", "erase"},
	"yum":     {"remove", "erase"},
	"apk":     {"del"},
	"zypper":  {"remove"},
}

func matchPackageRemove(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head == "pacman" && (s.shortFlag('R') || s.longFlag("--remove")) {
			return true
		}
		for _, w := range pkgRemove[s.head] {
			if len(s.words) > 0 && s.words[0] == w {
				return true
			}
		}
		return false
	})
}

// matchPublish flags pushing an artifact somewhere the world can pull it;
// outward-facing and, for most registries, unpublishable afterwards.
func matchPublish(c *cmdline) bool {
	return c.sub("npm", "publish") || c.sub("yarn", "publish") || c.sub("pnpm", "publish") ||
		c.sub("cargo", "publish") || c.sub("gem", "push") || c.sub("twine", "upload") ||
		c.sub("docker", "push") || c.sub("podman", "push") || c.sub("helm", "push") ||
		c.sub("poetry", "publish") || c.sub("uv", "publish") || c.sub("goreleaser", "release")
}

// --- executing code ---

func matchPipeToShell(c *cmdline) bool {
	if !c.cmd(keys(fetchers)...) {
		return false
	}
	return c.each(func(s *segment) bool { return s.piped && interpreters[s.head] })
}

// matchRunFetched covers the shapes that fetch and execute without a pipe:
// `bash <(curl …)`, `eval "$(curl …)"`.
func matchRunFetched(c *cmdline) bool {
	return c.cmd(keys(fetchers)...) && c.cmd(keys(interpreters)...)
}

// matchPipeIntoShell flags anything piped into a shell, fetched or not: a
// decoded payload executes exactly as readily as a downloaded one.
func matchPipeIntoShell(c *cmdline) bool {
	return c.each(func(s *segment) bool { return s.piped && shells[s.head] })
}

var scriptExt = regexp.MustCompile(`\.(sh|bash|zsh|ksh|py|rb|pl|ps1)$`)

// matchLocalScript flags running a script from disk: as the command itself, or
// handed to an interpreter or a dot-source.
func matchLocalScript(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if scriptExt.MatchString(s.headRaw) {
			return true
		}
		if !interpreters[s.head] {
			return false
		}
		for _, w := range s.words {
			if scriptExt.MatchString(w) {
				return true
			}
		}
		return false
	})
}

// matchLocalBinary flags executing something out of the working directory that
// is not a recognised script; `./configure`, `./installer`.
func matchLocalBinary(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		return strings.HasPrefix(s.headRaw, "./") || strings.HasPrefix(s.headRaw, "../")
	})
}

// matchReverseShell flags netcat wired to a shell in either direction.
func matchReverseShell(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "nc", "ncat", "netcat", "socat":
		default:
			return false
		}
		if s.longFlag("-e") || s.longFlag("-c") {
			return true
		}
		for _, a := range s.args {
			if strings.Contains(a.val, "/bin/sh") || strings.Contains(a.val, "/bin/bash") ||
				strings.HasPrefix(a.val, "EXEC:") {
				return true
			}
		}
		return false
	})
}

// --- permissions, accounts, secrets ---

var symbolicMode = regexp.MustCompile(`^([ugoa]*)([-+=])([rwxXstugo]*)$`)

// matchChmodWritable flags a mode that opens write access to the group or to
// everyone. Only the group and other digits count: `chmod 644` and `chmod 755`
// are the two most common chmods there are, and neither grants anybody a write
// bit they did not already have.
func matchChmodWritable(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "chmod" || len(s.words) == 0 {
			return false
		}
		mode := s.words[0]
		if octal, ok := parseOctal(mode); ok {
			return octal.group&2 != 0 || octal.other&2 != 0
		}
		m := symbolicMode.FindStringSubmatch(mode)
		if len(m) < 4 || m[2] == "-" || !strings.Contains(m[3], "w") {
			return false
		}
		who := m[1]
		return who == "" || strings.ContainsAny(who, "goa")
	})
}

// matchChmodSetid flags setuid/setgid, which lets a file run as its owner.
func matchChmodSetid(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "chmod" || len(s.words) == 0 {
			return false
		}
		mode := s.words[0]
		if octal, ok := parseOctal(mode); ok {
			return octal.special&6 != 0
		}
		m := symbolicMode.FindStringSubmatch(mode)
		return len(m) >= 4 && m[2] != "-" && strings.Contains(m[3], "s")
	})
}

type octalMode struct{ special, owner, group, other int }

// parseOctal reads a 3- or 4-digit numeric mode, keeping the special bits
// separate so setuid is not mistaken for an owner permission.
func parseOctal(s string) (octalMode, bool) {
	if len(s) < 3 || len(s) > 4 {
		return octalMode{}, false
	}
	d := make([]int, 0, 4)
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '7' {
			return octalMode{}, false
		}
		d = append(d, int(s[i]-'0'))
	}
	if len(d) == 3 {
		return octalMode{0, d[0], d[1], d[2]}, true
	}
	return octalMode{d[0], d[1], d[2], d[3]}, true
}

func matchChownRecursive(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "chown" && s.head != "chgrp" {
			return false
		}
		return s.shortFlag('R') || s.longFlag("--recursive")
	})
}

// matchPartitionEdit flags rewriting a disk's partition table, but not the
// listing flags every one of these tools also answers to.
func matchPartitionEdit(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "fdisk", "parted", "sgdisk", "gdisk", "cfdisk", "sfdisk":
		default:
			return false
		}
		return !s.shortFlag('l') && !s.longFlag("--list") && !s.longFlag("--print")
	})
}

// matchRsyncDelete flags the flag that makes rsync remove files at the far end
// to match the source: a mirror, not a copy.
func matchRsyncDelete(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if s.head != "rsync" {
			return false
		}
		for _, a := range s.args {
			if !a.quoted && strings.HasPrefix(a.val, "--delete") {
				return true
			}
		}
		return false
	})
}

// matchEphemeralRun flags the runners that fetch a package and execute it in
// one step, with nothing written down about what version ran.
func matchEphemeralRun(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "npx", "uvx", "bunx", "pnpx", "dlx":
			return len(s.words) > 0
		case "pipx", "pnpm", "yarn":
			return len(s.words) > 0 && (s.words[0] == "run" || s.words[0] == "dlx")
		case "go":
			return len(s.words) > 0 && s.words[0] == "run" && s.argPrefix("github.com/")
		}
		return false
	})
}

// matchScheduleEdit flags changing what runs unattended, but not reading it.
func matchScheduleEdit(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "at", "batch":
			return true
		case "crontab", "systemd-run":
			return !s.shortFlag('l') && !s.longFlag("--list")
		}
		return false
	})
}

// matchUpload flags piping local output into a request body: the shape data
// leaves a machine in.
func matchUpload(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		if !s.piped || !fetchers[s.head] {
			return false
		}
		for _, f := range []string{"-d", "--data", "--data-binary", "--data-raw",
			"-F", "--form", "-T", "--upload-file", "--post-file", "--post-data"} {
			if s.longFlag(f) {
				return true
			}
		}
		return false
	})
}

var secretPath = regexp.MustCompile(`(?i)(/etc/(shadow|sudoers)|\.ssh/id_[a-z0-9]+$|\.ssh/id_[a-z0-9]+\s|\.pem$|\.p12$|\.aws/credentials|\.kube/config|\.netrc|\.npmrc|id_rsa|id_ed25519)`)

// readers only print a file. Reading a private key is worth saying out loud,
// but only when reading is what is happening: `cat /etc/passwd` names the same
// directory as `cat /etc/shadow` and is nobody's business.
var readers = set("cat", "less", "more", "head", "tail", "bat", "strings", "xxd", "od")

// copiers move a file somewhere else, which for key material is the more
// interesting verb: it is how a secret ends up on a second machine.
var copiers = set("cp", "scp", "rsync", "tar", "zip", "install", "sftp")

func matchReadsSecret(c *cmdline) bool { return touchesSecret(c, readers) }

func matchCopiesSecret(c *cmdline) bool { return touchesSecret(c, copiers) }

func touchesSecret(c *cmdline, verbs map[string]bool) bool {
	return c.each(func(s *segment) bool {
		if !verbs[s.head] {
			return false
		}
		for _, w := range s.words {
			if secretPath.MatchString(w) {
				return true
			}
		}
		return false
	})
}

func matchExportsSecret(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "gpg":
			return s.longFlag("--export-secret-keys") || s.longFlag("--export-secret-subkeys")
		case "security":
			return len(s.words) > 0 && s.words[0] == "dump-keychain"
		}
		return false
	})
}

var secretName = regexp.MustCompile(`(?i)^[A-Z0-9_]*(SECRET|TOKEN|PASSWORD|PASSWD|API_?KEY|PRIVATE_?KEY|ACCESS_?KEY|CREDENTIAL)[A-Z0-9_]*=`)

// matchSecretEnv notes a credential typed into the environment, where it lands
// in history and in every child process.
func matchSecretEnv(c *cmdline) bool {
	for _, s := range c.segs {
		for _, t := range s.all {
			if !t.quoted && secretName.MatchString(t.val) {
				return true
			}
		}
		if s.head == "export" {
			for _, w := range s.words {
				if secretName.MatchString(w) {
					return true
				}
			}
		}
	}
	return false
}

// --- system state ---

func matchSudo(c *cmdline) bool {
	for _, s := range c.segs {
		if s.sudo && !s.inert {
			return true
		}
	}
	return false
}

func matchHaltReboot(c *cmdline) bool {
	if c.cmd("shutdown", "reboot", "halt", "poweroff") {
		return true
	}
	return c.each(func(s *segment) bool {
		if s.head == "init" || s.head == "telinit" {
			return len(s.words) > 0 && (s.words[0] == "0" || s.words[0] == "6")
		}
		return s.head == "systemctl" && len(s.words) > 0 &&
			(s.words[0] == "reboot" || s.words[0] == "poweroff" || s.words[0] == "halt")
	})
}

func matchServiceState(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "systemctl", "service", "launchctl", "rc-service", "supervisorctl":
		default:
			return false
		}
		for _, w := range s.words {
			switch w {
			case "stop", "disable", "mask", "restart", "unload", "kill":
				return true
			}
		}
		return false
	})
}

func matchFirewallDown(c *cmdline) bool {
	return c.each(func(s *segment) bool {
		switch s.head {
		case "iptables", "ip6tables", "nft":
			return s.shortFlag('F') || s.shortFlag('X') || s.longFlag("--flush") ||
				(len(s.words) > 0 && s.words[0] == "flush")
		case "ufw":
			return len(s.words) > 0 && (s.words[0] == "disable" || s.words[0] == "reset")
		case "pfctl":
			return s.shortFlag('d') || s.shortFlag('F')
		case "setenforce":
			return len(s.words) > 0 && (s.words[0] == "0" || s.words[0] == "Permissive")
		}
		return false
	})
}

// matchCrontabWipe flags `crontab -r`, which deletes every scheduled job with
// no confirmation and no copy.
func matchCrontabWipe(c *cmdline) bool {
	return c.each(func(s *segment) bool { return s.head == "crontab" && s.shortFlag('r') })
}

// matchNetwork is the floor: this command talks to another machine.
func matchNetwork(c *cmdline) bool {
	return c.cmd("curl", "wget", "ssh", "scp", "rsync", "sftp", "ftp", "nc", "ncat",
		"telnet", "aria2c", "http", "httpie", "mosh")
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
