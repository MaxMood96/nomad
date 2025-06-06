// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package command

import (
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hashicorp/cli"
	"github.com/hashicorp/nomad/api"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/testutil"
	"github.com/shoenig/test/must"
)

func TestPlanCommand_Implements(t *testing.T) {
	ci.Parallel(t)
	var _ cli.Command = &JobRunCommand{}
}

func TestPlanCommand_Fails(t *testing.T) {
	ci.Parallel(t)

	// Create a server
	s := testutil.NewTestServer(t, nil)
	defer s.Stop()

	ui := cli.NewMockUi()
	cmd := &JobPlanCommand{Meta: Meta{Ui: ui, flagAddress: "http://" + s.HTTPAddr}}

	// Fails on misuse
	if code := cmd.Run([]string{"some", "bad", "args"}); code != 255 {
		t.Fatalf("expected exit code 1, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, commandErrorText(cmd)) {
		t.Fatalf("expected help output, got: %s", out)
	}
	ui.ErrorWriter.Reset()

	// Fails when specified file does not exist
	if code := cmd.Run([]string{"/unicorns/leprechauns"}); code != 255 {
		t.Fatalf("expect exit 255, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, "Error getting job struct") {
		t.Fatalf("expect getting job struct error, got: %s", out)
	}
	ui.ErrorWriter.Reset()

	// Fails on invalid HCL
	fh1, err := os.CreateTemp("", "nomad")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer os.Remove(fh1.Name())
	if _, err := fh1.WriteString("nope"); err != nil {
		t.Fatalf("err: %s", err)
	}
	if code := cmd.Run([]string{fh1.Name()}); code != 255 {
		t.Fatalf("expect exit 255, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, "Error getting job struct") {
		t.Fatalf("expect parsing error, got: %s", out)
	}
	ui.ErrorWriter.Reset()

	// Fails on invalid job spec
	fh2, err := os.CreateTemp("", "nomad")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer os.Remove(fh2.Name())
	if _, err := fh2.WriteString(`job "job1" {}`); err != nil {
		t.Fatalf("err: %s", err)
	}
	if code := cmd.Run([]string{fh2.Name()}); code != 255 {
		t.Fatalf("expect exit 255, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, "Error during plan") {
		t.Fatalf("expect validation error, got: %s", out)
	}
	ui.ErrorWriter.Reset()

	// Fails on connection failure (requires a valid job)
	fh3, err := os.CreateTemp("", "nomad")
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	defer os.Remove(fh3.Name())
	_, err = fh3.WriteString(`
job "job1" {
	type = "service"
	datacenters = [ "dc1" ]
	group "group1" {
		count = 1
		task "task1" {
			driver = "exec"
			resources {
				cpu = 1000
				memory = 512
			}
		}
	}
}`)
	if err != nil {
		t.Fatalf("err: %s", err)
	}
	if code := cmd.Run([]string{"-address=nope", fh3.Name()}); code != 255 {
		t.Fatalf("expected exit code 255, got: %d", code)
	}
	if out := ui.ErrorWriter.String(); !strings.Contains(out, "Error during plan") {
		t.Fatalf("expected failed query error, got: %s", out)
	}
}

func TestPlanCommand_From_STDIN(t *testing.T) {
	_, _, addr := testServer(t, false, nil)

	ci.Parallel(t)
	stdinR, stdinW, err := os.Pipe()
	if err != nil {
		t.Fatalf("err: %s", err)
	}

	ui := cli.NewMockUi()
	cmd := &JobPlanCommand{
		Meta: Meta{
			Ui:          ui,
			flagAddress: addr,
		},
		JobGetter: JobGetter{testStdin: stdinR},
	}

	go func() {
		stdinW.WriteString(`
job "job1" {
	datacenters = ["dc1"]
  type = "service"
	group "group1" {
    count = 1
		task "task1" {
      driver = "exec"
			resources {
        cpu    = 100
        memory = 100
      }
    }
  }
}`)
		stdinW.Close()
	}()

	args := []string{"-address", addr, "-"}
	code := cmd.Run(args)
	must.Eq(t, 1, code, must.Sprintf("expected exit code 1, got %d: %q", code, ui.ErrorWriter.String()))
	must.Eq(t, "", ui.ErrorWriter.String(), must.Sprintf("expected no stderr output, got:\n%s", ui.ErrorWriter.String()))
}

func TestPlanCommand_From_Files(t *testing.T) {

	// Create a Vault server
	v := testutil.NewTestVault(t)
	defer v.Stop()

	// Create a Nomad server
	s := testutil.NewTestServer(t, func(c *testutil.TestServerConfig) {
		c.Vaults[0].Address = v.HTTPAddr
		c.Vaults[0].Enabled = true
		c.Vaults[0].AllowUnauthenticated = pointer.Of(false)
		c.Vaults[0].Token = v.RootToken
	})
	defer s.Stop()

	t.Run("fail to place", func(t *testing.T) {
		ui := cli.NewMockUi()
		cmd := &JobPlanCommand{Meta: Meta{Ui: ui}}
		args := []string{"-address", "http://" + s.HTTPAddr, "testdata/example-basic.nomad"}
		code := cmd.Run(args)
		must.One(t, code) // no client running, fail to place
		must.StrContains(t, ui.OutputWriter.String(), "WARNING: Failed to place all allocations.")
	})

	t.Run("vault no token", func(t *testing.T) {
		ui := cli.NewMockUi()
		cmd := &JobPlanCommand{Meta: Meta{Ui: ui}}
		args := []string{"-address", "http://" + s.HTTPAddr, "testdata/example-vault.nomad"}
		code := cmd.Run(args)
		must.One(t, code) // no client running, fail to place
	})
}

func TestPlanCommand_From_URL(t *testing.T) {
	ci.Parallel(t)
	ui := cli.NewMockUi()
	cmd := &JobPlanCommand{
		Meta: Meta{Ui: ui},
	}

	args := []string{"https://example.com/foo/bar"}
	if code := cmd.Run(args); code != 255 {
		t.Fatalf("expected exit code 255, got %d: %q", code, ui.ErrorWriter.String())
	}

	if out := ui.ErrorWriter.String(); !strings.Contains(out, "Error getting jobfile") {
		t.Fatalf("expected error getting jobfile, got: %s", out)
	}
}

func TestPlanCommand_Preemptions(t *testing.T) {
	ci.Parallel(t)
	ui := cli.NewMockUi()
	cmd := &JobPlanCommand{Meta: Meta{Ui: ui}}

	// Only one preempted alloc
	resp1 := &api.JobPlanResponse{
		Annotations: &api.PlanAnnotations{
			PreemptedAllocs: []*api.AllocationListStub{
				{
					ID:        "alloc1",
					JobID:     "jobID1",
					TaskGroup: "meta",
					JobType:   "batch",
					Namespace: "test",
				},
			},
		},
	}
	cmd.addPreemptions(resp1)
	out := ui.OutputWriter.String()
	must.StrContains(t, out, "Alloc ID")
	must.StrContains(t, out, "alloc1")

	// Less than 10 unique job ids
	var preemptedAllocs []*api.AllocationListStub
	for i := 0; i < 12; i++ {
		job_id := "job" + strconv.Itoa(i%4)
		alloc := &api.AllocationListStub{
			ID:        "alloc",
			JobID:     job_id,
			TaskGroup: "meta",
			JobType:   "batch",
			Namespace: "test",
		}
		preemptedAllocs = append(preemptedAllocs, alloc)
	}

	resp2 := &api.JobPlanResponse{
		Annotations: &api.PlanAnnotations{
			PreemptedAllocs: preemptedAllocs,
		},
	}
	ui.OutputWriter.Reset()
	cmd.addPreemptions(resp2)
	out = ui.OutputWriter.String()
	must.StrContains(t, out, "Job ID")
	must.StrContains(t, out, "Namespace")

	// More than 10 unique job IDs
	preemptedAllocs = make([]*api.AllocationListStub, 0)
	var job_type string
	for i := 0; i < 20; i++ {
		job_id := "job" + strconv.Itoa(i)
		if i%2 == 0 {
			job_type = "service"
		} else {
			job_type = "batch"
		}
		alloc := &api.AllocationListStub{
			ID:        "alloc",
			JobID:     job_id,
			TaskGroup: "meta",
			JobType:   job_type,
			Namespace: "test",
		}
		preemptedAllocs = append(preemptedAllocs, alloc)
	}

	resp3 := &api.JobPlanResponse{
		Annotations: &api.PlanAnnotations{
			PreemptedAllocs: preemptedAllocs,
		},
	}
	ui.OutputWriter.Reset()
	cmd.addPreemptions(resp3)
	out = ui.OutputWriter.String()
	must.StrContains(t, out, "Job Type")
	must.StrContains(t, out, "batch")
	must.StrContains(t, out, "service")
}

func TestPlanCommand_JSON(t *testing.T) {
	ui := cli.NewMockUi()
	cmd := &JobPlanCommand{
		Meta: Meta{Ui: ui},
	}

	args := []string{
		"-address=http://nope",
		"-json",
		"testdata/example-short.json",
	}
	code := cmd.Run(args)
	must.Eq(t, 255, code)
	must.StrContains(t, ui.ErrorWriter.String(), "Error during plan: Put")
}
