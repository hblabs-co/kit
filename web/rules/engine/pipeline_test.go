package rulesengine

import (
	"errors"
	"io/fs"
	"testing"
	"testing/fstest"
	"time"
)

func TestReadPipelineFilesParsesValidYAML(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"pipelines/access.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  access:
    pick: [granted]
    flow:
      - name: fetch
        with: { source: header }
        when: "data.org != nil"
      - parallel:
          - name: load_plan
          - name: load_user
`)},
	}
	got, err := readPipelineFiles(fsys, "pipelines/*.yaml")
	if err != nil {
		t.Fatalf("readPipelineFiles err = %v", err)
	}
	spec, ok := got["access"]
	if !ok {
		t.Fatal("pipeline 'access' missing from result")
	}
	if len(spec.Flow) != 2 {
		t.Fatalf("flow length = %d, want 2", len(spec.Flow))
	}
	if spec.Flow[0].Name != "fetch" || spec.Flow[0].When == "" {
		t.Fatalf("step[0] = %+v", spec.Flow[0])
	}
	if len(spec.Flow[1].Parallel) != 2 {
		t.Fatalf("parallel group size = %d", len(spec.Flow[1].Parallel))
	}
}

func TestReadPipelineFilesEnvInterpolation(t *testing.T) {
	t.Setenv("HB_TEST_RULES_PLAN", "pro")
	fsys := fstest.MapFS{
		"a.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - name: r
        with: { plan: "${HB_TEST_RULES_PLAN:-free}" }
`)},
	}
	got, err := readPipelineFiles(fsys, "*.yaml")
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got["p"].Flow[0].With.String("plan") != "pro" {
		t.Fatalf("env interpolation failed: %v", got["p"].Flow[0].With)
	}
}

func TestReadPipelineFilesStrictMode(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"x.yaml": &fstest.MapFile{Data: []byte(`
pipelines:
  p:
    flow:
      - name: r
        typo_field: oops
`)},
	}
	if _, err := readPipelineFiles(fsys, "*.yaml"); err == nil {
		t.Fatal("strict mode must reject unknown keys")
	}
}

func TestReadPipelineFilesDuplicatePipelineName(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{
		"a.yaml": &fstest.MapFile{Data: []byte("pipelines:\n  p:\n    flow:\n      - name: r\n")},
		"b.yaml": &fstest.MapFile{Data: []byte("pipelines:\n  p:\n    flow:\n      - name: r\n")},
	}
	if _, err := readPipelineFiles(fsys, "*.yaml"); err == nil {
		t.Fatal("duplicate pipeline name across files must error")
	}
}

func TestReadPipelineFilesGlobError(t *testing.T) {
	t.Parallel()
	if _, err := readPipelineFiles(fstest.MapFS{}, "[invalid"); err == nil {
		t.Fatal("malformed glob must error")
	}
}

func TestReadPipelineFilesOpenError(t *testing.T) {
	t.Parallel()
	fsys := errOpenFS{name: "broken.yaml", err: errors.New("forced open fail")}
	if _, err := readPipelineFiles(fsys, "broken.yaml"); err == nil {
		t.Fatal("Open failure must propagate")
	}
}

func TestReadPipelineFilesReadError(t *testing.T) {
	t.Parallel()
	fsys := errReadFS{name: "boom.yaml"}
	if _, err := readPipelineFiles(fsys, "boom.yaml"); err == nil {
		t.Fatal("Read failure must propagate")
	}
}

func TestKindOfDetectsShapes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		step *stepSpec
		want stepKind
		err  bool
	}{
		{"rule", &stepSpec{Name: "r"}, stepKindRule, false},
		{"parallel", &stepSpec{Parallel: []*stepSpec{{Name: "a"}}}, stepKindParallel, false},
		{"sequence", &stepSpec{Sequence: []*stepSpec{{Name: "a"}}}, stepKindSequence, false},
		{"empty", &stepSpec{}, stepKindUnknown, true},
		{"ambiguous", &stepSpec{Name: "r", Parallel: []*stepSpec{{Name: "a"}}}, stepKindUnknown, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := kindOf(tc.step)
			if tc.err {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("kindOf = %d, want %d", got, tc.want)
			}
		})
	}
}

// errOpenFS is a minimal fs.FS that succeeds Glob but errors on Open.
type errOpenFS struct {
	name string
	err  error
}

func (e errOpenFS) Open(name string) (fs.File, error) {
	if name == e.name {
		return nil, e.err
	}
	return nil, fs.ErrNotExist
}

func (e errOpenFS) Glob(pattern string) ([]string, error) {
	if pattern == e.name {
		return []string{e.name}, nil
	}
	return nil, nil
}

// errReadFS returns a file whose Read always errors — covers the
// io.ReadAll failure branch in readFileFS.
type errReadFS struct{ name string }

func (e errReadFS) Open(name string) (fs.File, error) {
	if name == e.name {
		return &errFile{name: name}, nil
	}
	return nil, fs.ErrNotExist
}

func (e errReadFS) Glob(pattern string) ([]string, error) {
	if pattern == e.name {
		return []string{e.name}, nil
	}
	return nil, nil
}

type errFile struct{ name string }

func (f *errFile) Stat() (fs.FileInfo, error) { return errFileInfo{name: f.name}, nil }
func (f *errFile) Read([]byte) (int, error)   { return 0, errors.New("forced read fail") }
func (f *errFile) Close() error               { return nil }

type errFileInfo struct{ name string }

func (i errFileInfo) Name() string       { return i.name }
func (i errFileInfo) Size() int64        { return 0 }
func (i errFileInfo) Mode() fs.FileMode  { return 0 }
func (i errFileInfo) ModTime() time.Time { return time.Time{} }
func (i errFileInfo) IsDir() bool        { return false }
func (i errFileInfo) Sys() any           { return nil }
