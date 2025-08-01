// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package reconciler

import (
	"testing"
	"time"

	"github.com/hashicorp/nomad/ci"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/mock"
	"github.com/hashicorp/nomad/nomad/structs"
	"github.com/shoenig/test/must"
)

func testJob_Disconnected() *structs.Job {
	testJob := mock.Job()
	testJob.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
		LostAfter: 5 * time.Second,
		Replace:   pointer.Of(true),
	}

	return testJob
}

func testJobSingle_Disconnected() *structs.Job {
	testJobSingle := mock.Job()
	testJobSingle.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
		LostAfter: 5 * time.Second,
		Replace:   pointer.Of(true),
	}
	return testJobSingle
}

func testJobNoMaxDisconnect_Disconnected() *structs.Job {
	testJobNoMaxDisconnect := mock.Job()
	testJobNoMaxDisconnect.TaskGroups[0].Disconnect = nil

	return testJobNoMaxDisconnect
}

func testJobNoMaxDisconnectSingle_Disconnected() *structs.Job {
	testJobNoMaxDisconnectSingle := mock.Job()
	testJobNoMaxDisconnectSingle.TaskGroups[0].Disconnect = &structs.DisconnectStrategy{
		LostAfter: 0 * time.Second,
		Replace:   pointer.Of(false),
	}

	return testJobNoMaxDisconnectSingle
}

func TestAllocSet_filterByTainted(t *testing.T) {
	ci.Parallel(t)

	now := time.Now()
	nodes := map[string]*structs.Node{
		"draining": {
			ID:            "draining",
			DrainStrategy: mock.DrainNode().DrainStrategy,
		},
		"lost": {
			ID:     "lost",
			Status: structs.NodeStatusDown,
		},
		"nil": nil,
		"normal": {
			ID:     "normal",
			Status: structs.NodeStatusReady,
		},
		"disconnected": {
			ID:     "disconnected",
			Status: structs.NodeStatusDisconnected,
		},
	}

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
			Time:  now.Add(-time.Second),
		},
		{
			Field: structs.AllocStateFieldClientStatus,
			Value: structs.AllocClientStatusRunning,
			Time:  now,
		},
	}

	jobDefinitions := []struct {
		name                         string
		testJob                      func() *structs.Job
		testJobSingle                func() *structs.Job
		testJobNoMaxDisconnect       func() *structs.Job
		testJobNoMaxDisconnectSingle func() *structs.Job
	}{
		{
			name:                         "new_definitions_using_disconnect_block",
			testJob:                      testJob_Disconnected,
			testJobSingle:                testJobSingle_Disconnected,
			testJobNoMaxDisconnect:       testJobNoMaxDisconnect_Disconnected,
			testJobNoMaxDisconnectSingle: testJobNoMaxDisconnectSingle_Disconnected,
		},
	}

	for _, jd := range jobDefinitions {
		testJob := jd.testJob()
		testJobSingle := jd.testJobSingle()
		testJobNoMaxDisconnect := jd.testJobNoMaxDisconnect()
		testJobNoMaxDisconnectSingle := jd.testJobNoMaxDisconnectSingle()

		t.Run(jd.name, func(t *testing.T) {
			testCases := []struct {
				name            string
				all             allocSet
				state           ClusterState
				skipNilNodeTest bool
				// expected results
				untainted     allocSet
				migrate       allocSet
				lost          allocSet
				disconnecting allocSet
				reconnecting  allocSet
				ignore        allocSet
				expiring      allocSet
			}{ // These two cases test that we maintain parity with pre-disconnected-clients behavior.
				{
					name:            "lost-client",
					state:           ClusterState{nodes, false, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"untainted1": {
							ID:           "untainted1",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJob,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted
						"untainted2": {
							ID:           "untainted2",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted, even on draining nodes
						"untainted3": {
							ID:           "untainted3",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "draining",
						},
						// Terminal allocs are always untainted, even on lost nodes
						"untainted4": {
							ID:           "untainted4",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "lost",
						},
						// Non-terminal alloc with migrate=true should migrate on a draining node
						"migrating1": {
							ID:                "migrating1",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJob,
							NodeID:            "draining",
						},
						// Non-terminal alloc with migrate=true should migrate on an unknown node
						"migrating2": {
							ID:                "migrating2",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJob,
							NodeID:            "nil",
						},
					},
					untainted: allocSet{
						"untainted1": {
							ID:           "untainted1",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJob,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted
						"untainted2": {
							ID:           "untainted2",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted, even on draining nodes
						"untainted3": {
							ID:           "untainted3",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "draining",
						},
						// Terminal allocs are always untainted, even on lost nodes
						"untainted4": {
							ID:           "untainted4",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJob,
							NodeID:       "lost",
						},
					},
					migrate: allocSet{
						// Non-terminal alloc with migrate=true should migrate on a draining node
						"migrating1": {
							ID:                "migrating1",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJob,
							NodeID:            "draining",
						},
						// Non-terminal alloc with migrate=true should migrate on an unknown node
						"migrating2": {
							ID:                "migrating2",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJob,
							NodeID:            "nil",
						},
					},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring:      allocSet{},
				},
				{
					name:  "lost-client-only-tainted-nodes",
					state: ClusterState{nodes, false, time.Now()},
					// The logic associated with this test case can only trigger if there
					// is a tainted node. Therefore, testing with a nil node set produces
					// false failures, so don't perform that test if in this case.
					skipNilNodeTest: true,
					all: allocSet{
						// Non-terminal allocs on lost nodes are lost
						"lost1": {
							ID:           "lost1",
							ClientStatus: structs.AllocClientStatusPending,
							Job:          testJob,
							NodeID:       "lost",
						},
						// Non-terminal allocs on lost nodes are lost
						"lost2": {
							ID:           "lost2",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJob,
							NodeID:       "lost",
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost: allocSet{
						// Non-terminal allocs on lost nodes are lost
						"lost1": {
							ID:           "lost1",
							ClientStatus: structs.AllocClientStatusPending,
							Job:          testJob,
							NodeID:       "lost",
						},
						// Non-terminal allocs on lost nodes are lost
						"lost2": {
							ID:           "lost2",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJob,
							NodeID:       "lost",
						},
					},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-disconnect-unset-max-disconnect",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: true,
					all: allocSet{
						// Non-terminal allocs on disconnected nodes w/o max-disconnect are lost
						"lost-running": {
							ID:            "lost-running",
							Name:          "lost-running",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnect,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost: allocSet{
						"lost-running": {
							ID:            "lost-running",
							Name:          "lost-running",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnect,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					expiring: allocSet{},
				},
				// Everything below this line tests the disconnected client mode.
				{
					name:            "disco-client-untainted-reconnect-failed-and-replaced",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "failed-original",
						},
						// Failed and replaced allocs on reconnected nodes
						// that are still desired-running are reconnected so
						// we can stop them
						"failed-original": {
							ID:            "failed-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "failed-original",
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"failed-original": {
							ID:            "failed-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore:   allocSet{},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-reconnecting-running-no-replacement",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						// Running allocs on reconnected nodes with no replacement are reconnecting.
						// Node.UpdateStatus has already handled syncing client state so this
						// should be a noop.
						"reconnecting-running-no-replacement": {
							ID:            "reconnecting-running-no-replacement",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"reconnecting-running-no-replacement": {
							ID:            "reconnecting-running-no-replacement",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore:   allocSet{},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-terminal",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						// Allocs on reconnected nodes that are complete need to be updated to stop
						"untainted-reconnect-complete": {
							ID:            "untainted-reconnect-complete",
							Name:          "untainted-reconnect-complete",
							ClientStatus:  structs.AllocClientStatusComplete,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						// Failed allocs on reconnected nodes are in reconnecting so that
						// they be marked with desired status stop at the server.
						"reconnecting-failed": {
							ID:            "reconnecting-failed",
							Name:          "reconnecting-failed",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						// Lost allocs on reconnected nodes don't get restarted
						"ignored-reconnect-lost": {
							ID:            "ignored-reconnect-lost",
							Name:          "ignored-reconnect-lost",
							ClientStatus:  structs.AllocClientStatusLost,
							DesiredStatus: structs.AllocDesiredStatusStop,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						// Replacement allocs that are complete need to be updated
						"untainted-reconnect-complete-replacement": {
							ID:                 "untainted-reconnect-complete-replacement",
							Name:               "untainted-reconnect-complete",
							ClientStatus:       structs.AllocClientStatusComplete,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							AllocStates:        unknownAllocState,
							PreviousAllocation: "untainted-reconnect-complete",
						},
						// Replacement allocs on reconnected nodes that are failed are ignored
						"ignored-reconnect-failed-replacement": {
							ID:                 "ignored-reconnect-failed-replacement",
							Name:               "ignored-reconnect-failed",
							ClientStatus:       structs.AllocClientStatusFailed,
							DesiredStatus:      structs.AllocDesiredStatusStop,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "reconnecting-failed",
						},
						// Lost replacement allocs on reconnected nodes don't get restarted
						"ignored-reconnect-lost-replacement": {
							ID:                 "ignored-reconnect-lost-replacement",
							Name:               "ignored-reconnect-lost",
							ClientStatus:       structs.AllocClientStatusLost,
							DesiredStatus:      structs.AllocDesiredStatusStop,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							AllocStates:        unknownAllocState,
							PreviousAllocation: "untainted-reconnect-lost",
						},
					},
					untainted: allocSet{
						"untainted-reconnect-complete": {
							ID:            "untainted-reconnect-complete",
							Name:          "untainted-reconnect-complete",
							ClientStatus:  structs.AllocClientStatusComplete,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						"untainted-reconnect-complete-replacement": {
							ID:                 "untainted-reconnect-complete-replacement",
							Name:               "untainted-reconnect-complete",
							ClientStatus:       structs.AllocClientStatusComplete,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							AllocStates:        unknownAllocState,
							PreviousAllocation: "untainted-reconnect-complete",
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"reconnecting-failed": {
							ID:            "reconnecting-failed",
							Name:          "reconnecting-failed",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore: allocSet{
						"ignored-reconnect-lost": {
							ID:            "ignored-reconnect-lost",
							Name:          "ignored-reconnect-lost",
							ClientStatus:  structs.AllocClientStatusLost,
							DesiredStatus: structs.AllocDesiredStatusStop,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						"ignored-reconnect-failed-replacement": {
							ID:                 "ignored-reconnect-failed-replacement",
							Name:               "ignored-reconnect-failed",
							ClientStatus:       structs.AllocClientStatusFailed,
							DesiredStatus:      structs.AllocDesiredStatusStop,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "reconnecting-failed",
						},
						"ignored-reconnect-lost-replacement": {
							ID:                 "ignored-reconnect-lost-replacement",
							Name:               "ignored-reconnect-lost",
							ClientStatus:       structs.AllocClientStatusLost,
							DesiredStatus:      structs.AllocDesiredStatusStop,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							AllocStates:        unknownAllocState,
							PreviousAllocation: "untainted-reconnect-lost",
						},
					},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-disconnect",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: true,
					all: allocSet{
						// Non-terminal allocs on disconnected nodes are disconnecting
						"disconnect-running": {
							ID:            "disconnect-running",
							Name:          "disconnect-running",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
						// Unknown allocs on disconnected nodes are acknowledge, so they wont be rescheduled again
						"untainted-unknown": {
							ID:            "untainted-unknown",
							Name:          "untainted-unknown",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						// Unknown allocs on disconnected nodes are lost when expired
						"expiring-unknown": {
							ID:            "expiring-unknown",
							Name:          "expiring-unknown",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
						// Pending allocs on disconnected nodes are lost
						"lost-pending": {
							ID:            "lost-pending",
							Name:          "lost-pending",
							ClientStatus:  structs.AllocClientStatusPending,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
						// Expired allocs on reconnected clients are lost
						"expiring-expired": {
							ID:            "expiring-expired",
							Name:          "expiring-expired",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
						// Failed and stopped allocs on disconnected nodes are ignored
						"ignore-reconnected-failed-stopped": {
							ID:            "ignore-reconnected-failed-stopped",
							Name:          "ignore-reconnected-failed-stopped",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusStop,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						// Unknown allocs on disconnected nodes are acknowledge, so they wont be rescheduled again
						"untainted-unknown": {
							ID:            "untainted-unknown",
							Name:          "untainted-unknown",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					migrate: allocSet{},
					disconnecting: allocSet{
						"disconnect-running": {
							ID:            "disconnect-running",
							Name:          "disconnect-running",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					reconnecting: allocSet{},
					ignore: allocSet{
						"ignore-reconnected-failed-stopped": {
							ID:            "ignore-reconnected-failed-stopped",
							Name:          "ignore-reconnected-failed-stopped",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusStop,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					lost: allocSet{
						"lost-pending": {
							ID:            "lost-pending",
							Name:          "lost-pending",
							ClientStatus:  structs.AllocClientStatusPending,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					expiring: allocSet{
						"expiring-unknown": {
							ID:            "expiring-unknown",
							Name:          "expiring-unknown",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
						"expiring-expired": {
							ID:            "expiring-expired",
							Name:          "expiring-expired",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
					},
				},
				{
					name:            "disco-client-reconnect",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						// Expired allocs on reconnected clients are lost
						"expired-reconnect": {
							ID:            "expired-reconnect",
							Name:          "expired-reconnect",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring: allocSet{
						"expired-reconnect": {
							ID:            "expired-reconnect",
							Name:          "expired-reconnect",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
					},
				},
				{
					name:            "disco-client-running-reconnecting-and-replacement-untainted",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "running-original",
						},
						// Running and replaced allocs on reconnected nodes are reconnecting
						"running-original": {
							ID:            "running-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJob,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "running-original",
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"running-original": {
							ID:            "running-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore:   allocSet{},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					// After an alloc is reconnected, it should be considered
					// "untainted" instead of "reconnecting" to allow changes such as
					// job updates to be applied properly.
					name:            "disco-client-reconnected-alloc-untainted",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-reconnected": {
							ID:            "running-reconnected",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   reconnectedAllocState,
						},
					},
					untainted: allocSet{
						"running-reconnected": {
							ID:            "running-reconnected",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJob,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   reconnectedAllocState,
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring:      allocSet{},
				},
				// Everything below this line tests the single instance on lost mode.
				{
					name:            "lost-client-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"untainted1": {
							ID:           "untainted1",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJobSingle,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted
						"untainted2": {
							ID:           "untainted2",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted, even on draining nodes
						"untainted3": {
							ID:           "untainted3",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "draining",
						},
						// Terminal allocs are always untainted, even on lost nodes
						"untainted4": {
							ID:           "untainted4",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
						// Non-terminal alloc with migrate=true should migrate on a draining node
						"migrating1": {
							ID:                "migrating1",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJobSingle,
							NodeID:            "draining",
						},
						// Non-terminal alloc with migrate=true should migrate on an unknown node
						"migrating2": {
							ID:                "migrating2",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJobSingle,
							NodeID:            "nil",
						},
					},
					untainted: allocSet{
						"untainted1": {
							ID:           "untainted1",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJobSingle,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted
						"untainted2": {
							ID:           "untainted2",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "normal",
						},
						// Terminal allocs are always untainted, even on draining nodes
						"untainted3": {
							ID:           "untainted3",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "draining",
						},
						// Terminal allocs are always untainted, even on lost nodes
						"untainted4": {
							ID:           "untainted4",
							ClientStatus: structs.AllocClientStatusComplete,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
					},
					migrate: allocSet{
						// Non-terminal alloc with migrate=true should migrate on a draining node
						"migrating1": {
							ID:                "migrating1",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJobSingle,
							NodeID:            "draining",
						},
						// Non-terminal alloc with migrate=true should migrate on an unknown node
						"migrating2": {
							ID:                "migrating2",
							ClientStatus:      structs.AllocClientStatusRunning,
							DesiredTransition: structs.DesiredTransition{Migrate: pointer.Of(true)},
							Job:               testJobSingle,
							NodeID:            "nil",
						},
					},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring:      allocSet{},
				},
				{
					name:  "lost-client-only-tainted-nodes-single-instance-on",
					state: ClusterState{nodes, false, time.Now()},
					// The logic associated with this test case can only trigger if there
					// is a tainted node. Therefore, testing with a nil node set produces
					// false failures, so don't perform that test if in this case.
					skipNilNodeTest: true,
					all: allocSet{
						// Non-terminal allocs on lost nodes are lost
						"lost1": {
							ID:           "lost1",
							ClientStatus: structs.AllocClientStatusPending,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
						// Non-terminal allocs on lost nodes are lost
						"lost2": {
							ID:           "lost2",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost: allocSet{
						// Non-terminal allocs on lost nodes are lost
						"lost1": {
							ID:           "lost1",
							ClientStatus: structs.AllocClientStatusPending,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
						// Non-terminal allocs on lost nodes are lost
						"lost2": {
							ID:           "lost2",
							ClientStatus: structs.AllocClientStatusRunning,
							Job:          testJobSingle,
							NodeID:       "lost",
						},
					},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-disconnect-unset-max-disconnect-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: true,
					all: allocSet{
						// Non-terminal allocs on disconnected nodes w/o max-disconnect are lost
						"disconnecting-running": {
							ID:            "disconnecting-running",
							Name:          "disconnecting-running",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					untainted: allocSet{},
					migrate:   allocSet{},
					disconnecting: allocSet{"disconnecting-running": {
						ID:            "disconnecting-running",
						Name:          "disconnecting-running",
						ClientStatus:  structs.AllocClientStatusRunning,
						DesiredStatus: structs.AllocDesiredStatusRun,
						Job:           testJobNoMaxDisconnectSingle,
						NodeID:        "disconnected",
						TaskGroup:     "web",
					}},
					reconnecting: allocSet{},
					ignore:       allocSet{},
					lost:         allocSet{},
					expiring:     allocSet{},
				},
				{
					name:            "disco-client-untainted-reconnect-failed-and-replaced-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJobSingle,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "failed-original",
						},
						// Failed and replaced allocs on reconnected nodes
						// that are still desired-running are reconnected so
						// we can stop them
						"failed-original": {
							ID:            "failed-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJobSingle,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "failed-original",
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"failed-original": {
							ID:            "failed-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusFailed,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore:   allocSet{},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					name:            "disco-client-reconnect-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						// Expired allocs on reconnected clients are lost
						"expired-reconnect": {
							ID:            "expired-reconnect",
							Name:          "expired-reconnect",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
					},
					untainted:     allocSet{},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring: allocSet{
						"expired-reconnect": {
							ID:            "expired-reconnect",
							Name:          "expired-reconnect",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   expiredAllocState,
						},
					},
				},
				{
					name:            "disco-client-running-reconnecting-and-replacement-untainted-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJobSingle,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "running-original",
						},
						// Running and replaced allocs on reconnected nodes are reconnecting
						"running-original": {
							ID:            "running-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						"running-replacement": {
							ID:                 "running-replacement",
							Name:               "web",
							ClientStatus:       structs.AllocClientStatusRunning,
							DesiredStatus:      structs.AllocDesiredStatusRun,
							Job:                testJobSingle,
							NodeID:             "normal",
							TaskGroup:          "web",
							PreviousAllocation: "running-original",
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting: allocSet{
						"running-original": {
							ID:            "running-original",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					ignore:   allocSet{},
					lost:     allocSet{},
					expiring: allocSet{},
				},
				{
					// After an alloc is reconnected, it should be considered
					// "untainted" instead of "reconnecting" to allow changes such as
					// job updates to be applied properly.
					name:            "disco-client-reconnected-alloc-untainted",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: false,
					all: allocSet{
						"running-reconnected": {
							ID:            "running-reconnected",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   reconnectedAllocState,
						},
					},
					untainted: allocSet{
						"running-reconnected": {
							ID:            "running-reconnected",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobSingle,
							NodeID:        "normal",
							TaskGroup:     "web",
							AllocStates:   reconnectedAllocState,
						},
					},
					migrate:       allocSet{},
					disconnecting: allocSet{},
					reconnecting:  allocSet{},
					ignore:        allocSet{},
					lost:          allocSet{},
					expiring:      allocSet{},
				},
				{
					name:            "disco-client-reconnected-alloc-untainted-single-instance-on",
					state:           ClusterState{nodes, true, time.Now()},
					skipNilNodeTest: true,
					all: allocSet{
						"untainted-unknown": {
							ID:            "untainted-unknown",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						"disconnecting-running": {
							ID:            "disconnecting-running",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
						"lost-running": {
							ID:            "lost-running",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnect,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
						"untainted-unknown-on-down-node": {
							ID:            "untainted-unknown-on-down-node",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "down",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					untainted: allocSet{
						"untainted-unknown": {
							ID:            "untainted-unknown",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "disconnected",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
						"untainted-unknown-on-down-node": {
							ID:            "untainted-unknown-on-down-node",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusUnknown,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "down",
							TaskGroup:     "web",
							AllocStates:   unknownAllocState,
						},
					},
					migrate: allocSet{},
					disconnecting: allocSet{
						"disconnecting-running": {
							ID:            "disconnecting-running",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnectSingle,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					reconnecting: allocSet{},
					ignore:       allocSet{},
					lost: allocSet{
						"lost-running": {
							ID:            "lost-running",
							Name:          "web",
							ClientStatus:  structs.AllocClientStatusRunning,
							DesiredStatus: structs.AllocDesiredStatusRun,
							Job:           testJobNoMaxDisconnect,
							NodeID:        "disconnected",
							TaskGroup:     "web",
						},
					},
					expiring: allocSet{},
				},
			}

			for _, tc := range testCases {
				t.Run(tc.name, func(t *testing.T) {
					// With tainted nodes
					untainted, migrate, lost, disconnecting, reconnecting, ignore, expired := tc.all.filterByTainted(tc.state)
					must.Eq(t, tc.untainted, untainted, must.Sprintf("with-nodes: untainted"))
					must.Eq(t, tc.migrate, migrate, must.Sprintf("with-nodes: migrate"))
					must.Eq(t, tc.lost, lost, must.Sprintf("with-nodes: lost"))
					must.Eq(t, tc.disconnecting, disconnecting, must.Sprintf("with-nodes: disconnecting"))
					must.Eq(t, tc.reconnecting, reconnecting, must.Sprintf("with-nodes: reconnecting"))
					must.Eq(t, tc.ignore, ignore, must.Sprintf("with-nodes: ignore"))
					must.Eq(t, tc.expiring, expired, must.Sprintf("with-nodes: expiring"))

					if tc.skipNilNodeTest {
						return
					}

					// Now again with nodes nil
					state := tc.state
					state.TaintedNodes = nil
					untainted, migrate, lost, disconnecting, reconnecting, ignore, expired = tc.all.filterByTainted(state)
					must.Eq(t, tc.untainted, untainted, must.Sprintf("with-nodes: untainted"))
					must.Eq(t, tc.migrate, migrate, must.Sprintf("with-nodes: migrate"))
					must.Eq(t, tc.lost, lost, must.Sprintf("with-nodes: lost"))
					must.Eq(t, tc.disconnecting, disconnecting, must.Sprintf("with-nodes: disconnecting"))
					must.Eq(t, tc.reconnecting, reconnecting, must.Sprintf("with-nodes: reconnecting"))
					must.Eq(t, tc.ignore, ignore, must.Sprintf("with-nodes: ignore"))
					must.Eq(t, tc.ignore, ignore, must.Sprintf("with-nodes: expiring"))
					must.Eq(t, tc.expiring, expired, must.Sprintf("with-nodes: expiring"))
				})
			}
		})
	}
}

func TestReconcile_shouldFilter(t *testing.T) {
	testCases := []struct {
		description   string
		batch         bool
		failed        bool
		desiredStatus string
		clientStatus  string
		rt            *structs.RescheduleTracker

		untainted bool
		ignore    bool
	}{
		{
			description:   "batch running",
			batch:         true,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusRun,
			clientStatus:  structs.AllocClientStatusRunning,
			untainted:     true,
			ignore:        false,
		},
		{
			description:   "batch stopped success",
			batch:         true,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusRunning,
			untainted:     true,
			ignore:        false,
		},
		{
			description:   "batch stopped failed",
			batch:         true,
			failed:        true,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "batch evicted",
			batch:         true,
			desiredStatus: structs.AllocDesiredStatusEvict,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "batch failed",
			batch:         true,
			desiredStatus: structs.AllocDesiredStatusRun,
			clientStatus:  structs.AllocClientStatusFailed,
			untainted:     false,
			ignore:        false,
		},
		{
			description:   "batch lost",
			batch:         true,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusLost,
			untainted:     true,
			ignore:        false,
		},
		{
			description:   "batch last reschedule failed",
			batch:         false,
			failed:        true,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusFailed,
			untainted:     false,
			ignore:        false,
			rt: &structs.RescheduleTracker{
				Events:         []*structs.RescheduleEvent{},
				LastReschedule: structs.LastRescheduleFailedToPlace,
			},
		},
		{
			description:   "service running",
			batch:         false,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusRun,
			clientStatus:  structs.AllocClientStatusRunning,
			untainted:     false,
			ignore:        false,
		},
		{
			description:   "service stopped",
			batch:         false,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "service evicted",
			batch:         false,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusEvict,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "service client complete",
			batch:         false,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusRun,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "service client complete",
			batch:         false,
			failed:        false,
			desiredStatus: structs.AllocDesiredStatusRun,
			clientStatus:  structs.AllocClientStatusComplete,
			untainted:     false,
			ignore:        true,
		},
		{
			description:   "service client reschedule failed",
			batch:         false,
			failed:        true,
			desiredStatus: structs.AllocDesiredStatusStop,
			clientStatus:  structs.AllocClientStatusFailed,
			untainted:     false,
			ignore:        false,
			rt: &structs.RescheduleTracker{
				Events:         []*structs.RescheduleEvent{},
				LastReschedule: structs.LastRescheduleFailedToPlace,
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.description, func(t *testing.T) {
			alloc := &structs.Allocation{
				DesiredStatus:     tc.desiredStatus,
				TaskStates:        map[string]*structs.TaskState{"task": {State: structs.TaskStateDead, Failed: tc.failed}},
				ClientStatus:      tc.clientStatus,
				RescheduleTracker: tc.rt,
			}

			untainted, ignore := shouldFilter(alloc, tc.batch)
			must.Eq(t, tc.untainted, untainted)
			must.Eq(t, tc.ignore, ignore)
		})
	}
}

// Test that we properly create the bitmap even when the alloc set includes an
// allocation with a higher count than the current min count and it is byte
// aligned.
// Ensure no regression from: https://github.com/hashicorp/nomad/issues/3008
func TestBitmapFrom(t *testing.T) {
	ci.Parallel(t)

	input := allocSet{
		"8": {
			JobID:     "foo",
			TaskGroup: "bar",
			Name:      "foo.bar[8]",
		},
	}
	b, dups := bitmapFrom(input, 1)
	must.Eq(t, 16, b.Size())
	must.MapEmpty(t, dups)

	b, dups = bitmapFrom(input, 8)
	must.Eq(t, 16, b.Size())
	must.MapEmpty(t, dups)
}

func Test_allocNameIndex_Highest(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		name                string
		inputAllocNameIndex *AllocNameIndex
		inputN              uint
		expectedOutput      map[string]struct{}
	}{
		{
			name: "select 1",
			inputAllocNameIndex: newAllocNameIndex(
				"example", "cache", 3, allocSet{
					"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
						Name:      "example.cache[0]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"e24771e6-8900-5d2d-ec93-e7076284774a": {
						Name:      "example.cache[1]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
						Name:      "example.cache[2]",
						JobID:     "example",
						TaskGroup: "cache",
					},
				}),
			inputN: 1,
			expectedOutput: map[string]struct{}{
				"example.cache[2]": {},
			},
		},
		{
			name: "select all",
			inputAllocNameIndex: newAllocNameIndex(
				"example", "cache", 3, allocSet{
					"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
						Name:      "example.cache[0]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"e24771e6-8900-5d2d-ec93-e7076284774a": {
						Name:      "example.cache[1]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
						Name:      "example.cache[2]",
						JobID:     "example",
						TaskGroup: "cache",
					},
				}),
			inputN: 3,
			expectedOutput: map[string]struct{}{
				"example.cache[2]": {},
				"example.cache[1]": {},
				"example.cache[0]": {},
			},
		},
		{
			name: "select too many",
			inputAllocNameIndex: newAllocNameIndex(
				"example", "cache", 3, allocSet{
					"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
						Name:      "example.cache[0]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"e24771e6-8900-5d2d-ec93-e7076284774a": {
						Name:      "example.cache[1]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
						Name:      "example.cache[2]",
						JobID:     "example",
						TaskGroup: "cache",
					},
				}),
			inputN: 13,
			expectedOutput: map[string]struct{}{
				"example.cache[2]": {},
				"example.cache[1]": {},
				"example.cache[0]": {},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			must.Eq(t, tc.expectedOutput, tc.inputAllocNameIndex.Highest(tc.inputN))
		})
	}
}

func Test_allocNameIndex_NextCanaries(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		name                string
		inputAllocNameIndex *AllocNameIndex
		inputN              uint
		inputExisting       allocSet
		inputDestructive    allocSet
		expectedOutput      []string
	}{
		{
			name: "single canary",
			inputAllocNameIndex: newAllocNameIndex(
				"example", "cache", 3, allocSet{
					"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
						Name:      "example.cache[0]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"e24771e6-8900-5d2d-ec93-e7076284774a": {
						Name:      "example.cache[1]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
						Name:      "example.cache[2]",
						JobID:     "example",
						TaskGroup: "cache",
					},
				}),
			inputN:        1,
			inputExisting: nil,
			inputDestructive: allocSet{
				"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
					Name:      "example.cache[0]",
					JobID:     "example",
					TaskGroup: "cache",
				},
				"e24771e6-8900-5d2d-ec93-e7076284774a": {
					Name:      "example.cache[1]",
					JobID:     "example",
					TaskGroup: "cache",
				},
				"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
					Name:      "example.cache[2]",
					JobID:     "example",
					TaskGroup: "cache",
				},
			},
			expectedOutput: []string{
				"example.cache[0]",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			must.SliceContainsAll(
				t, tc.expectedOutput,
				tc.inputAllocNameIndex.NextCanaries(tc.inputN, tc.inputExisting, tc.inputDestructive))
		})
	}
}

func Test_allocNameIndex_Next(t *testing.T) {
	ci.Parallel(t)

	testCases := []struct {
		name                string
		inputAllocNameIndex *AllocNameIndex
		inputN              uint
		expectedOutput      []string
	}{
		{
			name:                "empty existing bitmap",
			inputAllocNameIndex: newAllocNameIndex("example", "cache", 3, nil),
			inputN:              3,
			expectedOutput: []string{
				"example.cache[0]", "example.cache[1]", "example.cache[2]",
			},
		},
		{
			name: "non-empty existing bitmap simple",
			inputAllocNameIndex: newAllocNameIndex(
				"example", "cache", 3, allocSet{
					"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
						Name:      "example.cache[0]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"e24771e6-8900-5d2d-ec93-e7076284774a": {
						Name:      "example.cache[1]",
						JobID:     "example",
						TaskGroup: "cache",
					},
					"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
						Name:      "example.cache[2]",
						JobID:     "example",
						TaskGroup: "cache",
					},
				}),
			inputN: 3,
			expectedOutput: []string{
				"example.cache[0]", "example.cache[1]", "example.cache[2]",
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			must.SliceContainsAll(t, tc.expectedOutput, tc.inputAllocNameIndex.Next(tc.inputN))
		})
	}
}

func Test_allocNameIndex_Duplicates(t *testing.T) {
	ci.Parallel(t)

	inputAllocSet := allocSet{
		"6b255fa3-c2cb-94de-5ddd-41aac25a6851": {
			Name:      "example.cache[0]",
			JobID:     "example",
			TaskGroup: "cache",
		},
		"e24771e6-8900-5d2d-ec93-e7076284774a": {
			Name:      "example.cache[1]",
			JobID:     "example",
			TaskGroup: "cache",
		},
		"d7842822-32c4-1a1c-bac8-66c3f20dfb0f": {
			Name:      "example.cache[2]",
			JobID:     "example",
			TaskGroup: "cache",
		},
		"76a6a487-016b-2fc2-8295-d811473ca93d": {
			Name:      "example.cache[0]",
			JobID:     "example",
			TaskGroup: "cache",
		},
	}

	// Build the tracker, and check some key information.
	allocNameIndexTracker := newAllocNameIndex("example", "cache", 4, inputAllocSet)
	must.Eq(t, 8, allocNameIndexTracker.b.Size())
	must.MapLen(t, 1, allocNameIndexTracker.duplicates)
	must.True(t, allocNameIndexTracker.IsDuplicate(0))

	// Unsetting the index should remove the duplicate entry, but not the entry
	// from the underlying bitmap.
	allocNameIndexTracker.UnsetIndex(0)
	must.MapLen(t, 0, allocNameIndexTracker.duplicates)
	must.True(t, allocNameIndexTracker.b.Check(0))

	// If we now select a new index, having previously checked for a duplicate,
	// we should get a non-duplicate.
	nextAllocNames := allocNameIndexTracker.Next(1)
	must.Len(t, 1, nextAllocNames)
	must.Eq(t, "example.cache[3]", nextAllocNames[0])
}

func TestAllocSet_filterByRescheduleable(t *testing.T) {
	ci.Parallel(t)

	noRescheduleJob := mock.Job()
	noRescheduleTG := &structs.TaskGroup{
		Name: "noRescheduleTG",
		ReschedulePolicy: &structs.ReschedulePolicy{
			Attempts:  0,
			Unlimited: false,
		},
	}

	noRescheduleJob.TaskGroups[0] = noRescheduleTG

	testJob := mock.Job()
	rescheduleTG := &structs.TaskGroup{
		Name: "rescheduleTG",
		ReschedulePolicy: &structs.ReschedulePolicy{
			Attempts:      2,
			Interval:      time.Hour,
			Delay:         0,
			DelayFunction: "constant",
			MaxDelay:      -1,
			Unlimited:     false,
		},
	}
	testJob.TaskGroups[0] = rescheduleTG

	now := time.Now()

	rt := &structs.RescheduleTracker{
		Events: []*structs.RescheduleEvent{{
			RescheduleTime: now.Add(-24 * time.Hour).UnixNano(),
			PrevAllocID:    uuid.Generate(),
			PrevNodeID:     uuid.Generate(),
			Delay:          0,
		}},
	}

	type testCase struct {
		name                        string
		all                         allocSet
		isBatch                     bool
		supportsDisconnectedClients bool
		isDisconnecting             bool
		deployment                  *structs.Deployment

		// expected results
		untainted allocSet
		resNow    allocSet
		resLater  []*delayedRescheduleInfo
	}

	testCases := []testCase{
		{
			name:            "batch disconnecting allocation no reschedule",
			isDisconnecting: true,
			isBatch:         true,
			all: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			untainted: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			resNow:   allocSet{},
			resLater: []*delayedRescheduleInfo{},
		},
		{
			name:            "batch ignore unknown disconnecting allocs",
			isDisconnecting: true,
			isBatch:         true,
			all: allocSet{
				"disconnecting1": {
					ID:           "disconnection1",
					ClientStatus: structs.AllocClientStatusUnknown,
					Job:          testJob,
				},
			},
			untainted: allocSet{},
			resNow:    allocSet{},
			resLater:  []*delayedRescheduleInfo{},
		},
		{
			name:            "batch disconnecting allocation reschedule",
			isDisconnecting: true,
			isBatch:         true,
			all: allocSet{
				"rescheduleNow1": {
					ID:                "rescheduleNow1",
					ClientStatus:      structs.AllocClientStatusRunning,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
				},
			},
			untainted: allocSet{},
			resNow: allocSet{
				"rescheduleNow1": {
					ID:                "rescheduleNow1",
					ClientStatus:      structs.AllocClientStatusRunning,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
				},
			},
			resLater: []*delayedRescheduleInfo{},
		},

		{
			name:            "batch successfully complete should not reschedule",
			isDisconnecting: false,
			isBatch:         true,
			all: allocSet{
				"batchComplete1": {
					ID:                "batchComplete1",
					ClientStatus:      structs.AllocClientStatusComplete,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
					TaskStates: map[string]*structs.TaskState{
						"task": {State: structs.TaskStateDead, Failed: false}},
				},
			},
			untainted: allocSet{
				"batchComplete1": {
					ID:                "batchComplete1",
					ClientStatus:      structs.AllocClientStatusComplete,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
					TaskStates: map[string]*structs.TaskState{
						"task": {State: structs.TaskStateDead, Failed: false}},
				},
			},
			resNow:   allocSet{},
			resLater: []*delayedRescheduleInfo{},
		},
		{
			name:            "service disconnecting allocation no reschedule",
			isDisconnecting: true,
			isBatch:         false,
			all: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			untainted: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			resNow:   allocSet{},
			resLater: []*delayedRescheduleInfo{},
		},
		{
			name:            "service disconnecting allocation reschedule",
			isDisconnecting: true,
			isBatch:         false,
			all: allocSet{
				"rescheduleNow1": {
					ID:                "rescheduleNow1",
					ClientStatus:      structs.AllocClientStatusRunning,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
				},
			},
			untainted: allocSet{},
			resNow: allocSet{
				"rescheduleNow1": {
					ID:                "rescheduleNow1",
					ClientStatus:      structs.AllocClientStatusRunning,
					Job:               testJob,
					TaskGroup:         "rescheduleTG",
					RescheduleTracker: rt,
				},
			},
			resLater: []*delayedRescheduleInfo{},
		},
		{
			name:            "service ignore unknown disconnecting allocs",
			isDisconnecting: true,
			isBatch:         false,
			all: allocSet{
				"disconnecting1": {
					ID:           "disconnection1",
					ClientStatus: structs.AllocClientStatusUnknown,
					Job:          testJob,
				},
			},
			untainted: allocSet{},
			resNow:    allocSet{},
			resLater:  []*delayedRescheduleInfo{},
		},
		{
			name:            "service previously rescheduled alloc should not reschedule",
			isDisconnecting: false,
			isBatch:         false,
			all: allocSet{
				"failed1": {
					ID:             "failed1",
					ClientStatus:   structs.AllocClientStatusFailed,
					NextAllocation: uuid.Generate(),
					Job:            testJob,
					TaskGroup:      "rescheduleTG",
				},
			},
			untainted: allocSet{},
			resNow:    allocSet{},
			resLater:  []*delayedRescheduleInfo{},
		},
		{
			name:            "service complete should be ignored",
			isDisconnecting: false,
			isBatch:         false,
			all: allocSet{
				"complete1": {
					ID:            "complete1",
					DesiredStatus: structs.AllocDesiredStatusStop,
					ClientStatus:  structs.AllocClientStatusComplete,
					Job:           testJob,
					TaskGroup:     "rescheduleTG",
				},
			},
			untainted: allocSet{},
			resNow:    allocSet{},
			resLater:  []*delayedRescheduleInfo{},
		},
		{
			name:            "service running allocation no reschedule",
			isDisconnecting: false,
			isBatch:         true,
			all: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			untainted: allocSet{
				"untainted1": {
					ID:           "untainted1",
					ClientStatus: structs.AllocClientStatusRunning,
					Job:          noRescheduleJob,
					TaskGroup:    "noRescheduleTG",
				},
			},
			resNow:   allocSet{},
			resLater: []*delayedRescheduleInfo{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			untainted, resNow, resLater := tc.all.filterByRescheduleable(tc.isBatch,
				tc.isDisconnecting, now, "evailID", tc.deployment)
			must.Eq(t, tc.untainted, untainted, must.Sprintf("with-nodes: untainted"))
			must.Eq(t, tc.resNow, resNow, must.Sprintf("with-nodes: reschedule-now"))
			must.Eq(t, tc.resLater, resLater, must.Sprintf("with-nodes: rescheduleLater"))
		})
	}
}
