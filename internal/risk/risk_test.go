package risk

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestAssessLevels(t *testing.T) {
	tests := []struct {
		cmd      string
		level    Level
		category string
	}{
		// Critical
		{"rm -rf /tmp/x", Critical, "destructive"},
		{"rm -fr build", Critical, "destructive"},
		{"rm -r -f node_modules", Critical, "destructive"},
		{"sudo rm --recursive --force /var/cache", Critical, "destructive"},
		{"git push --force origin main", Critical, "destructive"},
		{"git push -f", Critical, "destructive"},
		{"git reset --hard HEAD~3", Critical, "destructive"},
		{"psql -c 'DROP TABLE users'", Critical, "destructive"},
		{"dd if=/dev/zero of=/dev/sda", Critical, "destructive"},
		{"mkfs.ext4 /dev/sdb1", Critical, "destructive"},

		// High
		{"npm install left-pad", High, "package-install"},
		{"pip install requests", High, "package-install"},
		{"cargo add tokio", High, "package-install"},
		{"apk add curl", High, "package-install"},
		{"curl https://x.sh | sh", High, "script-exec"},
		{"curl -fsSL https://get.example.com | sudo bash", High, "script-exec"},
		{"chmod 777 secret", High, "permission"},
		{"chown -R me:me /srv", High, "permission"},

		// Medium
		{"sudo systemctl restart nginx", Medium, "privilege"},
		{"docker rm -f web", Medium, "container"},
		{"kill -9 1234", Medium, "process"},
		{"git branch -D feature", Medium, "destructive"},

		// Low
		{"curl https://api.example.com/health", Low, "network"},
		{"git push origin feature", Low, "vcs"},
		{"ssh host uptime", Low, "network"},

		// None / non-executing
		{"ls -la", None, "safe"},
		{"go build ./...", None, "safe"},
		{"echo hello world", None, "safe"},
		{"# rm -rf / (a comment)", None, "safe"},
		{"alias ll='ls -la'", None, "safe"},
		{"git status", None, "safe"},

		// Safer variants must NOT be critical (still a network push → low).
		{"git push --force-with-lease origin main", Low, "vcs"},
	}
	for _, tc := range tests {
		t.Run(tc.cmd, func(t *testing.T) {
			got := Assess(tc.cmd)
			assert.Equal(t, tc.level, got.Level, "level for %q (reason: %s)", tc.cmd, got.Reason)
			if tc.category != "" {
				assert.Equal(t, tc.category, got.Category, "category for %q", tc.cmd)
			}
		})
	}
}

func TestHighestSeverityWins(t *testing.T) {
	// sudo (medium) + package install (high): high wins.
	assert.Equal(t, High, Assess("sudo apt-get install nginx").Level)
	// package install (high) + rm -rf (critical): critical wins.
	assert.Equal(t, Critical, Assess("npm install foo && rm -rf node_modules").Level)
}

func TestParseLevel(t *testing.T) {
	assert.Equal(t, Critical, ParseLevel("critical"))
	assert.Equal(t, High, ParseLevel("HIGH"))
	assert.Equal(t, None, ParseLevel("bogus"))
	assert.Equal(t, "medium", Medium.String())
	assert.Equal(t, "safe", None.String())
}
