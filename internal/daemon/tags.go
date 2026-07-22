package daemon

import (
	"sort"
	"strings"
	"sync"

	"yore/internal/proto"
	"yore/internal/rec"
)

// tagIndex resolves user-tag associations from TypeTag records: which freeform
// tags apply to each command id and each session id, plus a registry of known
// tag names and their descriptions. It is fed by the local ingest fold and the
// remote pull fold, and read at query time to resolve a row's effective tags.
// Safe for concurrent use.
type tagIndex struct {
	mu       sync.RWMutex
	desc     map[string]string          // tag name -> description (last non-empty wins)
	cmdTags  map[string]map[string]bool // command id -> set of tag names
	sessTags map[string]map[string]bool // session id -> set of tag names
	autoTags map[string]string          // cwd prefix -> tag name (config; resolved at read time)
}

// setAutoTags installs the cwd-prefix -> tag rules (from config.AutoTags). They
// are applied when resolving a row's effective tags, so there is no cost on the
// record path and a rule change applies retroactively.
func (t *tagIndex) setAutoTags(m map[string]string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.autoTags = m
}

// cwdUnder reports whether cwd is prefix or nested beneath it (segment-aware).
func cwdUnder(cwd, prefix string) bool {
	if prefix == "" || cwd == "" {
		return false
	}
	return cwd == prefix || strings.HasPrefix(cwd, strings.TrimRight(prefix, "/")+"/")
}

func newTagIndex() *tagIndex {
	return &tagIndex{
		desc:     map[string]string{},
		cmdTags:  map[string]map[string]bool{},
		sessTags: map[string]map[string]bool{},
	}
}

// normTag lowercases and trims a tag name (names are case-insensitive identity).
func normTag(name string) string { return strings.ToLower(strings.TrimSpace(name)) }

// apply folds one TypeTag record into the index: add/remove a label on a command
// (TargetID) or a session (Session); with neither target it just registers the
// name and description. Idempotent, so re-applying (warm tail overlap, re-pull)
// is harmless. Non-tag records are ignored.
func (t *tagIndex) apply(r rec.Record) {
	if r.Type != rec.TypeTag {
		return
	}
	name := normTag(r.TagName)
	if name == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	// Register the name; keep a description if one is provided.
	if r.TagDesc != "" {
		t.desc[name] = r.TagDesc
	} else if _, ok := t.desc[name]; !ok {
		t.desc[name] = ""
	}

	var m map[string]map[string]bool
	var target string
	switch {
	case r.TargetID != "":
		m, target = t.cmdTags, r.TargetID
	case r.Session != "":
		m, target = t.sessTags, r.Session
	default:
		return // a bare definition: name registered, no association
	}
	set := m[target]
	if set == nil {
		set = map[string]bool{}
		m[target] = set
	}
	if r.TagOp == rec.TagOpRemove {
		delete(set, name)
		if len(set) == 0 {
			delete(m, target)
		}
		return
	}
	set[name] = true
}

// effective returns a row's resolved freeform tags: its executor auto-tag (if
// any) ∪ command tags ∪ session tags, sorted and de-duplicated. nil when none.
func (t *tagIndex) effective(r rec.Record) []string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	set := map[string]bool{}
	if r.Tag != "" {
		set[normTag(r.Tag)] = true
	}
	for n := range t.cmdTags[r.ID] {
		set[n] = true
	}
	for n := range t.sessTags[r.Session] {
		set[n] = true
	}
	for prefix, name := range t.autoTags {
		if cwdUnder(r.Cwd, prefix) {
			set[normTag(name)] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// has reports whether a row carries the given freeform tag (executor auto-tag
// included), matched case-insensitively. An empty name matches everything.
func (t *tagIndex) has(r rec.Record, name string) bool {
	name = normTag(name)
	if name == "" {
		return true
	}
	if normTag(r.Tag) == name {
		return true
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.cmdTags[r.ID][name] || t.sessTags[r.Session][name] {
		return true
	}
	for prefix, an := range t.autoTags {
		if normTag(an) == name && cwdUnder(r.Cwd, prefix) {
			return true
		}
	}
	return false
}

// list returns every known tag with its association count and description,
// name-sorted. The count is how many commands+sessions carry it.
func (t *tagIndex) list() []proto.TagCount {
	t.mu.RLock()
	defer t.mu.RUnlock()
	counts := map[string]int{}
	for _, set := range t.cmdTags {
		for n := range set {
			counts[n]++
		}
	}
	for _, set := range t.sessTags {
		for n := range set {
			counts[n]++
		}
	}
	names := map[string]bool{}
	for n := range t.desc {
		names[n] = true
	}
	for n := range counts {
		names[n] = true
	}
	out := make([]proto.TagCount, 0, len(names))
	for n := range names {
		out = append(out, proto.TagCount{Name: n, Desc: t.desc[n], Count: counts[n]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
