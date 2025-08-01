// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/client/testutil"
	"github.com/hashicorp/nomad/helper/flatmap"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/kr/pretty"
	"github.com/shoenig/test/must"
)

func TestHelpers_FormatKV(t *testing.T) {
	ci.Parallel(t)
	in := []string{"alpha|beta", "charlie|delta", "echo|"}
	out := formatKV(in)

	expect := "alpha   = beta\n"
	expect += "charlie = delta\n"
	expect += "echo    = <none>"

	if out != expect {
		t.Fatalf("expect: %s, got: %s", expect, out)
	}
}

func TestHelpers_FormatList(t *testing.T) {
	ci.Parallel(t)
	in := []string{"alpha|beta||delta"}
	out := formatList(in)

	expect := "alpha  beta  <none>  delta"

	if out != expect {
		t.Fatalf("expect: %s, got: %s", expect, out)
	}
}

func TestHelpers_NodeID(t *testing.T) {
	ci.Parallel(t)
	srv, _, _ := testServer(t, false, nil)
	defer srv.Shutdown()

	meta := Meta{Ui: cli.NewMockUi()}
	client, err := meta.Client()
	if err != nil {
		t.FailNow()
	}

	// This is because there is no client
	if _, err := getLocalNodeID(client); err == nil {
		t.Fatalf("getLocalNodeID() should fail")
	}
}

func TestHelpers_LineLimitReader_NoTimeLimit(t *testing.T) {
	ci.Parallel(t)
	helloString := `hello
world
this
is
a
test`

	noLines := "jskdfhjasdhfjkajkldsfdlsjkahfkjdsafa"

	cases := []struct {
		Input       string
		Output      string
		Lines       int
		SearchLimit int
	}{
		{
			Input:       helloString,
			Output:      helloString,
			Lines:       6,
			SearchLimit: 1000,
		},
		{
			Input: helloString,
			Output: `world
this
is
a
test`,
			Lines:       5,
			SearchLimit: 1000,
		},
		{
			Input:       helloString,
			Output:      `test`,
			Lines:       1,
			SearchLimit: 1000,
		},
		{
			Input:       helloString,
			Output:      "",
			Lines:       0,
			SearchLimit: 1000,
		},
		{
			Input:       helloString,
			Output:      helloString,
			Lines:       6,
			SearchLimit: 1, // Exceed the limit
		},
		{
			Input:       noLines,
			Output:      noLines,
			Lines:       10,
			SearchLimit: 1000,
		},
		{
			Input:       noLines,
			Output:      noLines,
			Lines:       10,
			SearchLimit: 2,
		},
	}

	for i, c := range cases {
		in := io.NopCloser(strings.NewReader(c.Input))
		limit := NewLineLimitReader(in, c.Lines, c.SearchLimit, 0)
		outBytes, err := io.ReadAll(limit)
		if err != nil {
			t.Fatalf("case %d failed: %v", i, err)
		}

		out := string(outBytes)
		if out != c.Output {
			t.Fatalf("case %d: got %q; want %q", i, out, c.Output)
		}
	}
}

type testReadCloser struct {
	data chan []byte
}

func (t *testReadCloser) Read(p []byte) (n int, err error) {
	select {
	case b, ok := <-t.data:
		if !ok {
			return 0, io.EOF
		}

		return copy(p, b), nil
	case <-time.After(10 * time.Millisecond):
		return 0, nil
	}
}

func (t *testReadCloser) Close() error {
	close(t.data)
	return nil
}

func TestHelpers_LineLimitReader_TimeLimit(t *testing.T) {
	ci.Parallel(t)
	// Create the test reader
	in := &testReadCloser{data: make(chan []byte)}

	// Set up the reader such that it won't hit the line/buffer limit and could
	// only terminate if it hits the time limit
	limit := NewLineLimitReader(in, 1000, 1000, 100*time.Millisecond)

	expected := []byte("hello world")

	errCh := make(chan error)
	resultCh := make(chan []byte)
	go func() {
		defer close(resultCh)
		defer close(errCh)
		outBytes, err := io.ReadAll(limit)
		if err != nil {
			errCh <- fmt.Errorf("ReadAll failed: %v", err)
			return
		}
		resultCh <- outBytes
	}()

	// Send the data
	in.data <- expected
	in.Close()

	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("ReadAll: %v", err)
		}
	case outBytes := <-resultCh:
		if !reflect.DeepEqual(outBytes, expected) {
			t.Fatalf("got:%s, expected,%s", string(outBytes), string(expected))
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("did not exit by time limit")
	}
}

const (
	job1 = `job "job1" {
  type        = "service"
  datacenters = ["dc1"]
  group "group1" {
    count = 1
    task "task1" {
      driver = "exec"
      resources {}
    }
    restart {
      attempts         = 10
      mode             = "delay"
      interval         = "15s"
      render_templates = false
    }
  }
}`
)

var (
	expectedApiJob = &api.Job{
		ID:          pointer.Of("job1"),
		Name:        pointer.Of("job1"),
		Type:        pointer.Of("service"),
		Datacenters: []string{"dc1"},
		TaskGroups: []*api.TaskGroup{
			{
				Name:  pointer.Of("group1"),
				Count: pointer.Of(1),
				RestartPolicy: &api.RestartPolicy{
					Attempts:        pointer.Of(10),
					Interval:        pointer.Of(15 * time.Second),
					Mode:            pointer.Of("delay"),
					RenderTemplates: pointer.Of(false),
				},

				Tasks: []*api.Task{
					{
						Driver:    "exec",
						Name:      "task1",
						Resources: &api.Resources{},
					},
				},
			},
		},
	}
)

// Test APIJob with local jobfile
func TestJobGetter_LocalFile(t *testing.T) {
	ci.Parallel(t)
	fh, err := os.CreateTemp("", "nomad")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer os.Remove(fh.Name())
	_, err = fh.WriteString(job1)
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	j := &JobGetter{}
	_, aj, err := j.ApiJob(fh.Name())
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	if !reflect.DeepEqual(expectedApiJob, aj) {
		eflat := flatmap.Flatten(expectedApiJob, nil, false)
		aflat := flatmap.Flatten(aj, nil, false)
		t.Fatalf("got:\n%v\nwant:\n%v", aflat, eflat)
	}
}

// TestJobGetter_HCL2_Variables asserts variable arguments from CLI
// and varfiles are both honored
func TestJobGetter_HCL2_Variables(t *testing.T) {

	hcl := `
variables {
  var1 = "default-val"
  var2 = "default-val"
  var3 = "default-val"
  var4 = "default-val"
}

job "example" {
  datacenters = ["${var.var1}", "${var.var2}", "${var.var3}", "${var.var4}"]
}
`
	t.Setenv("NOMAD_VAR_var4", "from-envvar")

	cliArgs := []string{`var2=from-cli`}
	fileVars := `var3 = "from-varfile"`
	expected := []string{"default-val", "from-cli", "from-varfile", "from-envvar"}

	hclf, err := os.CreateTemp("", "hcl")
	must.NoError(t, err)
	defer os.Remove(hclf.Name())
	defer hclf.Close()

	_, err = hclf.WriteString(hcl)
	must.NoError(t, err)

	vf, err := os.CreateTemp("", "var.hcl")
	must.NoError(t, err)
	defer os.Remove(vf.Name())
	defer vf.Close()

	_, err = vf.WriteString(fileVars + "\n")
	must.NoError(t, err)

	jg := &JobGetter{
		Vars:     cliArgs,
		VarFiles: []string{vf.Name()},
		Strict:   true,
	}

	_, j, err := jg.Get(hclf.Name())
	must.NoError(t, err)

	must.NotNil(t, j)
	must.Eq(t, expected, j.Datacenters)
}

func TestJobGetter_HCL2_Variables_StrictFalse(t *testing.T) {

	hcl := `
variables {
  var1 = "default-val"
  var2 = "default-val"
  var3 = "default-val"
  var4 = "default-val"
}

job "example" {
  datacenters = ["${var.var1}", "${var.var2}", "${var.var3}", "${var.var4}"]
}
`

	t.Setenv("NOMAD_VAR_var4", "from-envvar")

	// Both the CLI and var file contain variables that are not used with the
	// template and therefore would error, if hcl2-strict was true.
	cliArgs := []string{`var2=from-cli`, `unsedVar1=from-cli`}
	fileVars := `
var3 = "from-varfile"
unsedVar2 = "from-varfile"
`
	expected := []string{"default-val", "from-cli", "from-varfile", "from-envvar"}

	hclf, err := os.CreateTemp("", "hcl")
	must.NoError(t, err)
	defer os.Remove(hclf.Name())
	defer hclf.Close()

	_, err = hclf.WriteString(hcl)
	must.NoError(t, err)

	vf, err := os.CreateTemp("", "var.hcl")
	must.NoError(t, err)
	defer os.Remove(vf.Name())
	defer vf.Close()

	_, err = vf.WriteString(fileVars + "\n")
	must.NoError(t, err)

	jg := &JobGetter{
		Vars:     cliArgs,
		VarFiles: []string{vf.Name()},
		Strict:   false,
	}

	_, j, err := jg.Get(hclf.Name())
	must.NoError(t, err)
	must.NotNil(t, j)
	must.Eq(t, expected, j.Datacenters)
}

// Test StructJob with jobfile from HTTP Server
func TestJobGetter_HTTPServer(t *testing.T) {
	ci.Parallel(t)
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, job1)
	})
	go http.ListenAndServe("127.0.0.1:12345", nil)

	// Wait until HTTP Server starts certainly
	time.Sleep(100 * time.Millisecond)

	j := &JobGetter{}
	_, aj, err := j.ApiJob("http://127.0.0.1:12345/")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if !reflect.DeepEqual(expectedApiJob, aj) {
		for _, d := range pretty.Diff(expectedApiJob, aj) {
			t.Log(d)
		}
		t.Fatal("Unexpected file")
	}
}

func TestJobGetter_Validate(t *testing.T) {
	cases := []struct {
		name        string
		jg          JobGetter
		errContains string
	}{
		{
			"VarsAndJSON",
			JobGetter{
				JSON: true,
				Vars: []string{"foo"},
			},
			"variables with JSON",
		},
		{
			"VarFilesAndJSON",
			JobGetter{
				JSON:     true,
				VarFiles: []string{"foo.var"},
			},
			"variables with JSON files",
		},
		{
			"JSON_OK",
			JobGetter{
				JSON: true,
			},
			"",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.jg.Validate()

			switch tc.errContains {
			case "":
				must.NoError(t, err)
			default:
				must.ErrorContains(t, err, tc.errContains)
			}

		})
	}
}

func TestPrettyTimeDiff(t *testing.T) {
	// Grab the time and truncate to the nearest second. This allows our tests
	// to be deterministic since we don't have to worry about rounding.
	now := time.Now().Truncate(time.Second)

	test_cases := []struct {
		t1  time.Time
		t2  time.Time
		exp string
	}{
		{now, time.Unix(0, 0), ""}, // This is the upgrade path case
		{now, now.Add(-10 * time.Millisecond), "0s ago"},
		{now, now.Add(-740 * time.Second), "12m20s ago"},
		{now, now.Add(-12 * time.Minute), "12m ago"},
		{now, now.Add(-60 * time.Minute), "1h ago"},
		{now, now.Add(-80 * time.Minute), "1h20m ago"},
		{now, now.Add(-6 * time.Hour), "6h ago"},
		{now.Add(-6 * time.Hour), now, "6h from now"},
		{now, now.Add(-22165 * time.Second), "6h9m ago"},
		{now, now.Add(-100 * time.Hour), "4d4h ago"},
		{now, now.Add(-438000 * time.Minute), "10mo4d ago"},
		{now, now.Add(-20460 * time.Hour), "2y4mo ago"},
	}
	for _, tc := range test_cases {
		t.Run(tc.exp, func(t *testing.T) {
			out := prettyTimeDiff(tc.t2, tc.t1)
			if out != tc.exp {
				t.Fatalf("expected :%v but got :%v", tc.exp, out)
			}
		})
	}

	var t1 time.Time
	out := prettyTimeDiff(t1, time.Now())

	if out != "" {
		t.Fatalf("Expected empty output but got:%v", out)
	}

}

// TestUiErrorWriter asserts that writer buffers and
func TestUiErrorWriter(t *testing.T) {
	ci.Parallel(t)

	var outBuf, errBuf bytes.Buffer
	ui := &cli.BasicUi{
		Writer:      &outBuf,
		ErrorWriter: &errBuf,
	}

	w := &uiErrorWriter{ui: ui}

	inputs := []string{
		"some line\n",
		"multiple\nlines\r\nhere",
		" with  followup\nand",
		" more lines ",
		" without new line ",
		"until here\nand then",
		"some more",
	}

	partialAcc := ""
	for _, in := range inputs {
		n, err := w.Write([]byte(in))
		must.NoError(t, err)
		must.Eq(t, len(in), n)

		// assert that writer emits partial result until last new line
		partialAcc += strings.ReplaceAll(in, "\r\n", "\n")
		lastNL := strings.LastIndex(partialAcc, "\n")
		must.Eq(t, partialAcc[:lastNL+1], errBuf.String())
	}

	must.Eq(t, "", outBuf.String())

	// note that the \r\n got replaced by \n
	expectedErr := "some line\nmultiple\nlines\nhere with  followup\nand more lines  without new line until here\n"
	must.Eq(t, expectedErr, errBuf.String())

	// close emits the final line
	err := w.Close()
	must.NoError(t, err)

	expectedErr += "and thensome more\n"
	must.Eq(t, expectedErr, errBuf.String())
}

func Test_extractVarFiles(t *testing.T) {
	ci.Parallel(t)

	t.Run("none", func(t *testing.T) {
		result, err := extractVarFiles(nil)
		must.NoError(t, err)
		must.Eq(t, "", result)
	})

	t.Run("files", func(t *testing.T) {
		d := t.TempDir()
		fileOne := filepath.Join(d, "one.hcl")
		fileTwo := filepath.Join(d, "two.hcl")

		must.NoError(t, os.WriteFile(fileOne, []byte(`foo = "bar"`), 0o644))
		must.NoError(t, os.WriteFile(fileTwo, []byte(`baz = 42`), 0o644))

		result, err := extractVarFiles([]string{fileOne, fileTwo})
		must.NoError(t, err)
		must.Eq(t, "foo = \"bar\"\nbaz = 42\n", result)
	})

	t.Run("unreadble", func(t *testing.T) {
		testutil.RequireNonRoot(t)

		d := t.TempDir()
		fileOne := filepath.Join(d, "one.hcl")

		must.NoError(t, os.WriteFile(fileOne, []byte(`foo = "bar"`), 0o200))

		_, err := extractVarFiles([]string{fileOne})
		must.ErrorContains(t, err, "permission denied")
	})
}

func Test_extractVarFlags(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil", func(t *testing.T) {
		result := extractVarFlags(nil)
		must.MapEmpty(t, result)
	})

	t.Run("complete", func(t *testing.T) {
		result := extractVarFlags([]string{"one=1", "two=2", "three"})
		must.Eq(t, map[string]string{
			"one":   "1",
			"two":   "2",
			"three": "",
		}, result)
	})
}

func Test_extractJobSpecEnvVars(t *testing.T) {
	ci.Parallel(t)

	t.Run("nil", func(t *testing.T) {
		must.MapEmpty(t, extractJobSpecEnvVars(nil))
	})

	t.Run("complete", func(t *testing.T) {
		result := extractJobSpecEnvVars([]string{
			"NOMAD_VAR_count=13",
			"GOPATH=/Users/jrasell/go",
			"NOMAD_VAR_image=redis:7",
		})
		must.Eq(t, map[string]string{
			"count": "13",
			"image": "redis:7",
		}, result)
	})

	t.Run("whitespace", func(t *testing.T) {
		result := extractJobSpecEnvVars([]string{
			"NOMAD_VAR_count = 13",
			"GOPATH = /Users/jrasell/go",
		})
		must.Eq(t, map[string]string{
			"count ": " 13",
		}, result)
	})

	t.Run("empty key", func(t *testing.T) {
		result := extractJobSpecEnvVars([]string{
			"NOMAD_VAR_=13",
			"=/Users/jrasell/go",
		})
		must.Eq(t, map[string]string{}, result)
	})

	t.Run("empty value", func(t *testing.T) {
		result := extractJobSpecEnvVars([]string{
			"NOMAD_VAR_count=",
			"GOPATH=",
		})
		must.Eq(t, map[string]string{
			"count": "",
		}, result)
	})
}

// TestHelperGetByPrefix exercises the generic getByPrefix function used by
// commands to find a single match by prefix or return matching results if there
// are multiple
func TestHelperGetByPrefix(t *testing.T) {

	type testStub struct{ ID string }

	testCases := []struct {
		name        string
		queryObjs   []*testStub
		queryErr    error
		queryPrefix string

		expectMatch    *testStub
		expectPossible []*testStub
		expectErr      string
	}{
		{
			name:      "query error",
			queryErr:  errors.New("foo"),
			expectErr: "error querying stubs: foo",
		},
		{
			name: "multiple prefix matches with exact match",
			queryObjs: []*testStub{
				{ID: "testing"},
				{ID: "testing123"},
			},
			queryPrefix: "testing",
			expectMatch: &testStub{ID: "testing"},
		},
		{
			name: "multiple prefix matches no exact match",
			queryObjs: []*testStub{
				{ID: "testing"},
				{ID: "testing123"},
			},
			queryPrefix:    "test",
			expectPossible: []*testStub{{ID: "testing"}, {ID: "testing123"}},
		},
		{
			name:        "no matches",
			queryObjs:   []*testStub{},
			queryPrefix: "test",
			expectErr:   "no stubs with prefix or ID \"test\" found",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			match, possible, err := getByPrefix[testStub]("stubs",
				func(*api.QueryOptions) ([]*testStub, *api.QueryMeta, error) {
					return tc.queryObjs, nil, tc.queryErr
				},
				func(stub *testStub, prefix string) bool { return stub.ID == prefix },
				&api.QueryOptions{Prefix: tc.queryPrefix})

			if tc.expectErr != "" {
				must.EqError(t, err, tc.expectErr)
			} else {
				must.NoError(t, err)
				must.Eq(t, tc.expectMatch, match, must.Sprint("expected exact match"))
				must.Eq(t, tc.expectPossible, possible, must.Sprint("expected prefix matches"))
			}
		})
	}

}

// TestHelperStreamFrames tests the streamFrames command helper used
// by the agent_monitor and fs_alloc endpoints to populate a reader
// with data from the streamFrame channel passed to the function
func TestHelperStreamFrames(t *testing.T) {
	const loopCount = 50

	// Create test file
	dir := t.TempDir()
	f, err := os.CreateTemp(dir, "log")
	must.NoError(t, err)
	writeLine := []byte("[INFO]log log log made of wood you are heavy but so good\n")
	writeLength := len(writeLine)

	for range loopCount {
		_, _ = f.Write(writeLine)
	}
	f.Close()

	// Create test file reader for streaming
	goldenFilePath := f.Name()
	goldenFileContents, err := os.ReadFile(goldenFilePath)
	must.NoError(t, err)

	fileReader, err := os.Open(goldenFilePath)
	must.NoError(t, err)

	// Helper func to populate stream chan in test case
	streamFunc := func() (chan *api.StreamFrame, chan error, chan struct{}) {
		framesCh := make(chan *api.StreamFrame, 30)
		errCh := make(chan error)
		cancelCh := make(chan struct{})

		offset := 0

		r := io.LimitReader(fileReader, 64)
		for {
			bytesHolder := make([]byte, 64)
			n, err := r.Read(bytesHolder)
			if err != nil && err != io.EOF {
				must.NoError(t, err)
			}

			if n == 0 && err == io.EOF {
				break
			}
			offset += n
			framesCh <- &api.StreamFrame{
				Offset: int64(offset),
				Data:   goldenFileContents,
				File:   goldenFilePath,
			}

			if n != 0 && err == io.EOF {
				//break after sending if we hit EOF with bytes in buffer
				break
			}
		}

		close(framesCh)
		return framesCh, errCh, cancelCh
	}
	testErr := errors.New("isErr")
	cases := []struct {
		name      string
		numLines  int
		expectErr bool
		err       error
	}{
		{
			name:     "happy_no_limit",
			numLines: -1,
		},
		{
			name:     "happy_limit",
			numLines: 25,
		},
		{
			name:      "error",
			numLines:  -1,
			expectErr: true,
			err:       testErr,
		},
	}

	for _, tc := range cases {

		t.Run(tc.name, func(t *testing.T) {

			framesCh, errCh, cancelCh := streamFunc()

			if tc.expectErr {
				go func() {
					time.Sleep(time.Nanosecond * 1)
					errCh <- tc.err
				}()
			}

			r, err := streamFrames(framesCh, errCh, int64(tc.numLines), cancelCh)
			if !tc.expectErr {
				must.NoError(t, err)
			}

			result, err := io.ReadAll(r)
			if !tc.expectErr {
				must.NoError(t, err)
			}
			if tc.numLines == -1 {
				//expectedLength := writeLength * loopCount
				must.Eq(t,
					goldenFileContents,
					result)
			} else {
				expectedLength := (writeLength * tc.numLines)
				must.Eq(t,
					expectedLength,
					len(result))
			}

			r.Close()
		})
	}
}
