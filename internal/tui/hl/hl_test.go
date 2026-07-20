package hl

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// at returns the Kind classified at the first index of sub within s.
func at(s, sub string) Kind {
	i := indexOf(s, sub)
	if i < 0 {
		return Normal
	}
	return Classify(s)[i]
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestClassify(t *testing.T) {
	cases := []struct {
		name     string
		cmd, sub string
		want     Kind
	}{
		{"command", "git commit -m fix", "git", Command},
		{"flag", "git commit -m fix", "-m", Flag},
		{"quoted string", "git commit -m 'a msg'", "'a msg'", String},
		{"cat command", "cat /etc/hosts", "cat", Command},
		{"path", "cat /etc/hosts", "/etc/hosts", Path},
		{"variable", "echo $HOME", "$HOME", Variable},
		{"braced variable", "echo ${PATH}", "${PATH}", Variable},
		{"pipe operator", "ls | grep foo", "|", Operator},
		{"command after a pipe", "ls | grep foo", "grep", Command},
		{"assignment doesn't take the command slot", "FOO=bar ./run", "./run", Command},
		{"docker command", "docker build -t app:1 .", "docker", Command},
		{"docker flag", "docker build -t app:1 .", "-t", Flag},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.Equal(t, c.want, at(c.cmd, c.sub))
		})
	}
}

func TestClassifyLengthMatches(t *testing.T) {
	for _, s := range []string{"", "x", "git status", "a | b && c", "echo \"hi $x\""} {
		t.Run(fmt.Sprintf("%q", s), func(t *testing.T) {
			require.Len(t, Classify(s), len(s))
		})
	}
}
