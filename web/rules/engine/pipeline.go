package rulesengine

import (
	"fmt"
	"io"
	"io/fs"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hblabs-co/kit/core/envcfg"
)

// pipelineFile is the on-disk YAML schema: a single document
// declares one-or-more pipelines under a top-level `pipelines:`
// map. The Engine can `Load` multiple files; later files merge
// into the registry (duplicate pipeline names = boot error).
//
//	pipelines:
//	  org_access_check:
//	    pick: [access_granted, plan]
//	    flow: [...]
type pipelineFile struct {
	Pipelines map[string]*pipelineSpec `yaml:"pipelines"`
}

// pipelineSpec is one named pipeline. `flow` is the ordered list of
// steps to run; `pick` (optional) filters the final Context.Data
// down to only those keys before returning the Result.
type pipelineSpec struct {
	Pick []string    `yaml:"pick"`
	Flow []*stepSpec `yaml:"flow"`
}

// stepSpec is one element of a flow list. It's a discriminated
// union over four shapes, distinguished by which field is set:
//
//   - Name != ""        → a single rule invocation
//   - len(Parallel) > 0 → a group of sibling steps run concurrently
//   - len(Sequence) > 0 → a group of sibling steps run in order
//   - Pipeline != ""    → inline another pipeline's flow at this point
//
// Exactly one of the four is allowed per element; the loader
// enforces it after decoding. Per-step modifiers (With, When,
// OnSuccess, OnFailure) only apply to the Name form — they're
// meaningless on a group container.
type stepSpec struct {
	// Name is the Rule identifier (matches Rule.Name()). Mutually
	// exclusive with Parallel and Sequence.
	Name string `yaml:"name"`

	// With is the rule-specific config, exposed to Run via
	// Context.Step(). YAML decodes untyped maps as map[string]any.
	With StepConfig `yaml:"with"`

	// When is an expr-lang expression; when set, the step is
	// skipped if it evaluates falsy. Compiled once at Engine.Build.
	When string `yaml:"when"`

	// OnSuccess runs after Run returns nil. Lets a step trigger a
	// downstream chain without inflating the main flow.
	OnSuccess []*stepSpec `yaml:"on_success"`

	// OnFailure runs when Run returns a non-Skip, non-Abort error.
	// If the failure branch returns nil from its last step, the
	// outer pipeline continues; otherwise the error propagates.
	OnFailure []*stepSpec `yaml:"on_failure"`

	// Parallel is a group of steps that all run on their own
	// goroutine. The Engine validates at Build that no two rules
	// inside the same Parallel block declare overlapping Writes().
	// Mutually exclusive with Name and Sequence.
	Parallel []*stepSpec `yaml:"parallel"`

	// Sequence is a named group of steps that run in order. Useful
	// inside on_success / on_failure to express "run this short
	// chain on success" without losing the parent's structure.
	// Mutually exclusive with Name and Parallel.
	Sequence []*stepSpec `yaml:"sequence"`

	// Pipeline references another pipeline by name and inlines its
	// flow at this point. The referenced pipeline's `pick:` is
	// ignored (a sub-pipeline contributes raw Data; only the top-
	// level pipeline's pick shapes the response). Mutually
	// exclusive with Name, Parallel and Sequence.
	//
	// Cycle detection runs at Build: a sub-pipeline that
	// transitively references itself surfaces as a boot error.
	// Diamond dependencies (A → B and A → C, both → D) are fine —
	// D is not a cycle.
	Pipeline string `yaml:"pipeline"`

	// Timeout caps how long a single attempt of this rule may run.
	// time.ParseDuration syntax ("2s", "500ms"). Zero / empty =
	// no per-attempt timeout (the engine still respects the
	// parent context's deadline). Only meaningful on rule steps;
	// the loader rejects it on parallel / sequence containers.
	Timeout string `yaml:"timeout"`

	// Retry controls how the engine re-invokes the rule when its
	// Run returns a retryable error. Nil = no retry. Retry is
	// intentionally opt-in per step: the engine has no way to
	// know if a rule is idempotent, so it doesn't retry by
	// default. See retrySpec for the knob meanings.
	Retry *retrySpec `yaml:"retry"`
}

// retrySpec is the per-step retry policy. All fields are optional
// individually but Count must be > 0 once the parent block is
// present (a retry block with count=0 is a config bug, not "no
// retry" — that's expressed by omitting the block entirely).
//
// Backoff defaults are biased toward "fast first attempt, escape
// hatch for long outages": exponential growth from 100ms to 5s
// caps total wait around ~8s after 3 retries, fits inside typical
// HTTP request budgets.
type retrySpec struct {
	// Count is the maximum number of additional attempts past the
	// first. Total invocations = Count + 1. Must be > 0.
	Count int `yaml:"count"`

	// Backoff is the delay schedule between attempts:
	//   - "constant" — always Initial.
	//   - "linear" — Initial * attempt, clamped to Max.
	//   - "exponential" (default) — Initial * 2^attempt, clamped to Max.
	Backoff string `yaml:"backoff"`

	// Initial is the first backoff delay. time.ParseDuration syntax.
	// Empty = 100ms.
	Initial string `yaml:"initial"`

	// Max caps the longest single backoff delay. time.ParseDuration
	// syntax. Empty = 5s.
	Max string `yaml:"max"`
}

// kind is the resolved shape of a stepSpec — used internally so
// the executor doesn't repeat the discriminator check on every
// step. Computed once by validate().
type stepKind int

const (
	stepKindUnknown stepKind = iota
	stepKindRule
	stepKindParallel
	stepKindSequence
	stepKindPipeline
)

// loadPipelinesFn is a variable seam so tests can inject a custom
// loader (or simulate read failures) without rewriting fs.FS plumbing.
// Defaults to the real implementation; production code never swaps it.
var loadPipelinesFn = readPipelineFiles

// readPipelineFiles walks fsys for every file matching glob, expands
// env-var refs (${HB_FOO:-default}) in each body, decodes in strict
// mode, and returns the merged map of pipeline name → spec.
// Duplicate names across files surface as a boot error.
func readPipelineFiles(fsys fs.FS, glob string) (map[string]*pipelineSpec, error) {
	matches, err := fs.Glob(fsys, glob)
	if err != nil {
		return nil, fmt.Errorf("rules: glob %s: %w", glob, err)
	}
	out := make(map[string]*pipelineSpec)
	for _, path := range matches {
		raw, err := readFileFS(fsys, path)
		if err != nil {
			return nil, err
		}
		var file pipelineFile
		dec := yaml.NewDecoder(strings.NewReader(envcfg.Expand(string(raw))))
		dec.KnownFields(true)
		if err := dec.Decode(&file); err != nil {
			return nil, fmt.Errorf("rules: decode %s: %w", path, err)
		}
		for name, spec := range file.Pipelines {
			if _, dup := out[name]; dup {
				return nil, fmt.Errorf("rules: duplicate pipeline %q in %s", name, path)
			}
			out[name] = spec
		}
	}
	return out, nil
}

// readFileFS opens, reads, and closes path inside fsys. Pulled out
// so the read-error path is its own one-liner — easier to test via
// an fs.FS that fails Open on a known sentinel name.
func readFileFS(fsys fs.FS, path string) ([]byte, error) {
	f, err := fsys.Open(path)
	if err != nil {
		return nil, fmt.Errorf("rules: open %s: %w", path, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("rules: read %s: %w", path, err)
	}
	return b, nil
}

// kindOf classifies a stepSpec and validates that exactly one
// shape is declared. Returns an error keyed by stepKindUnknown when
// the shape is ambiguous or empty — both are user-facing config
// bugs caught at Build time.
func kindOf(s *stepSpec) (stepKind, error) {
	shapes := 0
	var k stepKind
	if s.Name != "" {
		shapes++
		k = stepKindRule
	}
	if len(s.Parallel) > 0 {
		shapes++
		k = stepKindParallel
	}
	if len(s.Sequence) > 0 {
		shapes++
		k = stepKindSequence
	}
	if s.Pipeline != "" {
		shapes++
		k = stepKindPipeline
	}
	switch shapes {
	case 0:
		return stepKindUnknown, fmt.Errorf("rules: step must set one of name/parallel/sequence/pipeline")
	case 1:
		return k, nil
	default:
		return stepKindUnknown, fmt.Errorf("rules: step declares multiple shapes (name/parallel/sequence/pipeline are mutually exclusive)")
	}
}
