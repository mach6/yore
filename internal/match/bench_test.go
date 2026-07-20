package match

import (
	"fmt"
	"math/rand"
	"testing"
)

// synthCommands builds n deterministic, realistic-looking shell commands
// (a git/docker/ssh/paths mixture) for tests and benchmarks.
func synthCommands(n int) []string {
	rng := rand.New(rand.NewSource(1))
	words := []string{
		"api", "auth", "cache", "core", "db", "gateway", "index", "job",
		"kafka", "logger", "metrics", "nginx", "orders", "payments", "queue",
		"redis", "search", "tokens", "user", "vault", "web", "worker",
	}
	users := []string{"deploy", "root", "dev", "ci", "svc", "admin"}
	tags := []string{"latest", "v1.2.3", "v2.0.0", "edge", "stable", "dev"}
	w := func() string { return words[rng.Intn(len(words))] }

	templates := []func() string{
		func() string { return fmt.Sprintf("git commit -m \"fix %s in %s\"", w(), w()) },
		func() string { return fmt.Sprintf("git checkout -b feature/%s-%s", w(), w()) },
		func() string { return fmt.Sprintf("git rebase -i origin/main # %s", w()) },
		func() string { return fmt.Sprintf("git log --oneline -- %s/%s.go", w(), w()) },
		func() string {
			return fmt.Sprintf("docker run -it --rm %s/%s:%s bash", w(), w(), tags[rng.Intn(len(tags))])
		},
		func() string { return fmt.Sprintf("docker build -t %s/%s:%s .", w(), w(), tags[rng.Intn(len(tags))]) },
		func() string { return fmt.Sprintf("docker compose -f %s/docker-compose.yml up -d", w()) },
		func() string {
			return fmt.Sprintf("ssh %s@%s-%02d.prod.example.com", users[rng.Intn(len(users))], w(), rng.Intn(20))
		},
		func() string {
			return fmt.Sprintf("scp ./%s/%s.tar.gz %s@host:/srv/%s", w(), w(), users[rng.Intn(len(users))], w())
		},
		func() string { return fmt.Sprintf("cd /home/%s/projects/%s/%s", users[rng.Intn(len(users))], w(), w()) },
		func() string { return fmt.Sprintf("kubectl get pods -n %s -l app=%s", w(), w()) },
		func() string { return fmt.Sprintf("kubectl logs -f deploy/%s -n %s", w(), w()) },
		func() string { return fmt.Sprintf("curl -sS https://%s.example.com/api/%s | jq .", w(), w()) },
		func() string { return fmt.Sprintf("grep -rn %s ./internal/%s", w(), w()) },
		func() string { return fmt.Sprintf("go test ./internal/%s/... -run %s", w(), w()) },
		func() string { return fmt.Sprintf("vim /etc/%s/%s.conf", w(), w()) },
	}

	cmds := make([]string, n)
	for i := range cmds {
		cmds[i] = templates[rng.Intn(len(templates))]()
	}
	return cmds
}

const benchN = 200_000

func BenchmarkFilterCold(b *testing.B) {
	cmds := synthCommands(benchN)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := NewFilter()
		f.Apply("git commit", cmds) // no prior query: full scan of 200k rows
	}
}

func BenchmarkFilterIncremental(b *testing.B) {
	cmds := synthCommands(benchN)
	f := NewFilter()
	f.Apply("git", cmds) // warm the match set (untimed full scan)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Extend the query by one char (a trailing space). "git" is still
		// the only term, so the corpus length is unchanged and Apply
		// re-tests only the previously matched "git" rows (the incremental
		// optimization) instead of all 200k. Stable across iterations.
		f.Apply("git ", cmds)
	}
}
