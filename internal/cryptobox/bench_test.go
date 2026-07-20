package cryptobox

import "testing"

// These two operations dominate a deep-history search: opening an epoch's DEK
// once, then opening every record sealed under it. Keep them allocation-lean.

func BenchmarkUnwrapDEK(b *testing.B) {
	hk, _ := NewHistoryKey()
	keyID, dek, _ := NewDEK()
	blob, err := WrapDEK(dek, hk, keyID, "dev-1", 1700000000000, 1)
	if err != nil {
		b.Fatalf("WrapDEK: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := UnwrapDEK(blob, hk, keyID, "dev-1", 1700000000000, 1); err != nil {
			b.Fatalf("UnwrapDEK: %v", err)
		}
	}
}

func BenchmarkOpenRecord(b *testing.B) {
	_, dek, _ := NewDEK()
	aad := RecordAAD{RecordID: "01H...REC", HostID: "01H...HOST", Seq: 12345, KeyID: "01H...KEY"}
	// A record roughly the size of a typical command's JSON encoding.
	pt := []byte(`{"id":"01H...","cmd":"go test ./internal/cryptobox/...","cwd":"/home/dev/yore","exit":0}`)
	blob, err := SealRecord(pt, dek, aad)
	if err != nil {
		b.Fatalf("SealRecord: %v", err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := OpenRecord(blob, dek, aad); err != nil {
			b.Fatalf("OpenRecord: %v", err)
		}
	}
}
