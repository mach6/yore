package redact

import "testing"

// BenchmarkSensitiveClean runs a typical innocent command through the FULL
// rule table. It is the hot path (nearly every recorded command is clean), so
// the target is zero allocations and single-digit microseconds: the literal
// hint pre-scan rejects the command before any regex engine is entered.
func BenchmarkSensitiveClean(b *testing.B) {
	f, _ := New(nil, nil)
	const cmd = `git commit -m "optimize query planner and refresh fixtures"`
	if f.Sensitive(cmd) {
		b.Fatalf("benchmark command should be clean: %q", cmd)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = f.Sensitive(cmd)
	}
}

// BenchmarkSensitiveCleanNearMiss is a harder clean case: the command trips
// several rules' literal hints ("://", "token", "-p", "auth") and so DOES
// enter regex engines, but matches none. Shows the cost when the pre-scan
// cannot short-circuit.
func BenchmarkSensitiveCleanNearMiss(b *testing.B) {
	f, _ := New(nil, nil)
	const cmd = `curl -sS https://api.example.com/v1/tokens -H "x-auth: none" | jq .`
	if f.Sensitive(cmd) {
		b.Fatalf("benchmark command should be clean: %q", cmd)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = f.Sensitive(cmd)
	}
}

// BenchmarkSensitiveHit measures a command that actually matches (a late rule),
// so most hint scans run and one regex fires.
func BenchmarkSensitiveHit(b *testing.B) {
	f, _ := New(nil, nil)
	const cmd = `export DB_PASSWORD=hunter2secret`
	if !f.Sensitive(cmd) {
		b.Fatalf("benchmark command should be sensitive: %q", cmd)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sink = f.Sensitive(cmd)
	}
}

// sink defeats dead-code elimination of the benchmarked calls.
var sink bool
