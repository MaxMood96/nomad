// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package scheduler

import (
	"fmt"
	"sort"
	"testing"
	"time"

	memdb "github.com/hashicorp/go-memdb"
	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/hashicorp/nomad/scheduler/feasible"
	"github.com/hashicorp/nomad/scheduler/reconciler"
	"github.com/hashicorp/nomad/scheduler/tests"
	"github.com/shoenig/test/must"
)

func TestSystemSched_JobRegister(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	_ = createNodes(t, h, 10)

	// Create a job
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan does not have annotations but the eval does
	must.Nil(t, plan.Annotations, must.Sprint("expected no annotations"))
	must.SliceNotEmpty(t, h.Evals)
	must.Eq(t, 10, h.Evals[0].PlanAnnotations.DesiredTGUpdates["web"].Place)

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 10, planned)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Len(t, 10, out)

	// Note that all system allocations have the same name derived from Job.Name
	allocNames := helper.ConvertSlice(out,
		func(alloc *structs.Allocation) string { return alloc.Name })
	expectAllocNames := []string{}
	for i := 0; i < 10; i++ {
		expectAllocNames = append(expectAllocNames, fmt.Sprintf("%s.web[0]", job.Name))
	}
	must.SliceContainsAll(t, expectAllocNames, allocNames)

	// Check the available nodes
	count, ok := out[0].Metrics.NodesAvailable["dc1"]
	must.True(t, ok)
	must.Eq(t, 10, count, must.Sprintf("bad metrics %#v:", out[0].Metrics))

	must.Eq(t, 10, out[0].Metrics.NodesInPool,
		must.Sprint("expected NodesInPool metric to be set"))

	// Ensure no allocations are queued
	queued := h.Evals[0].QueuedAllocations["web"]
	must.Eq(t, 0, queued, must.Sprint("unexpected queued allocations"))

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_JobRegister_StickyAllocs(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	_ = createNodes(t, h, 10)

	// Create a job
	job := mock.SystemJob()
	job.TaskGroups[0].EphemeralDisk.Sticky = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	if err := h.Process(NewSystemScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure the plan allocated
	plan := h.Plans[0]
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 10 {
		t.Fatalf("bad: %#v", plan)
	}

	// Get an allocation and mark it as failed
	alloc := planned[4].Copy()
	alloc.ClientStatus = structs.AllocClientStatusFailed
	must.NoError(t, h.State.UpdateAllocsFromClient(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to handle the update
	eval = &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	h1 := tests.NewHarnessWithState(t, h.State)
	if err := h1.Process(NewSystemScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure we have created only one new allocation
	plan = h1.Plans[0]
	var newPlanned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		newPlanned = append(newPlanned, allocList...)
	}
	if len(newPlanned) != 1 {
		t.Fatalf("bad plan: %#v", plan)
	}
	// Ensure that the new allocation was placed on the same node as the older
	// one
	if newPlanned[0].NodeID != alloc.NodeID || newPlanned[0].PreviousAllocation != alloc.ID {
		t.Fatalf("expected: %#v, actual: %#v", alloc, newPlanned[0])
	}
}

func TestSystemSched_JobRegister_EphemeralDiskConstraint(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a job
	job := mock.SystemJob()
	job.TaskGroups[0].EphemeralDisk.SizeMB = 60 * 1024
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create another job with a lot of disk resource ask so that it doesn't fit
	// the node
	job1 := mock.SystemJob()
	job1.TaskGroups[0].EphemeralDisk.SizeMB = 60 * 1024
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	if err := h.Process(NewSystemScheduler, eval); err != nil {
		t.Fatalf("err: %v", err)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 1 {
		t.Fatalf("bad: %#v", out)
	}

	// Create a new harness to test the scheduling result for the second job
	h1 := tests.NewHarnessWithState(t, h.State)
	// Create a mock evaluation to register the job
	eval1 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job1.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job1.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval1}))

	// Process the evaluation
	if err := h1.Process(NewSystemScheduler, eval1); err != nil {
		t.Fatalf("err: %v", err)
	}

	out, err = h1.State.AllocsByJob(ws, job.Namespace, job1.ID, false)
	must.NoError(t, err)
	if len(out) != 0 {
		t.Fatalf("bad: %#v", out)
	}
}

func TestSystemSched_ExhaustResources(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Enable Preemption
	h.State.SchedulerSetConfig(h.NextIndex(), &structs.SchedulerConfiguration{
		PreemptionConfig: structs.PreemptionConfig{
			SystemSchedulerEnabled: true,
		},
	})

	// Create a service job which consumes most of the system resources
	svcJob := mock.Job()
	svcJob.TaskGroups[0].Count = 1
	svcJob.TaskGroups[0].Tasks[0].Resources.CPU = 13500 // mock.Node() has 14k
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, svcJob))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    svcJob.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       svcJob.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	err := h.Process(NewServiceScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Create a system job
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval1 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval1}))
	// Process the evaluation
	if err := h.Process(NewSystemScheduler, eval1); err != nil {
		t.Fatalf("err: %v", err)
	}

	// System scheduler will preempt the service job and would have placed eval1
	newPlan := h.Plans[1]
	must.MapLen(t, 1, newPlan.NodeAllocation)
	must.MapLen(t, 1, newPlan.NodePreemptions)

	for _, allocList := range newPlan.NodeAllocation {
		must.Len(t, 1, allocList)
		must.Eq(t, job.ID, allocList[0].JobID)
	}

	for _, allocList := range newPlan.NodePreemptions {
		must.Len(t, 1, allocList)
		must.Eq(t, svcJob.ID, allocList[0].JobID)
	}
	// Ensure that we have no queued allocations on the second eval
	queued := h.Evals[1].QueuedAllocations["web"]
	if queued != 0 {
		t.Fatalf("expected: %v, actual: %v", 1, queued)
	}
}

func TestSystemSched_JobRegister_Annotate(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	for i := 0; i < 10; i++ {
		node := mock.Node()
		if i < 9 {
			node.NodeClass = "foo"
		} else {
			node.NodeClass = "bar"
		}
		node.ComputeClass()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a job constraining on node class
	job := mock.SystemJob()
	fooConstraint := &structs.Constraint{
		LTarget: "${node.class}",
		RTarget: "foo",
		Operand: "==",
	}
	job.Constraints = append(job.Constraints, fooConstraint)
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:    structs.DefaultNamespace,
		ID:           uuid.Generate(),
		Priority:     job.Priority,
		TriggeredBy:  structs.EvalTriggerJobRegister,
		JobID:        job.ID,
		AnnotatePlan: true,
		Status:       structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != 9 {
		t.Fatalf("bad: %#v %d", planned, len(planned))
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	if len(out) != 9 {
		t.Fatalf("bad: %#v", out)
	}

	// Check the available nodes
	if count, ok := out[0].Metrics.NodesAvailable["dc1"]; !ok || count != 10 {
		t.Fatalf("bad: %#v", out[0].Metrics)
	}
	must.Eq(t, 10, out[0].Metrics.NodesInPool,
		must.Sprint("expected NodesInPool metric to be set"))

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Ensure the plan had annotations.
	if plan.Annotations == nil {
		t.Fatalf("expected annotations")
	}

	desiredTGs := plan.Annotations.DesiredTGUpdates
	if l := len(desiredTGs); l != 1 {
		t.Fatalf("incorrect number of task groups; got %v; want %v", l, 1)
	}

	desiredChanges, ok := desiredTGs["web"]
	if !ok {
		t.Fatalf("expected task group web to have desired changes")
	}

	expected := &structs.DesiredUpdates{Place: 9}
	must.Eq(t, desiredChanges, expected)

}

func TestSystemSched_JobRegister_AddNode(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Add a new node.
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Create a mock evaluation to deal with the node update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan had no node updates
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	must.SliceEmpty(t, update)

	// Ensure the plan allocated on the new node
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 1, planned)

	// Ensure it allocated on the right node
	if _, ok := plan.NodeAllocation[node.ID]; !ok {
		t.Fatalf("allocated on wrong node: %#v", plan)
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	if len(out) != 11 {
		t.Fatalf("bad: %#v", out)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_JobRegister_AllocFail(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create NO nodes
	// Create a job
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure no plan as this should be a no-op.
	if len(h.Plans) != 0 {
		t.Fatalf("bad: %#v", h.Plans)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_JobModify(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Add a few terminal status allocations, these should be ignored
	var terminal []*structs.Allocation
	for i := 0; i < 5; i++ {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = nodes[i].ID
		alloc.Name = "my-job.web[0]"
		alloc.DesiredStatus = structs.AllocDesiredStatusStop
		terminal = append(terminal, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), terminal))

	// Update the job
	job2 := mock.SystemJob()
	job2.ID = job.ID

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	must.Eq(t, len(allocs), len(update))

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 10, planned)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	out, _ = structs.FilterTerminalAllocs(out)
	must.Len(t, 10, out)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_JobModify_Rolling(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.SystemJob()
	job2.ID = job.ID
	job2.Update = structs.UpdateStrategy{
		Stagger:     30 * time.Second,
		MaxParallel: 5,
	}

	// Update the task, such that it cannot be done in-place
	job2.TaskGroups[0].Tasks[0].Config["command"] = "/bin/other"
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Ensure a single plan
	if len(h.Plans) != 1 {
		t.Fatalf("bad: %#v", h.Plans)
	}
	plan := h.Plans[0]

	// Ensure the plan evicted only MaxParallel
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	if len(update) != job2.Update.MaxParallel {
		t.Fatalf("bad: %#v", plan)
	}

	// Ensure the plan allocated
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	if len(planned) != job2.Update.MaxParallel {
		t.Fatalf("bad: %#v", plan)
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Ensure a follow up eval was created
	eval = h.Evals[0]
	if eval.NextEval == "" {
		t.Fatalf("missing next eval")
	}

	// Check for create
	if len(h.CreateEvals) == 0 {
		t.Fatalf("missing created eval")
	}
	create := h.CreateEvals[0]
	if eval.NextEval != create.ID {
		t.Fatalf("ID mismatch")
	}
	if create.PreviousEval != eval.ID {
		t.Fatalf("missing previous eval")
	}

	if create.TriggeredBy != structs.EvalTriggerRollingUpdate {
		t.Fatalf("bad: %#v", create)
	}
}

func TestSystemSched_JobModify_InPlace(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.AllocForNode(node)
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := mock.SystemJob()
	job2.ID = job.ID
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to deal with update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan did not evict any allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	must.SliceEmpty(t, update)

	// Ensure the plan updated the existing allocs
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 10, planned)

	for _, p := range planned {
		must.Eq(t, job2, p.Job, must.Sprint("should update job"))
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Len(t, 10, out)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)

	// Verify the network did not change
	rp := structs.Port{Label: "admin", Value: 5000}
	for _, alloc := range out {
		for _, resources := range alloc.TaskResources {
			must.Eq(t, rp, resources.Networks[0].ReservedPorts[0])
		}
	}
}

func TestSystemSched_JobModify_RemoveDC(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	node1 := mock.Node()
	node1.Datacenter = "dc1"
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node1))

	node2 := mock.Node()
	node2.Datacenter = "dc2"
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	nodes := []*structs.Node{node1, node2}

	// Generate a fake job with allocations
	job := mock.SystemJob()
	job.Datacenters = []string{"dc1", "dc2"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Update the job
	job2 := job.Copy()
	job2.Datacenters = []string{"dc1"}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Create a mock evaluation to deal with update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan did not evict any allocs
	var update []*structs.Allocation
	for _, updateList := range plan.NodeUpdate {
		update = append(update, updateList...)
	}
	must.Len(t, 1, update)

	// Ensure the plan updated the existing allocs
	var planned []*structs.Allocation
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
	}
	must.Len(t, 1, planned)

	for _, p := range planned {
		must.Eq(t, job2, p.Job, must.Sprint("should update job"))
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Len(t, 2, out)
	h.AssertEvalStatus(t, structs.EvalStatusComplete)

}

func TestSystemSched_JobDeregister_Purged(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	for _, alloc := range allocs {
		must.NoError(t, h.State.UpsertJobSummary(h.NextIndex(), mock.JobSummary(alloc.JobID)))
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobDeregister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted the job from all nodes.
	for _, node := range nodes {
		must.Len(t, 1, plan.NodeUpdate[node.ID])
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no remaining allocations
	out, _ = structs.FilterTerminalAllocs(out)
	must.SliceEmpty(t, out)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_JobDeregister_Stopped(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	nodes := createNodes(t, h, 10)

	// Generate a fake job with allocations
	job := mock.SystemJob()
	job.Stop = true
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	var allocs []*structs.Allocation
	for _, node := range nodes {
		alloc := mock.Alloc()
		alloc.Job = job
		alloc.JobID = job.ID
		alloc.NodeID = node.ID
		alloc.Name = "my-job.web[0]"
		allocs = append(allocs, alloc)
	}
	for _, alloc := range allocs {
		must.NoError(t, h.State.UpsertJobSummary(h.NextIndex(), mock.JobSummary(alloc.JobID)))
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), allocs))

	// Create a mock evaluation to deregister the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerJobDeregister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted the job from all nodes.
	for _, node := range nodes {
		must.Len(t, 1, plan.NodeUpdate[node.ID])
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no remaining allocations
	out, _ = structs.FilterTerminalAllocs(out)
	must.SliceEmpty(t, out)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_NodeDown(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a down node
	node := mock.Node()
	node.Status = structs.NodeStatusDown
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job allocated on that node.
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.DesiredTransition.Migrate = pointer.Of(true)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	must.Len(t, 1, plan.NodeUpdate[node.ID])

	// Ensure the plan updated the allocation.
	planned := make([]*structs.Allocation, 0)
	for _, allocList := range plan.NodeUpdate {
		planned = append(planned, allocList...)
	}
	must.Len(t, 1, planned)

	// Ensure the allocations is stopped
	p := planned[0]
	must.Eq(t, structs.AllocDesiredStatusStop, p.DesiredStatus)
	// removed badly designed assertion on client_status = lost
	// the actual client_status is pending

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_NodeDrain_Down(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.DrainNode()
	node.Status = structs.NodeStatusDown
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job allocated on that node.
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to deal with the node update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval) // todo: yikes
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted non terminal allocs
	must.Len(t, 1, plan.NodeUpdate[node.ID])

	// Ensure that the allocation is marked as lost
	var lost []string
	for _, alloc := range plan.NodeUpdate[node.ID] {
		lost = append(lost, alloc.ID)
	}
	must.Eq(t, []string{alloc.ID}, lost)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_NodeDrain(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a draining node
	node := mock.DrainNode()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job allocated on that node.
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.DesiredTransition.Migrate = pointer.Of(true)
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted all allocs
	must.Len(t, 1, plan.NodeUpdate[node.ID])

	// Ensure the plan updated the allocation.
	planned := make([]*structs.Allocation, 0)
	for _, allocList := range plan.NodeUpdate {
		planned = append(planned, allocList...)
	}
	must.Len(t, 1, planned)

	// Ensure the allocations is stopped
	must.Eq(t, structs.AllocDesiredStatusStop, planned[0].DesiredStatus)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_NodeUpdate(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a node
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a fake job allocated on that node.
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))

	// Create a mock evaluation to deal with the node update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure that queued allocations is zero
	val, ok := h.Evals[0].QueuedAllocations["web"]
	must.True(t, ok)
	must.Zero(t, val)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_RetryLimit(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)
	h.Planner = &tests.RejectPlan{h}

	// Create some nodes
	_ = createNodes(t, h, 10)

	// Create a job
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure multiple plans
	must.SliceNotEmpty(t, h.Plans)

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure no allocations placed
	must.SliceEmpty(t, out)

	// Should hit the retry limit
	h.AssertEvalStatus(t, structs.EvalStatusFailed)
}

// This test ensures that the scheduler doesn't increment the queued allocation
// count for a task group when allocations can't be created on currently
// available nodes because of constraint mismatches.
func TestSystemSched_Queued_With_Constraints(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register a node
	node := mock.Node()
	node.Attributes["kernel.name"] = "darwin"
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Generate a system job which can't be placed on the node
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to deal with the node update
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure that queued allocations is zero
	val, ok := h.Evals[0].QueuedAllocations["web"]
	must.True(t, ok)
	must.Zero(t, val)
}

// This test ensures that the scheduler correctly ignores ineligible
// nodes when scheduling due to a new node being added. The job has two
// task groups constrained to a particular node class. The desired behavior
// should be that the TaskGroup constrained to the newly added node class is
// added and that the TaskGroup constrained to the ineligible node is ignored.
func TestSystemSched_JobConstraint_AddNode(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create two nodes
	var node *structs.Node
	node = mock.Node()
	node.NodeClass = "Class-A"
	must.NoError(t, node.ComputeClass())
	must.Nil(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	var nodeB *structs.Node
	nodeB = mock.Node()
	nodeB.NodeClass = "Class-B"
	must.NoError(t, nodeB.ComputeClass())
	must.Nil(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), nodeB))

	// Make a job with two task groups, each constraint to a node class
	job := mock.SystemJob()
	tgA := job.TaskGroups[0]
	tgA.Name = "groupA"
	tgA.Constraints = []*structs.Constraint{
		{
			LTarget: "${node.class}",
			RTarget: node.NodeClass,
			Operand: "=",
		},
	}
	tgB := job.TaskGroups[0].Copy()
	tgB.Name = "groupB"
	tgB.Constraints = []*structs.Constraint{
		{
			LTarget: "${node.class}",
			RTarget: nodeB.NodeClass,
			Operand: "=",
		},
	}

	// Upsert Job
	job.TaskGroups = []*structs.TaskGroup{tgA, tgB}
	must.Nil(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Evaluate the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	must.Nil(t, h.Process(NewSystemScheduler, eval))
	must.Eq(t, "complete", h.Evals[0].Status)

	// QueuedAllocations is drained
	val, ok := h.Evals[0].QueuedAllocations["groupA"]
	must.True(t, ok)
	must.Eq(t, 0, val)

	val, ok = h.Evals[0].QueuedAllocations["groupB"]
	must.True(t, ok)
	must.Eq(t, 0, val)

	// Single plan with two NodeAllocations
	must.Len(t, 1, h.Plans)
	must.MapLen(t, 2, h.Plans[0].NodeAllocation)

	// Mark the node as ineligible
	node.SchedulingEligibility = structs.NodeSchedulingIneligible

	// Evaluate the node update
	eval2 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		NodeID:      node.ID,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval2}))
	must.Nil(t, h.Process(NewSystemScheduler, eval2))
	must.Eq(t, "complete", h.Evals[1].Status)

	// Ensure no new plans
	must.Len(t, 1, h.Plans)

	// Ensure all NodeAllocations are from first Eval
	for _, allocs := range h.Plans[0].NodeAllocation {
		must.Len(t, 1, allocs)
		must.Eq(t, eval.ID, allocs[0].EvalID)
	}

	// Add a new node Class-B
	var nodeBTwo *structs.Node
	nodeBTwo = mock.Node()
	nodeBTwo.NodeClass = "Class-B"
	must.NoError(t, nodeBTwo.ComputeClass())
	must.Nil(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), nodeBTwo))

	// Evaluate the new node
	eval3 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		NodeID:      nodeBTwo.ID,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	// Ensure New eval is complete
	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval3}))
	must.Nil(t, h.Process(NewSystemScheduler, eval3))
	must.Eq(t, "complete", h.Evals[2].Status)

	must.Len(t, 2, h.Plans)
	must.MapLen(t, 1, h.Plans[1].NodeAllocation)
	// Ensure all NodeAllocations are from first Eval
	for _, allocs := range h.Plans[1].NodeAllocation {
		must.Len(t, 1, allocs)
		must.Eq(t, eval3.ID, allocs[0].EvalID)
	}

	ws := memdb.NewWatchSet()

	allocsNodeOne, err := h.State.AllocsByNode(ws, node.ID)
	must.NoError(t, err)
	must.Len(t, 1, allocsNodeOne)

	allocsNodeTwo, err := h.State.AllocsByNode(ws, nodeB.ID)
	must.NoError(t, err)
	must.Len(t, 1, allocsNodeTwo)

	allocsNodeThree, err := h.State.AllocsByNode(ws, nodeBTwo.ID)
	must.NoError(t, err)
	must.Len(t, 1, allocsNodeThree)
}

func TestSystemSched_JobConstraint_AllFiltered(t *testing.T) {
	ci.Parallel(t)
	h := tests.NewHarness(t)

	// Create two nodes, one with a custom class
	node := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	node2 := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a job with a constraint
	job := mock.SystemJob()
	job.Priority = structs.JobDefaultPriority
	fooConstraint := &structs.Constraint{
		LTarget: "${node.unique.name}",
		RTarget: "something-else",
		Operand: "==",
	}
	job.Constraints = []*structs.Constraint{fooConstraint}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to start the job, which will run on the foo node
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single eval
	must.Len(t, 1, h.Evals)
	eval = h.Evals[0]

	// Ensure that no evals report a failed allocation
	must.Eq(t, len(eval.FailedTGAllocs), 1)
	must.Eq(t, eval.FailedTGAllocs[job.TaskGroups[0].Name].ConstraintFiltered[fooConstraint.String()], 2)
}

// Test that the system scheduler can handle a job with a constraint on
// subsequent runs, and report the outcome appropriately
func TestSystemSched_JobConstraint_RunMultipleTimes(t *testing.T) {
	ci.Parallel(t)
	h := tests.NewHarness(t)

	// Create two nodes, one with a custom name
	fooNode := mock.Node()
	fooNode.Name = "foo"
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), fooNode))

	barNode := mock.Node()
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), barNode))

	// Create a job with a constraint on custom name
	job := mock.SystemJob()
	job.Priority = structs.JobDefaultPriority
	fooConstraint := &structs.Constraint{
		LTarget: "${node.unique.name}",
		RTarget: fooNode.Name,
		Operand: "==",
	}
	job.Constraints = []*structs.Constraint{fooConstraint}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to start the job, which will run on the foo node
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Begin rerunning jobs with the constraints to ensure that they return the correct error states
	testCases := []struct {
		desc  string
		job   func() *structs.Job
		check func(*testing.T, *tests.Harness)
	}{
		{
			desc: "Rerunning the job with constraint shouldn't report failed allocations",
			job: func() *structs.Job {
				return job
			},
			check: func(t *testing.T, h *tests.Harness) {
				// Ensure a plan is not added, because no action should be taken
				must.Len(t, 1, h.Plans)

				// Ensure that no evals report a failed allocation
				for _, eval := range h.Evals {
					must.Eq(t, 0, len(eval.FailedTGAllocs))
				}

				// Ensure that plan includes allocation running on fooNode
				must.Len(t, 1, h.Plans[0].NodeAllocation[fooNode.ID])
				// Ensure that plan does not include allocation running on barNode
				must.Len(t, 0, h.Plans[0].NodeAllocation[barNode.ID])
			},
		},
		{
			desc: "Running an inplace-update job with constraint update shouldn't report failed allocations",
			job: func() *structs.Job {
				job := job.Copy()
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
				return job
			},
			check: func(t *testing.T, h *tests.Harness) {
				// Ensure another plan is added for the updated alloc
				must.Len(t, 2, h.Plans)

				// Ensure that evals are not creating an alloc
				must.Len(t, 0, h.CreateEvals)

				// Ensure that no evals report a failed allocation
				for _, eval := range h.Evals {
					must.Eq(t, 0, len(eval.FailedTGAllocs))
				}

				// Ensure that plan still includes allocation running on fooNode
				must.Len(t, 1, h.Plans[1].NodeAllocation[fooNode.ID])
				// Ensure that plan does not include allocation running on barNode
				must.Len(t, 0, h.Plans[1].NodeAllocation[barNode.ID])
			},
		},
		{
			desc: "Running a destructive-update job with constraint that can be placed should not report failed allocations",
			job: func() *structs.Job {
				job := job.Copy()
				// Update the required resources, but not to exceed the node's capacity
				job.TaskGroups[0].Tasks[0].Resources.CPU = 123
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
				return job
			},
			check: func(t *testing.T, h *tests.Harness) {
				// Ensure another plan is added for the updated alloc
				must.Len(t, 3, h.Plans)

				// Ensure that no evals report a failed allocation
				for _, eval := range h.Evals {
					must.Eq(t, 0, len(eval.FailedTGAllocs))
				}

				// Ensure that newest plan includes allocation running on fooNode
				must.Len(t, 1, h.Plans[2].NodeAllocation[fooNode.ID])
				// Ensure that newest plan does not include allocation running on barNode
				must.Len(t, 0, h.Plans[2].NodeAllocation[barNode.ID])
			},
		},
		{
			desc: "Running a destructive-update job with constraint that can't be placed should report failed allocations",
			job: func() *structs.Job {
				job := job.Copy()
				// Update the required resources to exceed the non-constrained node's capacity
				job.TaskGroups[0].Tasks[0].Resources.MemoryMB = 100000000
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
				return job
			},
			check: func(t *testing.T, h *tests.Harness) {
				// Ensure another Plan for the updated alloc
				must.Len(t, 4, h.Plans)

				eval := h.Evals[4]
				// Ensure that this eval reports failed allocation, where the running alloc can no longer be run on the node
				must.Eq(t, 1, len(eval.FailedTGAllocs))
				must.Eq(t, eval.FailedTGAllocs[job.TaskGroups[0].Name].NodesExhausted, 1)
			},
		},
	}

	for _, tc := range testCases {
		job := tc.job()

		testEval := &structs.Evaluation{
			Namespace:   structs.DefaultNamespace,
			ID:          uuid.Generate(),
			Priority:    job.Priority,
			TriggeredBy: structs.EvalTriggerJobRegister,
			JobID:       job.ID,
			Status:      structs.EvalStatusPending,
		}
		must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{testEval}))

		err = h.Process(NewSystemScheduler, testEval)
		must.NoError(t, err)

		tc.check(t, h)
	}
}

// No errors reported when no available nodes prevent placement
func TestSystemSched_ExistingAllocNoNodes(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	var node *structs.Node
	// Create a node
	node = mock.Node()
	must.NoError(t, node.ComputeClass())
	must.Nil(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	// Make a job
	job := mock.SystemJob()
	must.Nil(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Evaluate the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	must.Nil(t, h.Process(NewSystemScheduler, eval))
	must.Eq(t, "complete", h.Evals[0].Status)

	// QueuedAllocations is drained
	val, ok := h.Evals[0].QueuedAllocations["web"]
	must.True(t, ok)
	must.Eq(t, 0, val)

	// The plan has one NodeAllocations
	must.Eq(t, 1, len(h.Plans))

	// Mark the node as ineligible
	node.SchedulingEligibility = structs.NodeSchedulingIneligible

	// Evaluate the job
	eval2 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval2}))
	must.Nil(t, h.Process(NewSystemScheduler, eval2))
	must.Eq(t, "complete", h.Evals[1].Status)

	// Create a new job version, deploy
	job2 := job.Copy()
	job2.Meta["version"] = "2"
	must.Nil(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	// Run evaluation as a plan
	eval3 := &structs.Evaluation{
		Namespace:    structs.DefaultNamespace,
		ID:           uuid.Generate(),
		Priority:     job2.Priority,
		TriggeredBy:  structs.EvalTriggerJobRegister,
		JobID:        job2.ID,
		Status:       structs.EvalStatusPending,
		AnnotatePlan: true,
	}

	// Ensure New eval is complete
	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval3}))
	must.Nil(t, h.Process(NewSystemScheduler, eval3))
	must.Eq(t, "complete", h.Evals[2].Status)

	// Ensure there are no FailedTGAllocs
	must.Eq(t, 0, len(h.Evals[2].FailedTGAllocs))
	must.Eq(t, 0, h.Evals[2].QueuedAllocations[job2.Name])
}

// No errors reported when constraints prevent placement
func TestSystemSched_ConstraintErrors(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	var node *structs.Node
	// Register some nodes
	// the tag "aaaaaa" is hashed so that the nodes are processed
	// in an order other than good, good, bad
	for _, tag := range []string{"aaaaaa", "foo", "foo", "foo"} {
		node = mock.Node()
		node.Meta["tag"] = tag
		must.NoError(t, node.ComputeClass())
		must.Nil(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Mark the last node as ineligible
	node.SchedulingEligibility = structs.NodeSchedulingIneligible

	// Make a job with a constraint that matches a subset of the nodes
	job := mock.SystemJob()
	job.Constraints = append(job.Constraints,
		&structs.Constraint{
			LTarget: "${meta.tag}",
			RTarget: "foo",
			Operand: "=",
		})

	must.Nil(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Evaluate the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.Nil(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))
	must.Nil(t, h.Process(NewSystemScheduler, eval))
	must.Eq(t, "complete", h.Evals[0].Status)

	// QueuedAllocations is drained
	val, ok := h.Evals[0].QueuedAllocations["web"]
	must.True(t, ok)
	must.Eq(t, 0, val)

	// The plan has two NodeAllocations
	must.Eq(t, 1, len(h.Plans))
	must.Nil(t, h.Plans[0].Annotations)
	must.Eq(t, 2, len(h.Plans[0].NodeAllocation))

	// Two nodes were allocated and are running
	ws := memdb.NewWatchSet()
	as, err := h.State.AllocsByJob(ws, structs.DefaultNamespace, job.ID, false)
	must.NoError(t, err)

	running := 0
	for _, a := range as {
		if "running" == a.Job.Status {
			running++
		}
	}

	must.Eq(t, 2, len(as))
	must.Eq(t, 2, running)

	// Failed allocations is empty
	must.Eq(t, 0, len(h.Evals[0].FailedTGAllocs))
}

func TestSystemSched_ChainedAlloc(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Create some nodes
	_ = createNodes(t, h, 10)

	// Create a job
	job := mock.SystemJob()
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	var allocIDs []string
	for _, allocList := range h.Plans[0].NodeAllocation {
		for _, alloc := range allocList {
			allocIDs = append(allocIDs, alloc.ID)
		}
	}
	sort.Strings(allocIDs)

	// Create a new harness to invoke the scheduler again
	h1 := tests.NewHarnessWithState(t, h.State)
	job1 := mock.SystemJob()
	job1.ID = job.ID
	job1.TaskGroups[0].Tasks[0].Env = make(map[string]string)
	job1.TaskGroups[0].Tasks[0].Env["foo"] = "bar"
	must.NoError(t, h1.State.UpsertJob(structs.MsgTypeTestSetup, h1.NextIndex(), nil, job1))

	// Insert two more nodes
	for i := 0; i < 2; i++ {
		node := mock.Node()
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// Create a mock evaluation to update the job
	eval1 := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job1.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job1.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval1}))
	// Process the evaluation
	if err := h1.Process(NewSystemScheduler, eval1); err != nil {
		t.Fatalf("err: %v", err)
	}

	must.Len(t, 1, h.Plans)
	plan := h1.Plans[0]

	// Collect all the chained allocation ids and the new allocations which
	// don't have any chained allocations
	var prevAllocs []string
	var newAllocs []string
	for _, allocList := range plan.NodeAllocation {
		for _, alloc := range allocList {
			if alloc.PreviousAllocation == "" {
				newAllocs = append(newAllocs, alloc.ID)
				continue
			}
			prevAllocs = append(prevAllocs, alloc.PreviousAllocation)
		}
	}
	sort.Strings(prevAllocs)

	// Ensure that the new allocations has their corresponding original
	// allocation ids
	must.Eq(t, allocIDs, prevAllocs)

	// Ensuring two new allocations don't have any chained allocations
	must.Len(t, 2, newAllocs)
}

func TestSystemSched_PlanWithDrainedNode(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register two nodes with two different classes
	node := mock.DrainNode()
	node.NodeClass = "green"
	must.NoError(t, node.ComputeClass())
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	node2 := mock.Node()
	node2.NodeClass = "blue"
	must.NoError(t, node2.ComputeClass())
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a Job with two task groups, each constrained on node class
	job := mock.SystemJob()
	tg1 := job.TaskGroups[0]
	tg1.Constraints = append(tg1.Constraints,
		&structs.Constraint{
			LTarget: "${node.class}",
			RTarget: "green",
			Operand: "==",
		})

	tg2 := tg1.Copy()
	tg2.Name = "web2"
	tg2.Constraints[0].RTarget = "blue"
	job.TaskGroups = append(job.TaskGroups, tg2)
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create an allocation on each node
	alloc := mock.Alloc()
	alloc.Job = job
	alloc.JobID = job.ID
	alloc.NodeID = node.ID
	alloc.Name = "my-job.web[0]"
	alloc.DesiredTransition.Migrate = pointer.Of(true)
	alloc.TaskGroup = "web"

	alloc2 := mock.Alloc()
	alloc2.Job = job
	alloc2.JobID = job.ID
	alloc2.NodeID = node2.ID
	alloc2.Name = "my-job.web2[0]"
	alloc2.TaskGroup = "web2"
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc, alloc2}))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)
	plan := h.Plans[0]

	// Ensure the plan evicted the alloc on the failed node
	planned := plan.NodeUpdate[node.ID]
	must.Len(t, 1, plan.NodeUpdate[node.ID])

	// Ensure the plan didn't place
	must.MapEmpty(t, plan.NodeAllocation)

	// Ensure the allocations is stopped
	must.Eq(t, structs.AllocDesiredStatusStop, planned[0].DesiredStatus)

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_QueuedAllocsMultTG(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	// Register two nodes with two different classes
	node := mock.Node()
	node.NodeClass = "green"
	must.NoError(t, node.ComputeClass())
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

	node2 := mock.Node()
	node2.NodeClass = "blue"
	must.NoError(t, node2.ComputeClass())
	must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node2))

	// Create a Job with two task groups, each constrained on node class
	job := mock.SystemJob()
	tg1 := job.TaskGroups[0]
	tg1.Constraints = append(tg1.Constraints,
		&structs.Constraint{
			LTarget: "${node.class}",
			RTarget: "green",
			Operand: "==",
		})

	tg2 := tg1.Copy()
	tg2.Name = "web2"
	tg2.Constraints[0].RTarget = "blue"
	job.TaskGroups = append(job.TaskGroups, tg2)
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to deal with drain
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    50,
		TriggeredBy: structs.EvalTriggerNodeUpdate,
		JobID:       job.ID,
		NodeID:      node.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Len(t, 1, h.Plans)

	qa := h.Evals[0].QueuedAllocations
	must.Zero(t, qa["pinger"])
	must.Zero(t, qa["pinger2"])

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_Preemption(t *testing.T) {
	ci.Parallel(t)

	h := tests.NewHarness(t)

	legacyCpuResources, processorResources := tests.CpuResources(3072)

	// Create nodes
	nodes := make([]*structs.Node, 0)
	for i := 0; i < 2; i++ {
		node := mock.Node()
		// TODO: remove in 0.11
		node.Resources = &structs.Resources{
			CPU:      3072,
			MemoryMB: 5034,
			DiskMB:   20 * 1024,
			Networks: []*structs.NetworkResource{{
				Device: "eth0",
				CIDR:   "192.168.0.100/32",
				MBits:  1000,
			}},
		}
		node.NodeResources = &structs.NodeResources{
			Processors: processorResources,
			Cpu:        legacyCpuResources,
			Memory:     structs.NodeMemoryResources{MemoryMB: 5034},
			Disk:       structs.NodeDiskResources{DiskMB: 20 * 1024},
			Networks: []*structs.NetworkResource{{
				Device: "eth0",
				CIDR:   "192.168.0.100/32",
				MBits:  1000,
			}},
			NodeNetworks: []*structs.NodeNetworkResource{{
				Mode:   "host",
				Device: "eth0",
				Addresses: []structs.NodeNetworkAddress{{
					Family:  structs.NodeNetworkAF_IPv4,
					Alias:   "default",
					Address: "192.168.0.100",
				}},
			}},
		}
		must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))
		nodes = append(nodes, node)
	}

	// Enable Preemption
	err := h.State.SchedulerSetConfig(h.NextIndex(), &structs.SchedulerConfiguration{
		PreemptionConfig: structs.PreemptionConfig{
			SystemSchedulerEnabled: true,
		},
	})
	must.NoError(t, err)

	// Create some low priority batch jobs and allocations for them
	// One job uses a reserved port
	job1 := mock.BatchJob()
	job1.Type = structs.JobTypeBatch
	job1.Priority = 20
	job1.TaskGroups[0].Tasks[0].Resources = &structs.Resources{
		CPU:      512,
		MemoryMB: 1024,
		Networks: []*structs.NetworkResource{{
			MBits: 200,
			ReservedPorts: []structs.Port{{
				Label: "web",
				Value: 80,
			}},
		}},
	}

	alloc1 := mock.Alloc()
	alloc1.Job = job1
	alloc1.JobID = job1.ID
	alloc1.NodeID = nodes[0].ID
	alloc1.Name = "my-job[0]"
	alloc1.TaskGroup = job1.TaskGroups[0].Name
	alloc1.AllocatedResources = &structs.AllocatedResources{
		Tasks: map[string]*structs.AllocatedTaskResources{
			"web": {
				Cpu:    structs.AllocatedCpuResources{CpuShares: 512},
				Memory: structs.AllocatedMemoryResources{MemoryMB: 1024},
				Networks: []*structs.NetworkResource{{
					Device: "eth0",
					IP:     "192.168.0.100",
					MBits:  200,
				}},
			},
		},
		Shared: structs.AllocatedSharedResources{DiskMB: 5 * 1024},
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job1))

	job2 := mock.BatchJob()
	job2.Type = structs.JobTypeBatch
	job2.Priority = 20
	job2.TaskGroups[0].Tasks[0].Resources = &structs.Resources{
		CPU:      512,
		MemoryMB: 1024,
		Networks: []*structs.NetworkResource{{MBits: 200}},
	}

	alloc2 := mock.Alloc()
	alloc2.Job = job2
	alloc2.JobID = job2.ID
	alloc2.NodeID = nodes[0].ID
	alloc2.Name = "my-job[2]"
	alloc2.TaskGroup = job2.TaskGroups[0].Name
	alloc2.AllocatedResources = &structs.AllocatedResources{
		Tasks: map[string]*structs.AllocatedTaskResources{
			"web": {
				Cpu:    structs.AllocatedCpuResources{CpuShares: 512},
				Memory: structs.AllocatedMemoryResources{MemoryMB: 1024},
				Networks: []*structs.NetworkResource{{
					Device: "eth0",
					IP:     "192.168.0.100",
					MBits:  200,
				}},
			},
		},
		Shared: structs.AllocatedSharedResources{DiskMB: 5 * 1024},
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job2))

	job3 := mock.Job()
	job3.Type = structs.JobTypeBatch
	job3.Priority = 40
	job3.TaskGroups[0].Tasks[0].Resources = &structs.Resources{
		CPU:      1024,
		MemoryMB: 2048,
		Networks: []*structs.NetworkResource{{
			Device: "eth0",
			MBits:  400,
		}},
	}

	alloc3 := mock.Alloc()
	alloc3.Job = job3
	alloc3.JobID = job3.ID
	alloc3.NodeID = nodes[0].ID
	alloc3.Name = "my-job[0]"
	alloc3.TaskGroup = job3.TaskGroups[0].Name
	alloc3.AllocatedResources = &structs.AllocatedResources{
		Tasks: map[string]*structs.AllocatedTaskResources{
			"web": {
				Cpu:    structs.AllocatedCpuResources{CpuShares: 1024},
				Memory: structs.AllocatedMemoryResources{MemoryMB: 25},
				Networks: []*structs.NetworkResource{{
					Device: "eth0",
					IP:     "192.168.0.100",
					MBits:  400,
				}},
			},
		},
		Shared: structs.AllocatedSharedResources{DiskMB: 5 * 1024},
	}
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc1, alloc2, alloc3}))

	// Create a high priority job and allocs for it
	// These allocs should not be preempted

	job4 := mock.BatchJob()
	job4.Type = structs.JobTypeBatch
	job4.Priority = 100
	job4.TaskGroups[0].Tasks[0].Resources = &structs.Resources{
		CPU:      1024,
		MemoryMB: 2048,
		Networks: []*structs.NetworkResource{{MBits: 100}},
	}

	alloc4 := mock.Alloc()
	alloc4.Job = job4
	alloc4.JobID = job4.ID
	alloc4.NodeID = nodes[0].ID
	alloc4.Name = "my-job4[0]"
	alloc4.TaskGroup = job4.TaskGroups[0].Name
	alloc4.AllocatedResources = &structs.AllocatedResources{
		Tasks: map[string]*structs.AllocatedTaskResources{
			"web": {
				Cpu: structs.AllocatedCpuResources{
					CpuShares: 1024,
				},
				Memory: structs.AllocatedMemoryResources{
					MemoryMB: 2048,
				},
				Networks: []*structs.NetworkResource{
					{
						Device:        "eth0",
						IP:            "192.168.0.100",
						ReservedPorts: []structs.Port{{Label: "web", Value: 80}},
						MBits:         100,
					},
				},
			},
		},
		Shared: structs.AllocatedSharedResources{
			DiskMB: 2 * 1024,
		},
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job4))
	must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc4}))

	// Create a system job such that it would need to preempt both allocs to succeed
	job := mock.SystemJob()
	job.TaskGroups[0].Tasks[0].Resources = &structs.Resources{
		CPU:      1948,
		MemoryMB: 256,
		Networks: []*structs.NetworkResource{{
			MBits:        800,
			DynamicPorts: []structs.Port{{Label: "http"}},
		}},
	}
	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   structs.DefaultNamespace,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}
	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation
	err = h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	// Ensure a single plan
	must.Eq(t, 1, len(h.Plans))
	plan := h.Plans[0]

	// Ensure the plan doesn't have annotations
	must.Nil(t, plan.Annotations)

	// Ensure the plan allocated on both nodes
	var planned []*structs.Allocation
	preemptingAllocId := ""
	must.Eq(t, 2, len(plan.NodeAllocation))

	// The alloc that got placed on node 1 is the preemptor
	for _, allocList := range plan.NodeAllocation {
		planned = append(planned, allocList...)
		for _, alloc := range allocList {
			if alloc.NodeID == nodes[0].ID {
				preemptingAllocId = alloc.ID
			}
		}
	}

	// Lookup the allocations by JobID
	ws := memdb.NewWatchSet()
	out, err := h.State.AllocsByJob(ws, job.Namespace, job.ID, false)
	must.NoError(t, err)

	// Ensure all allocations placed
	must.Eq(t, 2, len(out))

	// Verify that one node has preempted allocs
	must.NotNil(t, plan.NodePreemptions[nodes[0].ID])
	preemptedAllocs := plan.NodePreemptions[nodes[0].ID]

	// Verify that three jobs have preempted allocs
	must.Eq(t, 3, len(preemptedAllocs))

	expectedPreemptedJobIDs := []string{job1.ID, job2.ID, job3.ID}

	// We expect job1, job2 and job3 to have preempted allocations
	// job4 should not have any allocs preempted
	for _, alloc := range preemptedAllocs {
		must.SliceContains(t, expectedPreemptedJobIDs, alloc.JobID)
	}
	// Look up the preempted allocs by job ID
	ws = memdb.NewWatchSet()

	for _, jobId := range expectedPreemptedJobIDs {
		out, err = h.State.AllocsByJob(ws, structs.DefaultNamespace, jobId, false)
		must.NoError(t, err)
		for _, alloc := range out {
			must.Eq(t, structs.AllocDesiredStatusEvict, alloc.DesiredStatus)
			must.Eq(t, fmt.Sprintf("Preempted by alloc ID %v", preemptingAllocId), alloc.DesiredDescription)
		}
	}

	h.AssertEvalStatus(t, structs.EvalStatusComplete)
}

func TestSystemSched_canHandle(t *testing.T) {
	ci.Parallel(t)

	s := SystemScheduler{sysbatch: false}
	t.Run("system register", func(t *testing.T) {
		must.True(t, s.canHandle(structs.EvalTriggerJobRegister))
	})
	t.Run("system scheduled", func(t *testing.T) {
		must.False(t, s.canHandle(structs.EvalTriggerScheduled))
	})
	t.Run("system periodic", func(t *testing.T) {
		must.False(t, s.canHandle(structs.EvalTriggerPeriodicJob))
	})
}

func TestSystemSched_NodeDisconnected(t *testing.T) {
	ci.Parallel(t)

	systemJob := mock.SystemJob()
	systemAlloc := mock.SystemAlloc()
	systemAlloc.Name = fmt.Sprintf("my-job.%s[0]", systemJob.TaskGroups[0].Name)

	sysBatchJob := mock.SystemBatchJob()
	sysBatchJob.TaskGroups[0].Tasks[0].Env = make(map[string]string)
	sysBatchJob.TaskGroups[0].Tasks[0].Env["foo"] = "bar"
	sysBatchAlloc := mock.SysBatchAlloc()
	sysBatchAlloc.Name = fmt.Sprintf("my-sysbatch.%s[0]", sysBatchJob.TaskGroups[0].Name)

	now := time.Now().UTC()

	unknownAllocState := []*structs.AllocState{{
		Field: structs.AllocStateFieldClientStatus,
		Value: structs.AllocClientStatusUnknown,
		Time:  now,
	}}

	expiredAllocState := []*structs.AllocState{{
		Field: structs.AllocStateFieldClientStatus,
		Value: structs.AllocClientStatusUnknown,
		Time:  now.Add(-60 * time.Second),
	}}

	reconnectedAllocState := []*structs.AllocState{
		{
			Field: structs.AllocStateFieldClientStatus,
			Value: structs.AllocClientStatusUnknown,
			Time:  now.Add(-60 * time.Second),
		},
		{
			Field: structs.AllocStateFieldClientStatus,
			Value: structs.AllocClientStatusRunning,
			Time:  now,
		},
	}

	successTaskState := map[string]*structs.TaskState{
		systemJob.TaskGroups[0].Tasks[0].Name: {
			State:  structs.TaskStateDead,
			Failed: false,
		},
	}

	type testCase struct {
		name                   string
		jobType                string
		exists                 bool
		required               bool
		migrate                bool
		draining               bool
		targeted               bool
		modifyJob              bool
		previousTerminal       bool
		nodeStatus             string
		clientStatus           string
		desiredStatus          string
		allocState             []*structs.AllocState
		taskState              map[string]*structs.TaskState
		expectedPlanCount      int
		expectedNodeAllocation map[string]*structs.Allocation
		expectedNodeUpdate     map[string]*structs.Allocation
	}

	testCases := []testCase{
		{
			name:              "system-running-disconnect",
			jobType:           structs.JobTypeSystem,
			exists:            true,
			required:          true,
			nodeStatus:        structs.NodeStatusDisconnected,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         false,
			previousTerminal:  false,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusUnknown,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: nil,
		},
		{
			name:                   "system-running-reconnect",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             reconnectedAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "system-unknown-expired",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusUnknown,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             expiredAllocState,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusLost,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "system-migrate",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                true,
			draining:               true,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:              "sysbatch-running-unknown",
			jobType:           structs.JobTypeSysBatch,
			required:          true,
			exists:            true,
			nodeStatus:        structs.NodeStatusDisconnected,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         false,
			previousTerminal:  false,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusUnknown,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: nil,
		},
		{
			name:                   "system-ignore-unknown",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusUnknown,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             unknownAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-ignore-unknown",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusUnknown,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             unknownAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-ignore-complete-disconnected",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusComplete,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             unknownAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-running-reconnect",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             reconnectedAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-failed-reconnect",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusFailed,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             reconnectedAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-complete-reconnect",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusComplete,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             reconnectedAllocState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-unknown-expired",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusUnknown,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             expiredAllocState,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusLost,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "sysbatch-migrate",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                true,
			draining:               true,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "system-stopped",
			jobType:                structs.JobTypeSysBatch,
			required:               false,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "system-lost",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusLost,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "sysbatch-lost",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusLost,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "system-node-draining",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               true,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-node-draining",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               true,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "system-node-down-complete",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusComplete,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-node-down-complete",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusComplete,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-ignore-terminal",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusEvict,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "system-ignore-ineligible",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusPending,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "sysbatch-ignore-ineligible",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDisconnected,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusPending,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:                   "system-stop-not-targeted",
			jobType:                structs.JobTypeSystem,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               false,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "sysbatch-stop-not-targeted",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               false,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      1,
			expectedNodeAllocation: nil,
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:              "system-update-job-version",
			jobType:           structs.JobTypeSystem,
			required:          true,
			exists:            true,
			nodeStatus:        structs.NodeStatusReady,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         true,
			previousTerminal:  false,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusPending,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:              "sysbatch-update-job-version",
			jobType:           structs.JobTypeSysBatch,
			required:          true,
			exists:            true,
			nodeStatus:        structs.NodeStatusReady,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         true,
			previousTerminal:  false,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusPending,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusRunning,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "sysbatch-ignore-successful-tainted",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 true,
			nodeStatus:             structs.NodeStatusDown,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       false,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			taskState:              successTaskState,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
		{
			name:              "sysbatch-annotate-when-not-existing",
			jobType:           structs.JobTypeSysBatch,
			required:          true,
			exists:            false,
			nodeStatus:        structs.NodeStatusReady,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         false,
			previousTerminal:  false,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusPending,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: nil,
		},
		{
			name:              "sysbatch-update-modified-terminal-when-not-existing",
			jobType:           structs.JobTypeSysBatch,
			required:          true,
			exists:            false,
			nodeStatus:        structs.NodeStatusReady,
			migrate:           false,
			draining:          false,
			targeted:          true,
			modifyJob:         true,
			previousTerminal:  true,
			clientStatus:      structs.AllocClientStatusRunning,
			desiredStatus:     structs.AllocDesiredStatusRun,
			allocState:        nil,
			expectedPlanCount: 1,
			expectedNodeAllocation: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusPending,
					DesiredStatus: structs.AllocDesiredStatusRun,
				},
			},
			expectedNodeUpdate: map[string]*structs.Allocation{
				"id": {
					ClientStatus:  structs.AllocClientStatusComplete,
					DesiredStatus: structs.AllocDesiredStatusStop,
				},
			},
		},
		{
			name:                   "sysbatch-ignore-unmodified-terminal-when-not-existing",
			jobType:                structs.JobTypeSysBatch,
			required:               true,
			exists:                 false,
			nodeStatus:             structs.NodeStatusReady,
			migrate:                false,
			draining:               false,
			targeted:               true,
			modifyJob:              false,
			previousTerminal:       true,
			clientStatus:           structs.AllocClientStatusRunning,
			desiredStatus:          structs.AllocDesiredStatusRun,
			allocState:             nil,
			expectedPlanCount:      0,
			expectedNodeAllocation: nil,
			expectedNodeUpdate:     nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			h := tests.NewHarness(t)

			// Register a node
			node := mock.Node()
			node.Status = tc.nodeStatus

			if tc.draining {
				node.SchedulingEligibility = structs.NodeSchedulingIneligible
			}

			must.NoError(t, h.State.UpsertNode(structs.MsgTypeTestSetup, h.NextIndex(), node))

			// Generate a fake job allocated on that node.
			var job *structs.Job
			var alloc *structs.Allocation
			switch tc.jobType {
			case structs.JobTypeSystem:
				job = systemJob.Copy()
				alloc = systemAlloc.Copy()
			case structs.JobTypeSysBatch:
				job = sysBatchJob.Copy()
				alloc = sysBatchAlloc.Copy()
			default:
				t.Fatalf("invalid jobType")
			}

			job.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
				LostAfter: 5 * time.Second,
			}

			if !tc.required {
				job.Stop = true
			}

			// If we are no longer on a targeted node, change it to a non-targeted datacenter
			if !tc.targeted {
				job.Datacenters = []string{"not-targeted"}
			}

			must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

			alloc.Job = job.Copy()
			alloc.JobID = job.ID
			alloc.NodeID = node.ID
			alloc.TaskGroup = job.TaskGroups[0].Name
			alloc.ClientStatus = tc.clientStatus
			alloc.DesiredStatus = tc.desiredStatus
			alloc.DesiredTransition.Migrate = pointer.Of(tc.migrate)
			alloc.AllocStates = tc.allocState
			alloc.TaskStates = tc.taskState

			if tc.exists {
				must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{alloc}))
			}

			if tc.modifyJob {
				if tc.jobType == structs.JobTypeSystem {
					job.TaskGroups[0].Tasks[0].Resources.Networks[0].DynamicPorts = []structs.Port{{Label: "grpc"}}
				}
				if tc.jobType == structs.JobTypeSysBatch {
					alloc.Job.TaskGroups[0].Tasks[0].Driver = "raw_exec"
				}
				must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))
			}

			if tc.previousTerminal {
				prev := alloc.Copy()
				if tc.modifyJob {
					prev.Job.JobModifyIndex = alloc.Job.JobModifyIndex - 1
				}
				prev.ClientStatus = structs.AllocClientStatusComplete
				prev.DesiredStatus = structs.AllocDesiredStatusRun

				must.NoError(t, h.State.UpsertAllocs(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Allocation{prev}))
			}
			// Create a mock evaluation to deal with disconnect
			eval := &structs.Evaluation{
				Namespace:   structs.DefaultNamespace,
				ID:          uuid.Generate(),
				Priority:    50,
				TriggeredBy: structs.EvalTriggerNodeUpdate,
				JobID:       job.ID,
				NodeID:      node.ID,
				Status:      structs.EvalStatusPending,
			}
			must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup, h.NextIndex(), []*structs.Evaluation{eval}))

			// Process the evaluation
			err := h.Process(NewSystemScheduler, eval)
			must.NoError(t, err)

			// Ensure a single plan
			must.Len(t, tc.expectedPlanCount, h.Plans)
			if tc.expectedPlanCount == 0 {
				return
			}

			plan := h.Plans[0]

			// Ensure the plan creates the expected plan
			must.Len(t, len(tc.expectedNodeAllocation), plan.NodeAllocation[node.ID])
			must.Len(t, len(tc.expectedNodeUpdate), plan.NodeUpdate[node.ID])

			foundMatch := false

			for _, plannedNodeAllocs := range plan.NodeAllocation {
				for _, actual := range plannedNodeAllocs {
					for _, expected := range tc.expectedNodeAllocation {
						if expected.ClientStatus == actual.ClientStatus &&
							expected.DesiredStatus == actual.DesiredStatus {
							foundMatch = true
							break
						}
					}
				}
			}

			if len(tc.expectedNodeAllocation) > 0 {
				must.True(t, foundMatch, must.Sprint("NodeAllocation did not match"))
			}

			foundMatch = false
			for _, plannedNodeUpdates := range plan.NodeUpdate {
				for _, actual := range plannedNodeUpdates {
					for _, expected := range tc.expectedNodeUpdate {
						if expected.ClientStatus == actual.ClientStatus &&
							expected.DesiredStatus == actual.DesiredStatus {
							foundMatch = true
							break
						}
					}
				}
			}

			if len(tc.expectedNodeUpdate) > 0 {
				must.True(t, foundMatch, must.Sprint("NodeUpdate did not match"))
			}

			h.AssertEvalStatus(t, structs.EvalStatusComplete)
		})
	}
}

func TestSystemSched_CSITopology(t *testing.T) {
	ci.Parallel(t)
	h := tests.NewHarness(t)

	zones := []string{"zone-0", "zone-1", "zone-2", "zone-3"}

	// Create some nodes, each running a CSI plugin with topology for
	// a different "zone"
	for i := 0; i < 12; i++ {
		node := mock.Node()
		node.Datacenter = zones[i%4]
		node.CSINodePlugins = map[string]*structs.CSIInfo{
			"test-plugin-" + zones[i%4]: {
				PluginID: "test-plugin-" + zones[i%4],
				Healthy:  true,
				NodeInfo: &structs.CSINodeInfo{
					MaxVolumes: 3,
					AccessibleTopology: &structs.CSITopology{
						Segments: map[string]string{"zone": zones[i%4]}},
				},
			},
		}
		must.NoError(t, h.State.UpsertNode(
			structs.MsgTypeTestSetup, h.NextIndex(), node))
	}

	// create a non-default namespace for the job and volume
	ns := "non-default-namespace"
	must.NoError(t, h.State.UpsertNamespaces(h.NextIndex(),
		[]*structs.Namespace{{Name: ns}}))

	// create a volume that lives in one zone
	vol0 := structs.NewCSIVolume("myvolume", 0)
	vol0.PluginID = "test-plugin-zone-0"
	vol0.Namespace = ns
	vol0.AccessMode = structs.CSIVolumeAccessModeMultiNodeMultiWriter
	vol0.AttachmentMode = structs.CSIVolumeAttachmentModeFilesystem
	vol0.RequestedTopologies = &structs.CSITopologyRequest{
		Required: []*structs.CSITopology{{
			Segments: map[string]string{"zone": "zone-0"},
		}},
	}

	must.NoError(t, h.State.UpsertCSIVolume(
		h.NextIndex(), []*structs.CSIVolume{vol0}))

	// Create a job that uses that volumes
	job := mock.SystemJob()
	job.Datacenters = zones
	job.Namespace = ns
	job.TaskGroups[0].Volumes = map[string]*structs.VolumeRequest{
		"myvolume": {
			Type:   "csi",
			Name:   "unique",
			Source: "myvolume",
		},
	}

	must.NoError(t, h.State.UpsertJob(structs.MsgTypeTestSetup, h.NextIndex(), nil, job))

	// Create a mock evaluation to register the job
	eval := &structs.Evaluation{
		Namespace:   ns,
		ID:          uuid.Generate(),
		Priority:    job.Priority,
		TriggeredBy: structs.EvalTriggerJobRegister,
		JobID:       job.ID,
		Status:      structs.EvalStatusPending,
	}

	must.NoError(t, h.State.UpsertEvals(structs.MsgTypeTestSetup,
		h.NextIndex(), []*structs.Evaluation{eval}))

	// Process the evaluation and expect a single plan without annotations
	err := h.Process(NewSystemScheduler, eval)
	must.NoError(t, err)

	must.Len(t, 1, h.Plans, must.Sprint("expected one plan"))
	must.Nil(t, h.Plans[0].Annotations, must.Sprint("expected no annotations"))

	// Expect the eval has not spawned a blocked eval
	must.Eq(t, len(h.CreateEvals), 0)
	must.Eq(t, "", h.Evals[0].BlockedEval, must.Sprint("did not expect a blocked eval"))
	must.Eq(t, structs.EvalStatusComplete, h.Evals[0].Status)

}

func TestEvictAndPlace_LimitLessThanAllocs(t *testing.T) {
	ci.Parallel(t)

	_, ctx := feasible.MockContext(t)
	allocs := []reconciler.AllocTuple{
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
	}
	diff := &reconciler.NodeReconcileResult{}

	limit := 2
	must.True(t, evictAndPlace(ctx, diff, allocs, "", &limit),
		must.Sprintf("evictAndReplace() should have returned true"))
	must.Zero(t, limit,
		must.Sprint("evictAndReplace() should decrement limit"))
	must.Len(t, 2, diff.Place,
		must.Sprintf("evictAndReplace() didn't insert into diffResult properly: %v", diff.Place))
}

func TestEvictAndPlace_LimitEqualToAllocs(t *testing.T) {
	ci.Parallel(t)

	_, ctx := feasible.MockContext(t)
	allocs := []reconciler.AllocTuple{
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
	}
	diff := &reconciler.NodeReconcileResult{}

	limit := 4
	must.False(t, evictAndPlace(ctx, diff, allocs, "", &limit),
		must.Sprint("evictAndReplace() should have returned false"))
	must.Zero(t, limit, must.Sprint("evictAndReplace() should decrement limit"))
	must.Len(t, 4, diff.Place,
		must.Sprintf("evictAndReplace() didn't insert into diffResult properly: %v", diff.Place))
}

func TestEvictAndPlace_LimitGreaterThanAllocs(t *testing.T) {
	ci.Parallel(t)

	_, ctx := feasible.MockContext(t)
	allocs := []reconciler.AllocTuple{
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
		{Alloc: &structs.Allocation{ID: uuid.Generate()}},
	}
	diff := &reconciler.NodeReconcileResult{}

	limit := 6
	must.False(t, evictAndPlace(ctx, diff, allocs, "", &limit))
	must.Eq(t, 2, limit, must.Sprint("evictAndReplace() should decrement limit"))
	must.Len(t, 4, diff.Place, must.Sprintf("evictAndReplace() didn't insert into diffResult properly: %v", diff.Place))
}
