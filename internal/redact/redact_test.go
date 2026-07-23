package redact

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// mustFilter builds a Filter and fails the test on any compile error.
func mustFilter(t *testing.T, userPatterns, ignoreDirs []string) *Filter {
	t.Helper()
	f, errs := New(userPatterns, ignoreDirs)
	require.Empty(t, errs, "unexpected New errors")
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
			require.True(t, f.Sensitive(tc.cmd), "cmd: %s", tc.cmd)
			require.Equal(t, tc.want, f.Reason(tc.cmd), "cmd: %s", tc.cmd)
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
			require.False(t, f.Sensitive(tc.cmd), "cmd: %s (rule %q)", tc.cmd, f.Reason(tc.cmd))
		})
	}
}

func TestReasonEmptyOnClean(t *testing.T) {
	f := mustFilter(t, nil, nil)
	require.Empty(t, f.Reason("git status"), "Reason(clean)")
	require.False(t, f.Sensitive(""), "empty command should be clean")
	require.Empty(t, f.Reason(""), "empty command should be clean")
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
			require.Equal(t, tc.want, f.SkipDir(tc.cwd))
		})
	}
}

// TestSkipDirRoot: a "/" ignore dir is a catch-all that skips every absolute
// path (including "/" itself), exercising the d=="/" boundary guard.
func TestSkipDirRoot(t *testing.T) {
	f := mustFilter(t, nil, []string{"/"})
	for _, cwd := range []string{"/", "/home/y/work", "/opt", "/a/b/c"} {
		t.Run(cwd, func(t *testing.T) {
			require.True(t, f.SkipDir(cwd), "with ignore dir \"/\", SkipDir should be true")
		})
	}
	require.False(t, f.SkipDir(""), "empty cwd is never skipped, even under a \"/\" ignore dir")
}

// TestSkipDirNoRoot isolates the segment-boundary logic without the catch-all
// "/" entry so "unrelated path" genuinely means not-skipped.
func TestSkipDirNoRoot(t *testing.T) {
	f := mustFilter(t, nil, []string{"/home/x/secrets"})
	require.False(t, f.SkipDir("/home/x/secretsandmore"), "/home/x/secretsandmore must not be skipped by /home/x/secrets")
	require.False(t, f.SkipDir("/home/y/work"), "unrelated path must not be skipped")
	require.True(t, f.SkipDir("/home/x/secrets/deep/nest"), "child of ignore dir must be skipped")
	require.False(t, f.SkipDir(""), "empty cwd must not be skipped")
}

func TestNoIgnoreDirs(t *testing.T) {
	f := mustFilter(t, nil, nil)
	require.False(t, f.SkipDir("/anything/at/all"), "with no ignore dirs, nothing is skipped")
}

func TestUserPatternsValid(t *testing.T) {
	f := mustFilter(t, []string{`INTERNAL-[0-9]{6}`, `(?i)my-corp-secret`}, nil)

	require.True(t, f.Sensitive("deploy --token INTERNAL-004217"), "valid user pattern should match")
	require.Equal(t, "user:INTERNAL-[0-9]{6}", f.Reason("deploy --token INTERNAL-004217"))
	require.True(t, f.Sensitive("echo MY-CORP-SECRET"), "case-insensitive user pattern should match")
	// Built-ins still work alongside user patterns.
	require.True(t, f.Sensitive("export DB_PASSWORD=hunter2"), "built-ins must still fire with user patterns present")
	// A command matching neither is clean.
	require.False(t, f.Sensitive("git status"), "clean command should not match")
}

func TestUserPatternsInvalidReported(t *testing.T) {
	f, errs := New([]string{`valid[0-9]+`, `(unclosed`, `also[a-`}, nil)
	require.Len(t, errs, 2, "want 2 errors for 2 bad patterns")
	for _, e := range errs {
		require.Contains(t, e.Error(), "invalid user pattern", "error missing context: %v", e)
	}
	// The valid one is still active; the invalid ones are skipped, not fatal.
	require.True(t, f.Sensitive("id valid123"), "valid pattern should still be compiled after skipping bad ones")
	require.False(t, f.Sensitive("nothing here"), "skipped bad patterns must not match anything")
}

func TestEmptyInputs(t *testing.T) {
	f, errs := New(nil, nil)
	require.Empty(t, errs, "New(nil,nil) should succeed")
	require.NotNil(t, f, "New(nil,nil) should succeed")

	// Blank/whitespace patterns and dirs are ignored, not errors.
	f2, errs2 := New([]string{"", "   "}, []string{"", "  "})
	require.Empty(t, errs2, "blank inputs should not error")
	require.Len(t, f2.rules, len(builtins), "blank user patterns should add no rules")
	require.Empty(t, f2.dirs, "blank dirs should add no ignore dirs")
}

// TestBuiltinsCompile is a guard: every built-in name is unique and non-empty,
// and the shared table compiled without panicking (reached here => it did).
func TestBuiltinsCompile(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range builtins {
		require.NotEmpty(t, r.name, "built-in with empty name")
		require.False(t, seen[r.name], "duplicate built-in name %q", r.name)
		seen[r.name] = true
		require.NotNil(t, r.re, "built-in %q has nil regexp", r.name)
		// Fold rules must carry lowercase hints (containsFold assumes it).
		if r.fold {
			for _, h := range r.hints {
				require.Equal(t, strings.ToLower(h), h, "fold rule %q has non-lowercase hint %q", r.name, h)
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
		t.Run(tc.s+"/"+tc.sub, func(t *testing.T) {
			require.Equal(t, tc.want, containsFold(tc.s, tc.sub))
		})
	}
}

// TestHintGating asserts the pre-scan actually gates: a command with none of a
// rule's hints must not even be considered by that rule. We verify indirectly
// by confirming clean commands that superficially resemble secrets stay clean.
func TestHintGating(t *testing.T) {
	f := mustFilter(t, nil, nil)
	// Contains "AKIA"-ish but wrong shape (too short) -> no match.
	require.False(t, f.Sensitive("echo AKIA123"), "short AKIA-like string must not match aws-access-key")
	// A real regexp sanity: our built-in count is what we expect (catch drift).
	require.Len(t, builtins, 17, "expected 17 built-in rules")
}

// Sanity: ensure our patterns are valid Go regexp (defense in depth beyond
// MustCompile at init, in case specs are edited).
func TestSpecsAreValidRegexp(t *testing.T) {
	for _, r := range builtins {
		_, err := regexp.Compile(r.re.String())
		require.NoError(t, err, "rule %q regexp invalid", r.name)
	}
}
