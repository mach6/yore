package daemon

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/mach6/yore/internal/rec"
)

// remoteRecords builds n commands for a host with ascending seq, as a pull
// would deliver them.
func remoteRecords(hostID string, from, n int) []rec.Record {
	out := make([]rec.Record, 0, n)
	for i := range n {
		seq := uint64(from + i)
		out = append(out, rec.Record{
			ID:       hostID + "-" + string(rune('a'+i%26)) + string(rune('0'+i/26%10)),
			HostID:   hostID,
			Hostname: hostID,
			Seq:      seq,
			Cmd:      "echo " + hostID,
			StartMs:  int64(1_700_000_000_000 + seq),
		})
	}
	return out
}

func newBoundedCache(keep int) *remoteCache {
	return &remoteCache{
		state:   "ok",
		cursors: map[string]uint64{},
		keep:    keep,
		tags:    newTagIndex(),
		prompts: newPromptIndex(),
	}
}

// cached returns the cached commands whose text contains sub. It reads the
// corpus directly rather than going through search(), which requires an
// attached syncer; what is under test here is what folding puts in the corpus.
func cached(rc *remoteCache, sub string) []rec.Record {
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	var out []rec.Record
	for i := range rc.records {
		if strings.Contains(rc.records[i].Cmd, sub) {
			out = append(out, rc.records[i])
		}
	}
	return out
}

// TestPruneBoundsRAMPerHost is the cap that keeps a long-lived daemon from
// growing without limit as remote history accumulates over years.
func TestPruneBoundsRAMPerHost(t *testing.T) {
	rc := newBoundedCache(10)

	rc.mu.Lock()
	rc.foldRemote(remoteRecords("h1", 1, 25))
	rc.foldRemote(remoteRecords("h2", 1, 4))
	rc.pruneLocked()
	kept := len(rc.records)
	rc.mu.Unlock()

	assert.Equal(t, 14, kept, "h1 trimmed to 10, h2 under the cap keeps all 4")

	rc.mu.RLock()
	defer rc.mu.RUnlock()
	var h1 []rec.Record
	for _, r := range rc.records {
		if r.HostID == "h1" {
			h1 = append(h1, r)
		}
	}
	require.Len(t, h1, 10)
	assert.Equal(t, uint64(16), h1[0].Seq, "the OLDEST records are the ones evicted")
	assert.Equal(t, uint64(25), h1[9].Seq, "the newest are retained")
	assert.Len(t, rc.cmds, kept, "the parallel commands slice stays in step")
}

// TestPruneAcrossCycles: the bound holds as records keep arriving, not just on
// the batch that first crosses it.
func TestPruneAcrossCycles(t *testing.T) {
	rc := newBoundedCache(5)
	for cycle := range 6 {
		rc.mu.Lock()
		rc.foldRemote(remoteRecords("h1", cycle*3+1, 3))
		rc.pruneLocked()
		rc.mu.Unlock()
	}
	rc.mu.RLock()
	defer rc.mu.RUnlock()
	require.Len(t, rc.records, 5)
	assert.Equal(t, uint64(14), rc.records[0].Seq, "18 pulled, newest 5 kept")
	assert.Equal(t, uint64(18), rc.records[4].Seq)
}

// TestPruneUnlimited: keep=0 is the explicit opt-out and must retain everything.
func TestPruneUnlimited(t *testing.T) {
	rc := newBoundedCache(0)
	rc.mu.Lock()
	rc.foldRemote(remoteRecords("h1", 1, 40))
	rc.pruneLocked()
	n := len(rc.records)
	rc.mu.Unlock()
	assert.Equal(t, 40, n, "keep=0 means unlimited")
}

// TestFoldRemoteRoutesRecordTypes: only commands enter the search corpus; tag
// and prompt records feed their indexes instead.
func TestFoldRemoteRoutesRecordTypes(t *testing.T) {
	rc := newBoundedCache(0)
	rc.mu.Lock()
	rc.foldRemote([]rec.Record{
		{ID: "c1", HostID: "h1", Cmd: "go build", PromptID: "p1", Seq: 1},
		{ID: "p1", HostID: "h1", Type: rec.TypePrompt, Prompt: "build it", Seq: 2},
		{ID: "t1", HostID: "h1", Type: rec.TypeTag, TagName: "work", TargetID: "c1", Seq: 3},
	})
	rc.mu.Unlock()

	assert.Len(t, cached(rc, "go build"), 1, "the command is searchable")
	assert.Empty(t, cached(rc, "build it"), "the prompt record is not a command")
	assert.Equal(t, "build it", promptText(rc.prompts, "p1"), "the prompt reached the shared index")
	assert.True(t, rc.tags.has(rec.Record{ID: "c1"}, "work"), "the tag reached the shared index")
}

// TestFoldRemoteAppliesTombstones: a delete pulled from another machine removes
// its target here too, and takes the prompt index entry with it.
func TestFoldRemoteAppliesTombstones(t *testing.T) {
	rc := newBoundedCache(0)
	rc.mu.Lock()
	rc.foldRemote([]rec.Record{
		{ID: "c1", HostID: "h1", Cmd: "secret thing", Seq: 1},
		{ID: "p1", HostID: "h1", Type: rec.TypePrompt, Prompt: "do the secret thing", Seq: 2},
	})
	rc.mu.Unlock()
	require.Len(t, cached(rc, "secret"), 1)

	rc.mu.Lock()
	rc.foldRemote([]rec.Record{
		{ID: "d1", HostID: "h1", Type: rec.TypeDelete, TargetID: "c1", Seq: 3},
		{ID: "d2", HostID: "h1", Type: rec.TypeDelete, TargetID: "p1", Seq: 4},
	})
	rc.mu.Unlock()

	assert.Empty(t, cached(rc, "secret"), "the tombstoned command is gone")
	assert.Empty(t, promptText(rc.prompts, "p1"), "the tombstoned prompt is gone from the index")
}
