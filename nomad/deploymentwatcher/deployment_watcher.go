package deploymentwatcher

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/hashicorp/nomad/nomad/structs"
)

const (
	// evalBatchPeriod is the batching length before creating an evaluation to
	// trigger the scheduler when allocations are marked as healthy.
	evalBatchPeriod = 1 * time.Second
)

// deploymentTriggers are the set of functions required to trigger changes on
// behalf of a deployment
type deploymentTriggers interface {
	// createEvaluation is used to create an evaluation.
	createEvaluation(eval *structs.Evaluation) (uint64, error)

	// upsertJob is used to roll back a job when autoreverting for a deployment
	upsertJob(job *structs.Job) (uint64, error)

	// upsertDeploymentStatusUpdate is used to upsert a deployment status update
	// and an optional evaluation and job to upsert
	upsertDeploymentStatusUpdate(u *structs.DeploymentStatusUpdate, eval *structs.Evaluation, job *structs.Job) (uint64, error)

	// upsertDeploymentPromotion is used to promote canaries in a deployment
	upsertDeploymentPromotion(req *structs.ApplyDeploymentPromoteRequest) (uint64, error)

	// upsertDeploymentAllocHealth is used to set the health of allocations in a
	// deployment
	upsertDeploymentAllocHealth(req *structs.ApplyDeploymentAllocHealthRequest) (uint64, error)
}

// deploymentWatcher is used to watch a single deployment and trigger the
// scheduler when allocation health transistions.
type deploymentWatcher struct {
	// queryLimiter is used to limit the rate of blocking queries
	queryLimiter *rate.Limiter

	// deploymentTriggers holds the methods required to trigger changes on behalf of the
	// deployment
	deploymentTriggers

	// DeploymentStateWatchers holds the methods required to watch objects for
	// changes on behalf of the deployment
	DeploymentStateWatchers

	// d is the deployment being watched
	d *structs.Deployment

	// j is the job the deployment is for
	j *structs.Job

	// autorevert is used to lookup if an task group should autorevert on
	// unhealthy allocations
	autorevert map[string]bool

	// outstandingBatch marks whether an outstanding function exists to create
	// the evaluation. Access should be done through the lock
	outstandingBatch bool

	logger *log.Logger
	ctx    context.Context
	exitFn context.CancelFunc
	l      sync.RWMutex
}

// newDeploymentWatcher returns a deployment watcher that is used to watch
// deployments and trigger the scheduler as needed.
func newDeploymentWatcher(
	parent context.Context,
	queryLimiter *rate.Limiter,
	logger *log.Logger,
	watchers DeploymentStateWatchers,
	d *structs.Deployment,
	j *structs.Job,
	triggers deploymentTriggers) *deploymentWatcher {

	ctx, exitFn := context.WithCancel(parent)
	w := &deploymentWatcher{
		queryLimiter:            queryLimiter,
		d:                       d,
		j:                       j,
		autorevert:              make(map[string]bool, len(j.TaskGroups)),
		DeploymentStateWatchers: watchers,
		deploymentTriggers:      triggers,
		logger:                  logger,
		ctx:                     ctx,
		exitFn:                  exitFn,
	}

	for _, tg := range j.TaskGroups {
		autorevert := false
		if tg.Update != nil && tg.Update.AutoRevert {
			autorevert = true
		}
		w.autorevert[tg.Name] = autorevert
	}

	go w.watch()
	return w
}

func (w *deploymentWatcher) SetAllocHealth(
	req *structs.DeploymentAllocHealthRequest,
	resp *structs.DeploymentUpdateResponse) error {

	// If we are failing the deployment, update the status and potentially
	// rollback
	var j *structs.Job
	var u *structs.DeploymentStatusUpdate

	// If there are unhealthy allocations we need to mark the deployment as
	// failed and check if we should roll back to a stable job.
	if l := len(req.UnhealthyAllocationIDs); l != 0 {
		unhealthy := make(map[string]struct{}, l)
		for _, alloc := range req.UnhealthyAllocationIDs {
			unhealthy[alloc] = struct{}{}
		}

		// Get the allocations for the deployment
		args := &structs.DeploymentSpecificRequest{DeploymentID: req.DeploymentID}
		var resp structs.AllocListResponse
		if err := w.Allocations(args, &resp); err != nil {
			return err
		}

		desc := structs.DeploymentStatusDescriptionFailedAllocations
		for _, alloc := range resp.Allocations {
			// Check that the alloc has been marked unhealthy
			if _, ok := unhealthy[alloc.ID]; !ok {
				continue
			}

			// Check if the group has autorevert set
			if !w.autorevert[alloc.TaskGroup] {
				continue
			}

			var err error
			j, err = w.latestStableJob()
			if err != nil {
				return err
			}

			desc = fmt.Sprintf("%s - rolling back to job version %d", desc, j.Version)
			break
		}

		u = w.getDeploymentStatusUpdate(structs.DeploymentStatusFailed, desc)
	}

	// Create the request
	areq := &structs.ApplyDeploymentAllocHealthRequest{
		DeploymentAllocHealthRequest: *req,
		Eval:             w.getEval(),
		DeploymentUpdate: u,
		Job:              j,
	}

	index, err := w.upsertDeploymentAllocHealth(areq)
	if err != nil {
		return err
	}

	// Build the response
	resp.EvalID = areq.Eval.ID
	resp.EvalCreateIndex = index
	resp.DeploymentModifyIndex = index
	return nil
}

func (w *deploymentWatcher) PromoteDeployment(
	req *structs.DeploymentPromoteRequest,
	resp *structs.DeploymentUpdateResponse) error {

	// Create the request
	areq := &structs.ApplyDeploymentPromoteRequest{
		DeploymentPromoteRequest: *req,
		Eval: w.getEval(),
	}

	index, err := w.upsertDeploymentPromotion(areq)
	if err != nil {
		return err
	}

	// Build the response
	resp.EvalID = areq.Eval.ID
	resp.EvalCreateIndex = index
	resp.DeploymentModifyIndex = index
	return nil
}

func (w *deploymentWatcher) PauseDeployment(
	req *structs.DeploymentPauseRequest,
	resp *structs.DeploymentUpdateResponse) error {
	// Determine the status we should transistion to and if we need to create an
	// evaluation
	status, desc := structs.DeploymentStatusPaused, structs.DeploymentStatusDescriptionPaused
	var eval *structs.Evaluation
	evalID := ""
	if !req.Pause {
		status, desc = structs.DeploymentStatusRunning, structs.DeploymentStatusDescriptionRunning
		eval = w.getEval()
		evalID = eval.ID
	}
	update := w.getDeploymentStatusUpdate(status, desc)

	// Commit the change
	i, err := w.upsertDeploymentStatusUpdate(update, eval, nil)
	if err != nil {
		return err
	}

	// Build the response
	resp.EvalID = evalID
	resp.EvalCreateIndex = i
	resp.DeploymentModifyIndex = i
	return nil
}

// StopWatch stops watching the deployment. This should be called whenever a
// deployment is completed or the watcher is no longer needed.
func (w *deploymentWatcher) StopWatch() {
	w.exitFn()
}

// watch is the long running watcher that takes actions upon allocation changes
func (w *deploymentWatcher) watch() {
	latestEval := uint64(0)
	for {
		// Block getting all allocations that are part of the deployment using
		// the last evaluation index. This will have us block waiting for
		// something to change past what the scheduler has evaluated.
		allocs, err := w.getAllocs(latestEval)
		if err != nil {
			if err == context.Canceled {
				return
			}

			w.logger.Printf("[ERR] nomad.deployment_watcher: failed to retrieve allocations for deployment %q: %v", w.d.ID, err)
		}

		// Get the latest evaluation snapshot index
		latestEval, err = w.latestEvalIndex()
		if err != nil {
			if err == context.Canceled {
				return
			}

			w.logger.Printf("[ERR] nomad.deployment_watcher: failed to determine last evaluation index for job %q: %v", w.d.JobID, err)
		}

		// Create an evaluation trigger if there is any allocation whose
		// deployment status has been updated past the latest eval index.
		createEval, failDeployment, rollback := false, false, false
		for _, alloc := range allocs {
			if alloc.DeploymentStatus == nil || alloc.DeploymentStatus.ModifyIndex <= latestEval {
				continue
			}

			// We need to create an eval
			createEval = true

			if alloc.DeploymentStatus.IsUnhealthy() {
				// Check if the group has autorevert set
				if w.autorevert[alloc.TaskGroup] {
					rollback = true
				}

				// Since we have an unhealthy allocation, fail the deployment
				failDeployment = true
			}

			// All conditions have been hit so we can break
			if createEval && failDeployment && rollback {
				break
			}
		}

		// Change the deployments status to failed
		if failDeployment {
			// Default description
			desc := structs.DeploymentStatusDescriptionFailedAllocations

			// Rollback to the old job if necessary
			var j *structs.Job
			if rollback {
				var err error
				j, err = w.latestStableJob()
				if err != nil {
					w.logger.Printf("[ERR] nomad.deployment_watcher: failed to lookup latest stable job for %q: %v", w.d.JobID, err)
				}

				// Description should include that the job is being rolled back to
				// version N
				desc = fmt.Sprintf("%s - rolling back to job version %d", desc, j.Version)
			}

			// Update the status of the deployment to failed and create an
			// evaluation.
			e, u := w.getEval(), w.getDeploymentStatusUpdate(structs.DeploymentStatusFailed, desc)
			if index, err := w.upsertDeploymentStatusUpdate(u, e, j); err != nil {
				w.logger.Printf("[ERR] nomad.deployment_watcher: failed to update deployment %q status: %v", w.d.ID, err)
			} else {
				latestEval = index
			}
		} else if createEval {
			// Create an eval to push the deployment along
			w.createEvalBatched()
		}
	}
}

// latestStableJob returns the latest stable job. It may be nil if none exist
func (w *deploymentWatcher) latestStableJob() (*structs.Job, error) {
	args := &structs.JobSpecificRequest{JobID: w.d.JobID}
	var resp structs.JobVersionsResponse
	if err := w.GetJobVersions(args, &resp); err != nil {
		return nil, err
	}

	var stable *structs.Job
	for _, job := range resp.Versions {
		if job.Stable {
			stable = job
			break
		}
	}

	return stable, nil
}

// createEval creates an evaluation for the job and commits it to Raft.
func (w *deploymentWatcher) createEval() (evalID string, evalCreateIndex uint64, err error) {
	e := w.getEval()
	evalCreateIndex, err = w.createEvaluation(e)
	return e.ID, evalCreateIndex, err
}

// createEvalBatched creates an eval but batches calls together
func (w *deploymentWatcher) createEvalBatched() {
	w.l.Lock()
	defer w.l.Unlock()

	if w.outstandingBatch {
		return
	}

	go func() {
		// Sleep til the batching period is over
		time.Sleep(evalBatchPeriod)

		w.l.Lock()
		w.outstandingBatch = false
		w.l.Unlock()

		if _, _, err := w.createEval(); err != nil {
			w.logger.Printf("[ERR] nomad.deployment_watcher: failed to create evaluation for deployment %q: %v", w.d.ID, err)
		}
	}()
}

// getEval returns an evaluation suitable for the deployment
func (w *deploymentWatcher) getEval() *structs.Evaluation {
	return &structs.Evaluation{
		ID:           structs.GenerateUUID(),
		Priority:     w.j.Priority,
		Type:         w.j.Type,
		TriggeredBy:  structs.EvalTriggerRollingUpdate,
		JobID:        w.j.ID,
		DeploymentID: w.d.ID,
		Status:       structs.EvalStatusPending,
	}
}

// getDeploymentStatusUpdate returns a deployment status update
func (w *deploymentWatcher) getDeploymentStatusUpdate(status, desc string) *structs.DeploymentStatusUpdate {
	return &structs.DeploymentStatusUpdate{
		DeploymentID:      w.d.ID,
		Status:            status,
		StatusDescription: desc,
	}
}

// getAllocs retrieves the allocations that are part of the deployment blocking
// at the given index.
func (w *deploymentWatcher) getAllocs(index uint64) ([]*structs.AllocListStub, error) {
	// Build the request
	args := &structs.DeploymentSpecificRequest{
		DeploymentID: w.d.ID,
		QueryOptions: structs.QueryOptions{
			MinQueryIndex: index,
		},
	}
	var resp structs.AllocListResponse

	for resp.Index <= index {
		if err := w.queryLimiter.Wait(w.ctx); err != nil {
			return nil, err
		}

		if err := w.Allocations(args, &resp); err != nil {
			return nil, err
		}
	}

	return resp.Allocations, nil
}

// latestEvalIndex returns the index of the last evaluation created for
// the job. The index is used to determine if an allocation update requires an
// evaluation to be triggered.
func (w *deploymentWatcher) latestEvalIndex() (uint64, error) {
	if err := w.queryLimiter.Wait(w.ctx); err != nil {
		return 0, err
	}

	args := &structs.JobSpecificRequest{
		JobID: w.d.JobID,
	}
	var resp structs.JobEvaluationsResponse
	err := w.Evaluations(args, &resp)
	if err != nil {
		return 0, err
	}

	if len(resp.Evaluations) == 0 {
		return resp.Index, nil
	}

	// Prefer using the snapshot index. Otherwise use the create index
	e := resp.Evaluations[0]
	if e.SnapshotIndex != 0 {
		return e.SnapshotIndex, nil
	}

	return e.CreateIndex, nil
}
