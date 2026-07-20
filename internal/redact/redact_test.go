package redact

import (
	"regexp"
	"strings"
	"testing"
)

// mustFilter builds a Filter and fails the test on any compile error.
func mustFilter(t *testing.T, userPatterns, ignoreDirs []string) *Filter {
	t.Helper()
	f, errs := New(userPatterns, ignoreDirs)
	if len(errs) != 0 {
		t.Fatalf("unexpected New errors: %v", errs)
	}
	return f
}

// TestTruePositives: each command MUST be flagged, and by the named rule.
// Naming the expected rule guards against a case being caught for the wrong
// reason (e.g. a broad rule masking a bug in the specific one).
func TestTruePositives(t *testing.T) {
	f := mustFilter(t, nil, nil)

	cases := []struct {
		name string
		cmd  string
		want string
	}{
		// AWS.
		{"aws access key export", `export AWS_ACCESS_KEY_ID=AKIAIOSFODNN7EXAMPLE`, "aws-access-key"},
		{"aws temp (ASIA) key", `AWS_ACCESS_KEY_ID=ASIAY34FZKBOKMUTVV7A aws s3 ls`, "aws-access-key"},
		{"aws configure set secret", `aws configure set aws_secret_access_key wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`, "aws-secret"},
		{"aws secret env assign", `AWS_SECRET_ACCESS_KEY=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY aws s3 ls`, "aws-secret"},

		// GitHub tokens, any position.
		{"ghp token in url", `git remote add o https://ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@github.com/x/y`, "github-token"},
		{"ghp token bare", `echo ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345`, "github-token"},
		{"github_pat token", `git clone https://github_pat_11ABCDE0Y0abcdefghijkl_MNOPQRSTUVWXYZ0123456789 .`, "github-token"},

		// Slack. `--token xoxb-` uses a space, so generic-token-assign does
		// NOT fire; slack-token must catch it on shape.
		{"slack xoxb", `slackcat --token xoxb-123456789012-AbCdEfGhIjKlMnOp file.txt`, "slack-token"},

		// Assignment styles.
		{"db password export", `export DB_PASSWORD=hunter2`, "generic-token-assign"},
		{"token assign then chain", `FOO_TOKEN=abc123def && ./run`, "generic-token-assign"},
		{"api-key flag", `./client --api-key=sk-1234567890abcdef call`, "generic-token-assign"},
		{"credentials env (no adjacency)", `DATABASE_CREDENTIALS=s3cr3tvalue ./manage.py runserver`, "env-secret-export"},
		{"secret_key leading assign", `SECRET_KEY=s3cr3tvalue python manage.py runserver`, "env-secret-export"},

		// Per-tool credential flags.
		{"mysql attached -p", `mysql -uroot -ps3cret db`, "mysql-password"},
		{"sshpass -p space form", `sshpass -p hunter2 ssh admin@10.0.0.1`, "sshpass-password"},
		{"curl -u user:pass", `curl -u admin:hunter2 https://api.example.com`, "curl-userpass"},
		{"wget --password", `wget --password=x https://host/f`, "wget-password"},
		{"mongo -p space form", `mongosh -u admin -p s3cret mydb`, "mongo-password"},

		// URL userinfo.
		{"url userinfo", `git clone https://user:pass@host.example.com/path.git`, "url-userinfo"},
		{"url userinfo mid-command", `psql "$(echo postgres://app:s3cret@db:5432/prod)"`, "url-userinfo"},

		// Crypto material.
		{"pem paste", `echo "-----BEGIN OPENSSH PRIVATE KEY-----" >> id_ed25519`, "pem-block"},
		{"pem rsa header", `printf -- '-----BEGIN RSA PRIVATE KEY-----\n' | tee key.pem`, "pem-block"},
		{"jwt paste", `curl -H "X-Auth: eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcDEF" u`, "jwt"},
		{"jwt bearer header", `HEADER="Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"`, "jwt"},

		// openssl / gpg inline passphrases.
		{"openssl passin", `openssl rsa -passin pass:s3cret -in enc.pem -out plain.pem`, "openssl-passin"},
		{"openssl passout", `openssl genrsa -passout pass:s3cret -out key.pem 2048`, "openssl-passin"},
		{"gpg passphrase", `gpg --batch --passphrase hunter2 -c secret.txt`, "gpg-passphrase"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !f.Sensitive(tc.cmd) {
				t.Fatalf("Sensitive=false, want true\n  cmd: %s", tc.cmd)
			}
			if got := f.Reason(tc.cmd); got != tc.want {
				t.Fatalf("Reason=%q, want %q\n  cmd: %s", got, tc.want, tc.cmd)
			}
		})
	}
}

// TestFalsePositives: each command is a real-shape innocent command that MUST
// pass through unflagged. A regression here silently deletes user history.
func TestFalsePositives(t *testing.T) {
	f := mustFilter(t, nil, nil)

	cases := []struct {
		name string
		cmd  string
	}{
		{"mkdir -p", `mkdir -p /tmp/x`},
		{"psql -p is a port", `psql -h db -p 5432 -U app`},
		{"docker compose -p project", `docker compose -p myproj up`},
		{"commit message with password word", `git commit -m "fix password reset email"`},
		{"grep token word", `grep -r token docs/`},
		{"echo secret word", `echo secret santa list`},
		{"git sha not a secret", `git checkout deadbeefcafe1234`},
		{"docker image digest", `docker run sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789 sh`},
		{"tokens path word", `curl https://api.example.com/v1/tokens`},
		{"editor export", `export EDITOR=vim`},
		{"password-store path name", `ls -la ~/.password-store`},
		{"man gpg", `man gpg`},
		{"apt secretstorage pkg", `apt install python3-secretstorage`},

		// mysql -p with a SPACE is a database name (mysql prompts for the
		// password); flagging it would drop legit `mysql -p <db>` history.
		// See the package notes / final report. Deliberately NOT flagged.
		{"mysql -p space is db name", `mysql -u root -p s3cret`},
		// psql/postgres port and passwordless URLs.
		{"redis url with port only", `redis-cli -u redis://localhost:6379/0 ping`},
		{"ssh url user no password", `git clone ssh://git@github.com/x/y.git`},
		{"curl -u user then prompt", `curl -u admin https://api.example.com`},
		{"gpg passphrase-fd not inline", `gpg --batch --passphrase-fd 0 -c secret.txt`},
		{"openssl passin from file", `openssl rsa -passin file:pw.txt -in enc.pem`},
		{"curl user-agent flag", `curl --user-agent mybot https://api.example.com/tokens`},
		{"mysql port capital P attached", `mysql -uroot -P3306 -h db`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if f.Sensitive(tc.cmd) {
				t.Fatalf("Sensitive=true (rule %q), want false\n  cmd: %s", f.Reason(tc.cmd), tc.cmd)
			}
		})
	}
}

func TestReasonEmptyOnClean(t *testing.T) {
	f := mustFilter(t, nil, nil)
	if got := f.Reason("git status"); got != "" {
		t.Fatalf("Reason(clean)=%q, want empty", got)
	}
	if f.Sensitive("") || f.Reason("") != "" {
		t.Fatalf("empty command should be clean")
	}
}

func TestSkipDir(t *testing.T) {
	f := mustFilter(t, nil, []string{"/home/x/secrets", "/opt/vault/"})

	cases := []struct {
		name string
		cwd  string
		want bool
	}{
		{"exact match", "/home/x/secrets", true},
		{"child dir", "/home/x/secrets/sub", true},
		{"deep child", "/home/x/secrets/a/b/c", true},
		{"trailing slash exact", "/home/x/secrets/", true},
		{"trailing slash child", "/home/x/secrets/sub/", true},
		{"non-canonical child", "/home/x/secrets/./sub/../sub", true},
		{"sibling prefix not a segment", "/home/x/secretsandmore", false},
		{"sibling prefix file", "/home/x/secretsandmore/z", false},
		{"unrelated path", "/home/y/work", false},
		{"configured with trailing slash", "/opt/vault/keys", true},
		{"empty cwd", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := f.SkipDir(tc.cwd); got != tc.want {
				t.Fatalf("SkipDir(%q)=%v, want %v", tc.cwd, got, tc.want)
			}
		})
	}
}

// TestSkipDirRoot: a "/" ignore dir is a catch-all that skips every absolute
// path (including "/" itself), exercising the d=="/" boundary guard.
func TestSkipDirRoot(t *testing.T) {
	f := mustFilter(t, nil, []string{"/"})
	for _, cwd := range []string{"/", "/home/y/work", "/opt", "/a/b/c"} {
		if !f.SkipDir(cwd) {
			t.Fatalf("with ignore dir \"/\", SkipDir(%q) should be true", cwd)
		}
	}
	if f.SkipDir("") {
		t.Fatal("empty cwd is never skipped, even under a \"/\" ignore dir")
	}
}

// TestSkipDirNoRoot isolates the segment-boundary logic without the catch-all
// "/" entry so "unrelated path" genuinely means not-skipped.
func TestSkipDirNoRoot(t *testing.T) {
	f := mustFilter(t, nil, []string{"/home/x/secrets"})
	if f.SkipDir("/home/x/secretsandmore") {
		t.Fatal("/home/x/secretsandmore must not be skipped by /home/x/secrets")
	}
	if f.SkipDir("/home/y/work") {
		t.Fatal("unrelated path must not be skipped")
	}
	if !f.SkipDir("/home/x/secrets/deep/nest") {
		t.Fatal("child of ignore dir must be skipped")
	}
	if f.SkipDir("") {
		t.Fatal("empty cwd must not be skipped")
	}
}

func TestNoIgnoreDirs(t *testing.T) {
	f := mustFilter(t, nil, nil)
	if f.SkipDir("/anything/at/all") {
		t.Fatal("with no ignore dirs, nothing is skipped")
	}
}

func TestUserPatternsValid(t *testing.T) {
	f := mustFilter(t, []string{`INTERNAL-[0-9]{6}`, `(?i)my-corp-secret`}, nil)

	if !f.Sensitive("deploy --ticket INTERNAL-004217") {
		t.Fatal("valid user pattern should match")
	}
	if got := f.Reason("deploy --ticket INTERNAL-004217"); got != "user:INTERNAL-[0-9]{6}" {
		t.Fatalf("Reason=%q, want user-pattern name", got)
	}
	if !f.Sensitive("echo MY-CORP-SECRET") {
		t.Fatal("case-insensitive user pattern should match")
	}
	// Built-ins still work alongside user patterns.
	if !f.Sensitive("export DB_PASSWORD=hunter2") {
		t.Fatal("built-ins must still fire with user patterns present")
	}
	// A command matching neither is clean.
	if f.Sensitive("git status") {
		t.Fatal("clean command should not match")
	}
}

func TestUserPatternsInvalidReported(t *testing.T) {
	f, errs := New([]string{`valid[0-9]+`, `(unclosed`, `also[a-`}, nil)
	if len(errs) != 2 {
		t.Fatalf("want 2 errors for 2 bad patterns, got %d: %v", len(errs), errs)
	}
	for _, e := range errs {
		if !strings.Contains(e.Error(), "invalid user pattern") {
			t.Fatalf("error missing context: %v", e)
		}
	}
	// The valid one is still active; the invalid ones are skipped, not fatal.
	if !f.Sensitive("id valid123") {
		t.Fatal("valid pattern should still be compiled after skipping bad ones")
	}
	if f.Sensitive("nothing here") {
		t.Fatal("skipped bad patterns must not match anything")
	}
}

func TestEmptyInputs(t *testing.T) {
	f, errs := New(nil, nil)
	if len(errs) != 0 || f == nil {
		t.Fatalf("New(nil,nil) should succeed: f=%v errs=%v", f, errs)
	}
	// Blank/whitespace patterns and dirs are ignored, not errors.
	f2, errs2 := New([]string{"", "   "}, []string{"", "  "})
	if len(errs2) != 0 {
		t.Fatalf("blank inputs should not error: %v", errs2)
	}
	if len(f2.rules) != len(builtins) {
		t.Fatalf("blank user patterns should add no rules: got %d", len(f2.rules))
	}
	if len(f2.dirs) != 0 {
		t.Fatalf("blank dirs should add no ignore dirs: got %d", len(f2.dirs))
	}
}

// TestBuiltinsCompile is a guard: every built-in name is unique and non-empty,
// and the shared table compiled without panicking (reached here => it did).
func TestBuiltinsCompile(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range builtins {
		if r.name == "" {
			t.Fatal("built-in with empty name")
		}
		if seen[r.name] {
			t.Fatalf("duplicate built-in name %q", r.name)
		}
		seen[r.name] = true
		if r.re == nil {
			t.Fatalf("built-in %q has nil regexp", r.name)
		}
		// Fold rules must carry lowercase hints (containsFold assumes it).
		if r.fold {
			for _, h := range r.hints {
				if h != strings.ToLower(h) {
					t.Fatalf("fold rule %q has non-lowercase hint %q", r.name, h)
				}
			}
		}
	}
}

// TestContainsFold spot-checks the allocation-free case-folded scan.
func TestContainsFold(t *testing.T) {
	cases := []struct {
		s, sub string
		want   bool
	}{
		{"HELLO world", "hello", true},
		{"prefix TOKEN=x", "token", true},
		{"no match here", "secret", false},
		{"edge", "edge", true},
		{"short", "muchlongerneedle", false},
		{"anything", "", true},
		{"MixedCASE", "mixedcase", true},
	}
	for _, tc := range cases {
		if got := containsFold(tc.s, tc.sub); got != tc.want {
			t.Fatalf("containsFold(%q,%q)=%v, want %v", tc.s, tc.sub, got, tc.want)
		}
	}
}

// TestHintGating asserts the pre-scan actually gates: a command with none of a
// rule's hints must not even be considered by that rule. We verify indirectly
// by confirming clean commands that superficially resemble secrets stay clean.
func TestHintGating(t *testing.T) {
	f := mustFilter(t, nil, nil)
	// Contains "AKIA"-ish but wrong shape (too short) -> no match.
	if f.Sensitive("echo AKIA123") {
		t.Fatal("short AKIA-like string must not match aws-access-key")
	}
	// A real regexp sanity: our built-in count is what we expect (catch drift).
	if len(builtins) != 17 {
		t.Fatalf("expected 17 built-in rules, got %d", len(builtins))
	}
}

// Sanity: ensure our patterns are valid Go regexp (defense in depth beyond
// MustCompile at init, in case specs are edited).
func TestSpecsAreValidRegexp(t *testing.T) {
	for _, r := range builtins {
		if _, err := regexp.Compile(r.re.String()); err != nil {
			t.Fatalf("rule %q regexp invalid: %v", r.name, err)
		}
	}
}
