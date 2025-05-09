// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: BUSL-1.1

package nomad

import (
	"fmt"
	"net/http"
	"time"

	"github.com/hashicorp/go-hclog"
	"github.com/hashicorp/go-memdb"
	metrics "github.com/hashicorp/go-metrics/compat"

	"github.com/hashicorp/nomad/acl"
	"github.com/hashicorp/nomad/helper/pointer"
	"github.com/hashicorp/nomad/helper/uuid"
	"github.com/hashicorp/nomad/nomad/state"
	"github.com/hashicorp/nomad/nomad/state/paginator"
	"github.com/hashicorp/nomad/nomad/structs"
)

// Alloc endpoint is used for manipulating allocations
type Alloc struct {
	srv    *Server
	ctx    *RPCContext
	logger hclog.Logger
}

func NewAllocEndpoint(srv *Server, ctx *RPCContext) *Alloc {
	return &Alloc{srv: srv, ctx: ctx, logger: srv.logger.Named("alloc")}
}

// List is used to list the allocations in the system
func (a *Alloc) List(args *structs.AllocListRequest, reply *structs.AllocListResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.List", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "alloc", "list"}, time.Now())

	namespace := args.RequestNamespace()

	// Check namespace read-job permissions
	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowNsOp(namespace, acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}
	allow := aclObj.AllowNsOpFunc(acl.NamespaceCapabilityReadJob)

	// Setup the blocking query
	sort := state.SortOption(args.Reverse)
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Scan all the allocations
			var err error
			var iter memdb.ResultIterator

			// get list of accessible namespaces
			allowableNamespaces, err := allowedNSes(aclObj, state, allow)
			if err == structs.ErrPermissionDenied {
				// return empty allocation if token is not authorized for any
				// namespace, matching other endpoints
				reply.Allocations = make([]*structs.AllocListStub, 0)
			} else if err != nil {
				return err
			} else {
				var tokenizer paginator.Tokenizer[*structs.Allocation]

				if prefix := args.QueryOptions.Prefix; prefix != "" {
					iter, err = state.AllocsByIDPrefix(ws, namespace, prefix, sort)
					tokenizer = paginator.IDTokenizer[*structs.Allocation](args.NextToken)
				} else if namespace != structs.AllNamespacesSentinel {
					iter, err = state.AllocsByNamespaceOrdered(ws, namespace, sort)
					tokenizer = paginator.CreateIndexAndIDTokenizer[*structs.Allocation](args.NextToken)
				} else {
					iter, err = state.Allocs(ws, sort)
					tokenizer = paginator.CreateIndexAndIDTokenizer[*structs.Allocation](args.NextToken)
				}
				if err != nil {
					return err
				}

				pager, err := paginator.NewPaginator(iter, args.QueryOptions,
					paginator.NamespaceSelectorFunc[*structs.Allocation](allowableNamespaces),
					tokenizer,
					func(a *structs.Allocation) (*structs.AllocListStub, error) {
						return a.Stub(args.Fields), nil
					},
				)
				if err != nil {
					return structs.NewErrRPCCodedf(
						http.StatusBadRequest, "failed to create result paginator: %v", err)
				}

				stubs, nextToken, err := pager.Page()
				if err != nil {
					return structs.NewErrRPCCodedf(
						http.StatusBadRequest, "failed to read result page: %v", err)
				}

				reply.QueryMeta.NextToken = nextToken
				reply.Allocations = stubs
			}

			// Use the last index that affected the allocs table
			index, err := state.Index("allocs")
			if err != nil {
				return err
			}
			reply.Index = index

			// Set the query response
			a.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return a.srv.blockingRPC(&opts)
}

// GetAlloc is used to lookup a particular allocation
func (a *Alloc) GetAlloc(args *structs.AllocSpecificRequest,
	reply *structs.SingleAllocResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.GetAlloc", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricRead, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}
	defer metrics.MeasureSince([]string{"nomad", "alloc", "get_alloc"}, time.Now())

	// Check namespace read-job permissions before performing blocking query.
	allowNsOp := acl.NamespaceValidator(acl.NamespaceCapabilityReadJob)
	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}

	// Setup the blocking query
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Lookup the allocation
			out, err := state.AllocByID(ws, args.AllocID)
			if err != nil {
				return err
			}

			// Setup the output
			if out != nil {
				out = out.Sanitize()
				reply.Alloc = out
				// Re-check namespace in case it differs from request.
				if !aclObj.AllowClientOp() && !allowNsOp(aclObj, out.Namespace) {
					return structs.NewErrUnknownAllocation(args.AllocID)
				}

				reply.Index = out.ModifyIndex
			} else {
				// Use the last index that affected the allocs table
				index, err := state.Index("allocs")
				if err != nil {
					return err
				}
				reply.Index = index
			}

			// Set the query response
			a.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		}}
	return a.srv.blockingRPC(&opts)
}

// GetAllocs is used to lookup a set of allocations
func (a *Alloc) GetAllocs(args *structs.AllocsGetRequest,
	reply *structs.AllocsGetResponse) error {

	aclObj, err := a.srv.AuthenticateClientOnly(a.ctx, args)
	a.srv.MeasureRPCRate("alloc", structs.RateMetricWrite, args)
	if err != nil {
		return structs.ErrPermissionDenied
	}

	if done, err := a.srv.forward("Alloc.GetAllocs", args, args, reply); done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "alloc", "get_allocs"}, time.Now())

	if !aclObj.AllowClientOp() {
		return structs.ErrPermissionDenied
	}

	allocs := make([]*structs.Allocation, len(args.AllocIDs))

	// Setup the blocking query. We wait for at least one of the requested
	// allocations to be above the min query index. This guarantees that the
	// server has received that index.
	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			// Lookup the allocation
			thresholdMet := false
			maxIndex := uint64(0)
			for i, alloc := range args.AllocIDs {
				out, err := state.AllocByID(ws, alloc)
				if err != nil {
					return err
				}
				if out == nil {
					// We don't have the alloc yet
					thresholdMet = false
					break
				}

				// Store the pointer
				allocs[i] = out

				// Check if we have passed the minimum index
				if out.ModifyIndex > args.QueryOptions.MinQueryIndex {
					thresholdMet = true
				}

				if maxIndex < out.ModifyIndex {
					maxIndex = out.ModifyIndex
				}
			}

			// Setup the output
			if thresholdMet {
				reply.Allocs = allocs
				reply.Index = maxIndex
			} else {
				// Use the last index that affected the allocs table
				index, err := state.Index("allocs")
				if err != nil {
					return err
				}
				reply.Index = index
			}

			// Set the query response
			a.srv.setQueryMeta(&reply.QueryMeta)
			return nil
		},
	}
	return a.srv.blockingRPC(&opts)
}

// Stop is used to stop an allocation and migrate it to another node.
func (a *Alloc) Stop(args *structs.AllocStopRequest, reply *structs.AllocStopResponse) error {

	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.Stop", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "alloc", "stop"}, time.Now())

	alloc, err := getAlloc(a.srv.State(), args.AllocID)
	if err != nil {
		return err
	}

	// Check for namespace alloc-lifecycle permissions.
	allowNsOp := acl.NamespaceValidator(acl.NamespaceCapabilityAllocLifecycle)
	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	} else if !allowNsOp(aclObj, alloc.Namespace) {
		return structs.ErrPermissionDenied
	}

	now := time.Now().UTC().UnixNano()
	eval := &structs.Evaluation{
		ID:             uuid.Generate(),
		Namespace:      alloc.Namespace,
		Priority:       alloc.Job.Priority,
		Type:           alloc.Job.Type,
		TriggeredBy:    structs.EvalTriggerAllocStop,
		JobID:          alloc.Job.ID,
		JobModifyIndex: alloc.Job.ModifyIndex,
		Status:         structs.EvalStatusPending,
		CreateTime:     now,
		ModifyTime:     now,
	}

	transitionReq := &structs.AllocUpdateDesiredTransitionRequest{
		Evals: []*structs.Evaluation{eval},
		Allocs: map[string]*structs.DesiredTransition{
			args.AllocID: {
				Migrate:         pointer.Of(true),
				NoShutdownDelay: pointer.Of(args.NoShutdownDelay),
			},
		},
	}

	// Commit this update via Raft
	_, index, err := a.srv.raftApply(structs.AllocUpdateDesiredTransitionRequestType, transitionReq)
	if err != nil {
		a.logger.Error("AllocUpdateDesiredTransitionRequest failed", "error", err)
		return err
	}

	// Setup the response
	reply.Index = index
	reply.EvalID = eval.ID
	return nil
}

// UpdateDesiredTransition is used to update the desired transitions of an
// allocation.
func (a *Alloc) UpdateDesiredTransition(args *structs.AllocUpdateDesiredTransitionRequest, reply *structs.GenericResponse) error {
	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward("Alloc.UpdateDesiredTransition", args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricWrite, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "alloc", "update_desired_transition"}, time.Now())

	// Check that it is a management token.
	if aclObj, err := a.srv.ResolveACL(args); err != nil {
		return err
	} else if !aclObj.IsManagement() {
		return structs.ErrPermissionDenied
	}

	// Ensure at least a single alloc
	if len(args.Allocs) == 0 {
		return fmt.Errorf("must update at least one allocation")
	}

	// Commit this update via Raft
	_, index, err := a.srv.raftApply(structs.AllocUpdateDesiredTransitionRequestType, args)
	if err != nil {
		a.logger.Error("AllocUpdateDesiredTransitionRequest failed", "error", err)
		return err
	}

	// Setup the response
	reply.Index = index
	return nil
}

// GetServiceRegistrations returns a list of service registrations which belong
// to the passed allocation ID.
func (a *Alloc) GetServiceRegistrations(
	args *structs.AllocServiceRegistrationsRequest,
	reply *structs.AllocServiceRegistrationsResponse) error {

	authErr := a.srv.Authenticate(a.ctx, args)
	if done, err := a.srv.forward(structs.AllocServiceRegistrationsRPCMethod, args, args, reply); done {
		return err
	}
	a.srv.MeasureRPCRate("alloc", structs.RateMetricList, args)
	if authErr != nil {
		return structs.ErrPermissionDenied
	}

	defer metrics.MeasureSince([]string{"nomad", "alloc", "get_service_registrations"}, time.Now())

	// Ensure the caller has the read-job namespace capability.
	aclObj, err := a.srv.ResolveACL(args)
	if err != nil {
		return err
	}
	if !aclObj.AllowNsOp(args.RequestNamespace(), acl.NamespaceCapabilityReadJob) {
		return structs.ErrPermissionDenied
	}

	// Set up the blocking query.
	return a.srv.blockingRPC(&blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, stateStore *state.StateStore) error {

			// Read the allocation to ensure its namespace matches the request
			// args.
			alloc, err := stateStore.AllocByID(ws, args.AllocID)
			if err != nil {
				return err
			}

			// Guard against the alloc not-existing or that the namespace does
			// not match the request arguments.
			if alloc == nil || alloc.Namespace != args.RequestNamespace() {
				return nil
			}

			// Perform the state query to get an iterator.
			iter, err := stateStore.GetServiceRegistrationsByAllocID(ws, args.AllocID)
			if err != nil {
				return err
			}

			// Set up our output after we have checked the error.
			services := make([]*structs.ServiceRegistration, 0)

			// Iterate the iterator, appending all service registrations
			// returned to the reply.
			for raw := iter.Next(); raw != nil; raw = iter.Next() {
				services = append(services, raw.(*structs.ServiceRegistration))
			}
			reply.Services = services

			// Use the index table to populate the query meta as we have no way
			// of tracking the max index on deletes.
			return a.srv.setReplyQueryMeta(stateStore, state.TableServiceRegistrations, &reply.QueryMeta)
		},
	})
}

// SignIdentities allows nodes to retrieve workload identities for their
// allocations.
//
// This is an internal-only RPC and not exposed via the HTTP API.
func (a *Alloc) SignIdentities(args *structs.AllocIdentitiesRequest, reply *structs.AllocIdentitiesResponse) error {

	aclObj, err := a.srv.AuthenticateClientOnly(a.ctx, args)
	a.srv.MeasureRPCRate("alloc", structs.RateMetricRead, args)
	if err != nil {
		return structs.ErrPermissionDenied
	}

	if done, err := a.srv.forward("Alloc.SignIdentities", args, args, reply); done {
		return err
	}
	defer metrics.MeasureSince([]string{"nomad", "alloc", "sign_identities"}, time.Now())

	if !aclObj.AllowClientOp() {
		return structs.ErrPermissionDenied
	}

	if len(args.Identities) == 0 {
		// Client bug. Fail loudly instead of letting clients waste time with
		// noops.
		return fmt.Errorf("no identities requested")
	}

	// Tracks whether the min index was satisfied by the blocking query
	thresholdMet := false

	// Most if not all identity requests will be for the same alloc, so create a
	// set of alloc IDs to avoid unnecessary looping in the blocking query.
	allocs := make(map[string]*structs.Allocation, len(args.Identities))
	for _, idReq := range args.Identities {
		allocs[idReq.AllocID] = nil // to be set while watching
	}

	opts := blockingOptions{
		queryOpts: &args.QueryOptions,
		queryMeta: &reply.QueryMeta,
		run: func(ws memdb.WatchSet, state *state.StateStore) error {
			var maxIndex uint64

			// Lookup the allocations
			for allocID := range allocs {
				out, err := state.AllocByID(ws, allocID)
				if err != nil {
					return err
				}

				if out == nil {
					// Alloc may have been GC'd and therefore should not be able to get
					// identities signed.
					continue
				}

				// Keep the alloc around so we can skip the statestore lookup later
				allocs[allocID] = out

				// If we have found a requested alloc created after the min query index
				// that means we're observing a new enough version of state to satisfy
				// the query.
				if out.CreateIndex > args.QueryOptions.MinQueryIndex {
					thresholdMet = true
				}

				if maxIndex < out.CreateIndex {
					maxIndex = out.CreateIndex
				}
			}

			// If we could not find an alloc created after the min index, note when
			// the index allocs were last updated.
			if !thresholdMet {
				index, err := state.Index("allocs")
				if err != nil {
					return err
				}
				maxIndex = index
			}

			reply.Index = maxIndex
			return nil
		}}

	if err := a.srv.blockingRPC(&opts); err != nil {
		return err
	}

	// Index threshold was not met in the blocking query. Set rejections since
	// allocs could not be found and should be considered invalid.
	if !thresholdMet {
		for _, idReq := range args.Identities {
			reply.Rejections = append(reply.Rejections, &structs.WorkloadIdentityRejection{
				WorkloadIdentityRequest: *idReq,
				Reason:                  structs.WIRejectionReasonMissingAlloc,
			})
		}

		// Return early so the rest of the func acts as the else (thresholdMet)
		return nil
	}

	// Threshold met, so create the response
	now := time.Now().UTC()
	for _, idReq := range args.Identities {
		out := allocs[idReq.AllocID]

		if out == nil {
			// Alloc may have been GC'd and therefore should not be able to get
			// new identities.
			reply.Rejections = append(reply.Rejections, &structs.WorkloadIdentityRejection{
				WorkloadIdentityRequest: *idReq,
				Reason:                  structs.WIRejectionReasonMissingAlloc,
			})
			continue
		}

		job := out.Job

		switch idReq.WorkloadType {
		case structs.WorkloadTypeTask:
			task := out.LookupTask(idReq.WorkloadIdentifier)
			if task == nil {
				// Job has likely been updated to remove this task
				reply.Rejections = append(reply.Rejections, &structs.WorkloadIdentityRejection{
					WorkloadIdentityRequest: *idReq,
					Reason:                  structs.WIRejectionReasonMissingTask,
				})
				continue
			}

			widFound, err := a.signTasks(task, out, idReq, reply, now)
			if err != nil {
				return err
			}

			if !widFound {
				reply.Rejections = append(reply.Rejections, &structs.WorkloadIdentityRejection{
					WorkloadIdentityRequest: *idReq,
					Reason:                  structs.WIRejectionReasonMissingIdentity,
				})
				continue
			}

		case structs.WorkloadTypeService:
			widFound, err := a.signServices(job, out, idReq, reply, now)
			if err != nil {
				return err
			}

			if !widFound {
				reply.Rejections = append(reply.Rejections, &structs.WorkloadIdentityRejection{
					WorkloadIdentityRequest: *idReq,
					Reason:                  structs.WIRejectionReasonMissingIdentity,
				})
				continue
			}
		}
	}
	return nil
}

func (a *Alloc) signTasks(
	task *structs.Task,
	alloc *structs.Allocation,
	idReq *structs.WorkloadIdentityRequest,
	reply *structs.AllocIdentitiesResponse,
	now time.Time,
) (widFound bool, err error) {
	for _, wid := range task.Identities {
		if wid.Name != idReq.IdentityName {
			continue
		}

		widFound = true
		builder := structs.NewIdentityClaimsBuilder(alloc.Job, alloc, &idReq.WIHandle, wid).
			WithTask(task).
			WithConsul()

		var node *structs.Node
		node, err = a.srv.State().NodeByID(nil, alloc.NodeID)
		if err != nil {
			return
		}
		builder.WithNode(node)

		vaultCfg := a.srv.GetConfig().GetVaultForIdentity(wid)
		if vaultCfg != nil && vaultCfg.DefaultIdentity != nil {
			builder.WithVault(vaultCfg.DefaultIdentity.ExtraClaims)
		}

		claims := builder.Build(now)
		err = a.signClaims(claims, idReq, reply)
		break
	}
	return
}

func (a *Alloc) signServices(
	job *structs.Job,
	alloc *structs.Allocation,
	idReq *structs.WorkloadIdentityRequest,
	reply *structs.AllocIdentitiesResponse,
	now time.Time,
) (widFound bool, err error) {
	wid := idReq.WIHandle

	// services can be on the level of task groups or tasks
	for _, tg := range job.TaskGroups {
		for _, service := range tg.Services {
			if service.IdentityHandle(nil).Equal(wid) {
				claims := structs.NewIdentityClaimsBuilder(
					alloc.Job, alloc, &idReq.WIHandle, service.Identity).
					WithConsul().
					WithService(service).
					Build(now)
				return true, a.signClaims(claims, idReq, reply)
			}
		}
		for _, task := range tg.Tasks {
			for _, service := range task.Services {
				if service.IdentityHandle(nil).Equal(wid) {
					claims := structs.NewIdentityClaimsBuilder(
						alloc.Job, alloc, &idReq.WIHandle, service.Identity).
						WithTask(task).
						WithConsul().
						WithService(service).
						Build(now)
					return true, a.signClaims(claims, idReq, reply)
				}
			}
		}
	}
	return
}

func (a *Alloc) signClaims(
	claims *structs.IdentityClaims,
	idReq *structs.WorkloadIdentityRequest,
	reply *structs.AllocIdentitiesResponse,
) error {
	token, _, err := a.srv.encrypter.SignClaims(claims)
	if err != nil {
		return err
	}
	reply.SignedIdentities = append(reply.SignedIdentities, &structs.SignedWorkloadIdentity{
		WorkloadIdentityRequest: *idReq,
		JWT:                     token,
		Expiration:              claims.Expiry.Time(),
	})

	return nil
}
