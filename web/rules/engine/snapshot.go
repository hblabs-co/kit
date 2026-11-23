package rulesengine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

// Snapshot is an opaque view of every pipeline currently compiled
// in the engine. Built by Engine.Snapshot, consumed by Diff for
// the dev hot-reload log. Internals (per-pipeline digests, per-
// step fingerprints) are unexported because no caller needs to
// introspect them — the public surface is "take a snapshot, diff
// it against another".
//
// Shape is intentionally flat under the hood: groups (parallel /
// sequence / pipeline) and branches (on_success / on_failure) are
// recursively walked in DFS order so the diff sees every rule
// invocation regardless of nesting. That's enough for "what
// changed" logs; a future Plan-style API can expose the full tree
// alongside this one rather than fattening Snapshot.
type Snapshot struct {
	// pipelines maps pipeline name → digest. Unexported so callers
	// route through Diff instead of poking at the map directly.
	pipelines map[string]pipelineDigest
}

// pipelineDigest captures the executable shape of a single
// pipeline for diffing purposes. Two pipelines whose digest
// fingerprints all match describe pipelines that would behave
// identically.
type pipelineDigest struct {
	steps []stepFingerprint
}

// stepFingerprint is the diff-stable representation of one rule
// step. Two fingerprints are equal iff the step would execute
// identically: same rule, same `when:` source, same `with:` config.
//
// Position is NOT part of equality — re-ordering steps within a
// pipeline still surfaces in the diff (the steps slice is
// order-aware), but matching individual steps across snapshots is
// by content, not index.
type stepFingerprint struct {
	rule       string // rule's Name(); the primary discriminator
	path       string // location tag e.g. "flow[1].parallel[0]"
	configHash string // sha256[:12] of `when:` source + canonical `with:`
}

// Snapshot returns the current Snapshot. Safe to call any time
// after Build; the engine's pipeline map is set once and never
// mutated after, so concurrent Run / Snapshot calls share the
// underlying data without locks. Returns a Snapshot with no
// pipelines if the engine has none.
func (e *Engine) Snapshot() Snapshot {
	out := make(map[string]pipelineDigest, len(e.pipelines))
	for name, p := range e.pipelines {
		out[name] = pipelineDigest{steps: collectFingerprints(p.flow, "flow")}
	}
	return Snapshot{pipelines: out}
}

// Diff returns the set of pipelines that changed between prev and
// s — pipelines added in s, removed from prev, or whose step
// fingerprints differ. Pipelines byte-equal across both snapshots
// are omitted.
//
// Ordering: returned slice is sorted by pipeline name so callers
// (the dev hot-reload log) emit a stable, easy-to-scan sequence.
func (s Snapshot) Diff(prev Snapshot) []PipelineChange {
	var changes []PipelineChange
	seen := make(map[string]bool, len(prev.pipelines)+len(s.pipelines))
	for name := range prev.pipelines {
		seen[name] = true
	}
	for name := range s.pipelines {
		seen[name] = true
	}
	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		change, ok := diffPipeline(name, prev, s)
		if ok {
			changes = append(changes, change)
		}
	}
	return changes
}

// collectFingerprints walks the compiled tree in DFS order, emitting
// one stepFingerprint per rule-kind step. Group containers (parallel
// / sequence) contribute their children but not themselves —
// matching the executor's semantics: groups are structure, rules
// are the work.
//
// Branches (on_success / on_failure) are walked with an appended
// path segment so the diff log can point at them, e.g.
// "flow[2].on_failure[0]".
func collectFingerprints(steps []*compiledStep, path string) []stepFingerprint {
	var out []stepFingerprint
	for i, s := range steps {
		segment := path + "[" + itoa(i) + "]"
		switch s.kind {
		case stepKindRule:
			out = append(out, stepFingerprint{
				rule:       s.rule.Name(),
				path:       segment,
				configHash: hashStepConfig(s),
			})
		case stepKindParallel:
			out = append(out, collectFingerprints(s.group, segment+".parallel")...)
		case stepKindSequence:
			out = append(out, collectFingerprints(s.group, segment+".sequence")...)
		case stepKindPipeline:
			// Walk the inlined sub-pipeline so its rules show up
			// in the digest. The path tag includes "pipeline"
			// even though the executor treats it like a sequence
			// — useful when reading a diff to know the steps
			// came from a sub.
			out = append(out, collectFingerprints(s.group, segment+".pipeline")...)
		}
		out = append(out, collectFingerprints(s.onSuccess, segment+".on_success")...)
		out = append(out, collectFingerprints(s.onFailure, segment+".on_failure")...)
	}
	return out
}

// hashStepConfig fingerprints a compiled step's runtime config —
// `when:` source plus a canonical JSON of `with:`. json.Marshal
// already sorts map keys; canonicalise recurses through nested
// maps and slices defensively so the encoding stays deterministic
// regardless of yaml.v3's decode order.
func hashStepConfig(s *compiledStep) string {
	h := sha256.New()
	if s.when != nil {
		h.Write([]byte(s.when.source))
	}
	h.Write([]byte{0x1f}) // unambiguous separator (ASCII unit separator)
	canonical, _ := json.Marshal(canonicalise(s.with))
	h.Write(canonical)
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// canonicalise converts a StepConfig into a value whose JSON
// encoding is stable: nested maps are walked recursively so that
// every level uses sorted keys (json.Marshal already sorts top-level
// map keys; this defensively handles the rare case of yaml.v3
// producing a non-sorted nested structure).
func canonicalise(v any) any {
	switch m := v.(type) {
	case StepConfig:
		return canonicaliseMap(map[string]any(m))
	case map[string]any:
		return canonicaliseMap(m)
	case []any:
		out := make([]any, len(m))
		for i, x := range m {
			out[i] = canonicalise(x)
		}
		return out
	default:
		return v
	}
}

func canonicaliseMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = canonicalise(v)
	}
	return out
}

// itoa avoids strconv just for a small positive int — keeps this
// file's imports minimal.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// PipelineChange describes how one pipeline differs between two
// snapshots. Produced by Snapshot.Diff. Kind discriminates the
// three transitions; the other fields are populated only for the
// kind they belong to.
type PipelineChange struct {
	// Pipeline is the pipeline's name (key in the engine's
	// pipeline map).
	Pipeline string

	// Kind is "added" | "removed" | "modified".
	//   - "added"    → pipeline appeared in next that wasn't in prev.
	//   - "removed"  → pipeline gone in next that was in prev.
	//   - "modified" → pipeline present in both with a different
	//     set of step fingerprints; AddedRules / RemovedRules /
	//     ChangedRules carry the specifics.
	Kind string

	// AddedRules are rule names that appear in next but not prev,
	// deduplicated. Only populated for Kind == "modified".
	AddedRules []string

	// RemovedRules are rule names in prev not in next, deduplicated.
	// Only populated for Kind == "modified".
	RemovedRules []string

	// ChangedRules are rule names present in both prev and next
	// but with at least one fingerprint whose ConfigHash differs
	// (`with:` or `when:` changed). Deduplicated. Only populated
	// for Kind == "modified".
	ChangedRules []string
}

// diffPipeline produces the PipelineChange for one pipeline name
// across two snapshots. The (prev, next) snapshots are passed by
// value but reference the same shared map keyed by name; the
// helper looks both up and decides added / removed / modified.
func diffPipeline(name string, prev, next Snapshot) (PipelineChange, bool) {
	_, prevHas := prev.pipelines[name]
	_, nextHas := next.pipelines[name]
	switch {
	case !prevHas:
		return PipelineChange{Pipeline: name, Kind: "added"}, true
	case !nextHas:
		return PipelineChange{Pipeline: name, Kind: "removed"}, true
	}

	added, removed, changed := diffSteps(prev.pipelines[name].steps, next.pipelines[name].steps)
	if len(added) == 0 && len(removed) == 0 && len(changed) == 0 {
		return PipelineChange{}, false
	}
	return PipelineChange{
		Pipeline:     name,
		Kind:         "modified",
		AddedRules:   added,
		RemovedRules: removed,
		ChangedRules: changed,
	}, true
}

// diffSteps computes the three rule-name sets that describe the
// difference between two step lists:
//
//   - added: rule names that appear in next but not prev.
//   - removed: rule names in prev but not next.
//   - changed: rule names present in both but with at least one
//     fingerprint whose ConfigHash differs.
//
// All three slices are sorted + deduplicated. A rule that moved
// position but kept the same config shows up in neither set; a
// pure reorder is invisible to the diff (path-by-path comparison
// would catch it, but it's noise for the dev log — execution
// order in a sequence doesn't usually matter to the operator
// scanning a save notification).
func diffSteps(prev, next []stepFingerprint) (added, removed, changed []string) {
	prevByRule := groupByRule(prev)
	nextByRule := groupByRule(next)

	addedSet := map[string]struct{}{}
	removedSet := map[string]struct{}{}
	changedSet := map[string]struct{}{}

	for rule, prevHashes := range prevByRule {
		nextHashes, present := nextByRule[rule]
		if !present {
			removedSet[rule] = struct{}{}
			continue
		}
		if !sameHashSet(prevHashes, nextHashes) {
			changedSet[rule] = struct{}{}
		}
	}
	for rule := range nextByRule {
		if _, present := prevByRule[rule]; !present {
			addedSet[rule] = struct{}{}
		}
	}
	return sortedKeys(addedSet), sortedKeys(removedSet), sortedKeys(changedSet)
}

// groupByRule returns rule name → set of ConfigHashes the rule
// shows up under. A rule that appears at two different positions
// with the same config contributes one hash; with different
// config, two. The set form makes equality cheap.
func groupByRule(steps []stepFingerprint) map[string]map[string]struct{} {
	out := make(map[string]map[string]struct{}, len(steps))
	for _, s := range steps {
		if _, ok := out[s.rule]; !ok {
			out[s.rule] = map[string]struct{}{}
		}
		out[s.rule][s.configHash] = struct{}{}
	}
	return out
}

// sameHashSet reports whether two ConfigHash sets are equal.
// Both are expected to be small (a rule appearing more than a
// handful of times in one pipeline is unusual) so O(n) is fine.
func sameHashSet(a, b map[string]struct{}) bool {
	if len(a) != len(b) {
		return false
	}
	for k := range a {
		if _, ok := b[k]; !ok {
			return false
		}
	}
	return true
}

// sortedKeys returns a sorted slice of a set's keys. Nil for the
// empty set so the caller can `len(...) == 0` cheaply.
func sortedKeys(m map[string]struct{}) []string {
	if len(m) == 0 {
		return nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
