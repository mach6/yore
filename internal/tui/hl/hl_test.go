package hl

import "testing"

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
		cmd, sub string
		want     Kind
	}{
		{"git commit -m fix", "git", Command},
		{"git commit -m fix", "-m", Flag},
		{"git commit -m 'a msg'", "'a msg'", String},
		{"cat /etc/hosts", "cat", Command},
		{"cat /etc/hosts", "/etc/hosts", Path},
		{"echo $HOME", "$HOME", Variable},
		{"echo ${PATH}", "${PATH}", Variable},
		{"ls | grep foo", "|", Operator},
		{"ls | grep foo", "grep", Command},  // command after a pipe
		{"FOO=bar ./run", "./run", Command}, // assignment doesn't take the command slot
		{"docker build -t app:1 .", "docker", Command},
		{"docker build -t app:1 .", "-t", Flag},
	}
	for _, c := range cases {
		got := at(c.cmd, c.sub)
		if got != c.want {
			t.Errorf("Classify(%q) at %q = %v, want %v", c.cmd, c.sub, got, c.want)
		}
	}
}

func TestClassifyLengthMatches(t *testing.T) {
	for _, s := range []string{"", "x", "git status", "a | b && c", "echo \"hi $x\""} {
		if len(Classify(s)) != len(s) {
			t.Errorf("Classify(%q) length %d != %d", s, len(Classify(s)), len(s))
		}
	}
}
