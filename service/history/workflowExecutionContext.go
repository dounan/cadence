// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package history

import (
	"context"
	"fmt"
	"time"

	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/backoff"
	"github.com/uber/cadence/common/clock"
	"github.com/uber/cadence/common/locks"
	"github.com/uber/cadence/common/log"
	"github.com/uber/cadence/common/log/tag"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

type (
	workflowExecutionContext interface {
		getDomainName() string
		getDomainID() string
		getExecution() *workflow.WorkflowExecution
		getLogger() log.Logger

		loadWorkflowExecution() (mutableState, error)
		loadExecutionStats() (*persistence.ExecutionStats, error)
		clear()

		lock(ctx context.Context) error
		unlock()

		getHistorySize() int64
		setHistorySize(size int64)

		persistFirstWorkflowEvents(
			workflowEvents *persistence.WorkflowEvents,
		) (int64, error)
		persistNonFirstWorkflowEvents(
			workflowEvents *persistence.WorkflowEvents,
		) (int64, error)

		createWorkflowExecution(
			newWorkflow *persistence.WorkflowSnapshot,
			historySize int64,
			now time.Time,
			createMode int,
			prevRunID string,
			prevLastWriteVersion int64,
		) error
		conflictResolveWorkflowExecution(
			now time.Time,
			prevRunID string,
			prevLastWriteVersion int64,
			prevState int,
			resetMutableState mutableState,
			resetHistorySize int64,
		) (mutableState, error)
		updateWorkflowExecutionAsActive(
			now time.Time,
		) error
		updateWorkflowExecutionWithNewAsActive(
			now time.Time,
			newContext workflowExecutionContext,
			newMutableState mutableState,
		) error
		updateWorkflowExecutionAsPassive(
			now time.Time,
		) error
		updateWorkflowExecutionWithNewAsPassive(
			now time.Time,
			newContext workflowExecutionContext,
			newMutableState mutableState,
		) error
		updateWorkflowExecutionWithNew(
			now time.Time,
			newContext workflowExecutionContext,
			newMutableState mutableState,
			currentWorkflowTransactionPolicy transactionPolicy,
			newWorkflowTransactionPolicy *transactionPolicy,
		) error

		resetWorkflowExecution(
			currMutableState mutableState,
			updateCurr bool,
			closeTask persistence.Task,
			cleanupTask persistence.Task,
			newMutableState mutableState,
			newHistorySize int64,
			newTransferTasks []persistence.Task,
			newTimerTasks []persistence.Task,
			currentReplicationTasks []persistence.Task,
			newReplicationTasks []persistence.Task,
			baseRunID string,
			baseRunNextEventID int64,
		) (retError error)
	}
)

type (
	workflowExecutionContextImpl struct {
		domainID          string
		workflowExecution workflow.WorkflowExecution
		shard             ShardContext
		engine            Engine
		executionManager  persistence.ExecutionManager
		logger            log.Logger
		metricsClient     metrics.Client
		timeSource        clock.TimeSource

		mutex           locks.Mutex
		msBuilder       mutableState
		stats           *persistence.ExecutionStats
		updateCondition int64
	}
)

var _ workflowExecutionContext = (*workflowExecutionContextImpl)(nil)

var (
	persistenceOperationRetryPolicy = common.CreatePersistanceRetryPolicy()
)

func newWorkflowExecutionContext(
	domainID string,
	execution workflow.WorkflowExecution,
	shard ShardContext,
	executionManager persistence.ExecutionManager,
	logger log.Logger,
) *workflowExecutionContextImpl {
	lg := logger.WithTags(
		tag.WorkflowID(execution.GetWorkflowId()),
		tag.WorkflowRunID(execution.GetRunId()),
		tag.WorkflowDomainID(domainID),
	)

	return &workflowExecutionContextImpl{
		domainID:          domainID,
		workflowExecution: execution,
		shard:             shard,
		engine:            shard.GetEngine(),
		executionManager:  executionManager,
		logger:            lg,
		metricsClient:     shard.GetMetricsClient(),
		timeSource:        shard.GetTimeSource(),
		mutex:             locks.NewMutex(),
		stats: &persistence.ExecutionStats{
			HistorySize: 0,
		},
	}
}

func (c *workflowExecutionContextImpl) lock(ctx context.Context) error {
	return c.mutex.Lock(ctx)
}

func (c *workflowExecutionContextImpl) unlock() {
	c.mutex.Unlock()
}

func (c *workflowExecutionContextImpl) clear() {
	c.metricsClient.IncCounter(metrics.WorkflowContextScope, metrics.WorkflowContextCleared)
	c.msBuilder = nil
	c.stats = nil
}

func (c *workflowExecutionContextImpl) getDomainID() string {
	return c.domainID
}

func (c *workflowExecutionContextImpl) getExecution() *workflow.WorkflowExecution {
	return &c.workflowExecution
}

func (c *workflowExecutionContextImpl) getLogger() log.Logger {
	return c.logger
}

func (c *workflowExecutionContextImpl) getDomainName() string {
	domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.domainID)
	if err != nil {
		return ""
	}
	return domainEntry.GetInfo().Name
}

func (c *workflowExecutionContextImpl) getHistorySize() int64 {
	return c.stats.HistorySize
}

func (c *workflowExecutionContextImpl) setHistorySize(size int64) {
	c.stats.HistorySize = size
}

func (c *workflowExecutionContextImpl) loadExecutionStats() (*persistence.ExecutionStats, error) {
	_, err := c.loadWorkflowExecution()
	if err != nil {
		return nil, err
	}
	return c.stats, nil
}

func (c *workflowExecutionContextImpl) loadWorkflowExecution() (mutableState, error) {
	err := c.loadWorkflowExecutionInternal()
	if err != nil {
		return nil, err
	}
	err = c.updateVersion()
	if err != nil {
		return nil, err
	}
	return c.msBuilder, nil
}

func (c *workflowExecutionContextImpl) loadWorkflowExecutionInternal() error {
	if c.msBuilder != nil {
		return nil
	}

	response, err := c.getWorkflowExecutionWithRetry(&persistence.GetWorkflowExecutionRequest{
		DomainID:  c.domainID,
		Execution: c.workflowExecution,
	})
	if err != nil {
		return err
	}

	c.msBuilder = newMutableStateBuilder(
		c.shard,
		c.shard.GetEventsCache(),
		c.logger,
		c.getDomainName(),
	)
	c.msBuilder.Load(response.State)
	c.stats = response.State.ExecutionStats
	c.updateCondition = response.State.ExecutionInfo.NextEventID

	// finally emit execution and session stats
	emitWorkflowExecutionStats(
		c.metricsClient,
		c.getDomainName(),
		response.MutableStateStats,
		c.stats.HistorySize,
	)
	return nil
}

func (c *workflowExecutionContextImpl) updateVersion() error {
	if c.shard.GetService().GetClusterMetadata().IsGlobalDomainEnabled() && c.msBuilder.GetReplicationState() != nil {
		if !c.msBuilder.IsWorkflowExecutionRunning() {
			// we should not update the version on mutable state when the workflow is finished
			return nil
		}
		// Support for global domains is enabled and we are performing an update for global domain
		domainEntry, err := c.shard.GetDomainCache().GetDomainByID(c.domainID)
		if err != nil {
			return err
		}
		c.msBuilder.UpdateReplicationStateVersion(domainEntry.GetFailoverVersion(), false)

		// this is a hack, only create replication task if have # target cluster > 1, for more see #868
		c.msBuilder.UpdateReplicationPolicy(domainEntry.GetReplicationPolicy())
	}
	return nil
}

func (c *workflowExecutionContextImpl) createWorkflowExecution(
	newWorkflow *persistence.WorkflowSnapshot,
	historySize int64,
	now time.Time,
	createMode int,
	prevRunID string,
	prevLastWriteVersion int64,
) error {

	createRequest := &persistence.CreateWorkflowExecutionRequest{
		// workflow create mode & prev run ID & version
		CreateWorkflowMode:       createMode,
		PreviousRunID:            prevRunID,
		PreviousLastWriteVersion: prevLastWriteVersion,

		NewWorkflowSnapshot: *newWorkflow,
	}

	createRequest.NewWorkflowSnapshot.ExecutionStats = &persistence.ExecutionStats{
		HistorySize: historySize,
	}

	_, err := c.createWorkflowExecutionWithRetry(createRequest)
	if err != nil {
		return err
	}

	c.notifyTasks(
		newWorkflow.TransferTasks,
		newWorkflow.ReplicationTasks,
		newWorkflow.TimerTasks,
	)
	return nil
}

func (c *workflowExecutionContextImpl) conflictResolveWorkflowExecution(
	now time.Time,
	prevRunID string,
	prevLastWriteVersion int64,
	prevState int,
	resetMutableState mutableState,
	resetHistorySize int64,
) (mutableState, error) {

	// this only resets one mutableState for a workflow
	resetWorkflow, workflowEventsSeq, err := resetMutableState.CloseTransactionAsSnapshot(
		now,
		transactionPolicyPassive,
	)
	if err != nil {
		return nil, err
	}
	if len(workflowEventsSeq) != 0 {
		return nil, &workflow.BadRequestError{
			Message: "reset mutable state should not generate new events",
		}
	}

	resetWorkflow.ExecutionStats = &persistence.ExecutionStats{
		HistorySize: resetHistorySize,
	}

	if err := c.shard.ConflictResolveWorkflowExecution(&persistence.ConflictResolveWorkflowExecutionRequest{
		// previous workflow information
		PrevRunID:            prevRunID,
		PrevLastWriteVersion: prevLastWriteVersion,
		PrevState:            prevState,

		ResetWorkflowSnapshot: *resetWorkflow,
	}); err != nil {
		return nil, err
	}

	c.notifyTasks(
		resetWorkflow.TransferTasks,
		resetWorkflow.ReplicationTasks,
		resetWorkflow.TimerTasks,
	)

	c.clear()
	return c.loadWorkflowExecution()
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionAsActive(
	now time.Time,
) error {

	return c.updateWorkflowExecutionWithNew(
		now,
		nil,
		nil,
		transactionPolicyActive,
		nil,
	)
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionWithNewAsActive(
	now time.Time,
	newContext workflowExecutionContext,
	newMutableState mutableState,
) error {

	return c.updateWorkflowExecutionWithNew(
		now,
		newContext,
		newMutableState,
		transactionPolicyActive,
		transactionPolicyActive.ptr(),
	)
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionAsPassive(
	now time.Time,
) error {

	return c.updateWorkflowExecutionWithNew(
		now,
		nil,
		nil,
		transactionPolicyPassive,
		nil,
	)
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionWithNewAsPassive(
	now time.Time,
	newContext workflowExecutionContext,
	newMutableState mutableState,
) error {

	return c.updateWorkflowExecutionWithNew(
		now,
		newContext,
		newMutableState,
		transactionPolicyPassive,
		transactionPolicyPassive.ptr(),
	)
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionWithNew(
	now time.Time,
	newContext workflowExecutionContext,
	newMutableState mutableState,
	currentWorkflowTransactionPolicy transactionPolicy,
	newWorkflowTransactionPolicy *transactionPolicy,
) (retError error) {

	defer func() {
		if retError != nil {
			c.clear()
		}
	}()

	currentWorkflow, workflowEventsSeq, err := c.msBuilder.CloseTransactionAsMutation(now, currentWorkflowTransactionPolicy)
	if err != nil {
		return err
	}
	currentWorkflowSize := c.getHistorySize()
	for _, workflowEvents := range workflowEventsSeq {
		eventsSize, err := c.persistNonFirstWorkflowEvents(workflowEvents)
		if err != nil {
			return err
		}
		currentWorkflowSize += eventsSize
	}
	c.setHistorySize(currentWorkflowSize)
	currentWorkflow.ExecutionStats = &persistence.ExecutionStats{
		HistorySize: currentWorkflowSize,
	}

	var newWorkflow *persistence.WorkflowSnapshot
	if newContext != nil && newMutableState != nil && newWorkflowTransactionPolicy != nil {

		defer func() {
			if retError != nil {
				newContext.clear()
			}
		}()

		newWorkflow, workflowEventsSeq, err = newMutableState.CloseTransactionAsSnapshot(now, *newWorkflowTransactionPolicy)
		if err != nil {
			return err
		}
		newWorkflowSizeSize := newContext.getHistorySize()
		eventsSize, err := c.persistFirstWorkflowEvents(workflowEventsSeq[0])
		if err != nil {
			return err
		}
		newWorkflowSizeSize += eventsSize
		newContext.setHistorySize(newWorkflowSizeSize)
		newWorkflow.ExecutionStats = &persistence.ExecutionStats{
			HistorySize: newWorkflowSizeSize,
		}
	}

	if err := c.mergeContinueAsNewReplicationTasks(
		currentWorkflow,
		newWorkflow,
	); err != nil {
		return err
	}

	resp, err := c.updateWorkflowExecutionWithRetry(&persistence.UpdateWorkflowExecutionRequest{
		// RangeID , this is set by shard context
		UpdateWorkflowMutation: *currentWorkflow,
		NewWorkflowSnapshot:    newWorkflow,
		// Encoding, this is set by shard context
	})
	if err != nil {
		return err
	}

	// TODO remove updateCondition in favor of condition in mutable state
	c.updateCondition = currentWorkflow.ExecutionInfo.NextEventID

	// for any change in the workflow, send a event
	c.engine.NotifyNewHistoryEvent(newHistoryEventNotification(
		c.domainID,
		&c.workflowExecution,
		c.msBuilder.GetLastFirstEventID(),
		c.msBuilder.GetNextEventID(),
		c.msBuilder.GetPreviousStartedEventID(),
		c.msBuilder.IsWorkflowExecutionRunning(),
		c.msBuilder.GetExecutionInfo().CloseStatus,
	))

	// notify current workflow tasks
	c.notifyTasks(
		currentWorkflow.TransferTasks,
		currentWorkflow.ReplicationTasks,
		currentWorkflow.TimerTasks,
	)

	// notify new workflow tasks
	if newWorkflow != nil {
		c.notifyTasks(
			newWorkflow.TransferTasks,
			newWorkflow.ReplicationTasks,
			newWorkflow.TimerTasks,
		)
	}

	// finally emit session stats
	domainName := c.getDomainName()
	emitWorkflowHistoryStats(
		c.metricsClient,
		domainName,
		int(c.stats.HistorySize),
		int(c.msBuilder.GetNextEventID()-1),
	)
	emitSessionUpdateStats(
		c.metricsClient,
		domainName,
		resp.MutableStateUpdateSessionStats,
	)
	// emit workflow completion stats if any
	if currentWorkflow.ExecutionInfo.State == persistence.WorkflowStateCompleted {
		if event, ok := c.msBuilder.GetCompletionEvent(); ok {
			emitWorkflowCompletionStats(c.metricsClient, domainName, event)
		}
	}

	return nil
}

func (c *workflowExecutionContextImpl) notifyTasks(
	transferTasks []persistence.Task,
	replicationTasks []persistence.Task,
	timerTasks []persistence.Task,
) {
	c.engine.NotifyNewTransferTasks(transferTasks)
	c.engine.NotifyNewReplicationTasks(replicationTasks)
	c.engine.NotifyNewTimerTasks(timerTasks)
}

func (c *workflowExecutionContextImpl) mergeContinueAsNewReplicationTasks(
	currentWorkflowMutation *persistence.WorkflowMutation,
	newWorkflowSnapshot *persistence.WorkflowSnapshot,
) error {
	if currentWorkflowMutation.ExecutionInfo.CloseStatus != persistence.WorkflowCloseStatusContinuedAsNew {
		return nil
	}

	// current workflow is doing continue as new

	// it is possible that continue as new is done as part of passive logic
	if len(currentWorkflowMutation.ReplicationTasks) == 0 {
		return nil
	}

	if newWorkflowSnapshot == nil || len(newWorkflowSnapshot.ReplicationTasks) != 1 {
		return &workflow.InternalServiceError{
			Message: "unable to find replication task from new workflow for continue as new replication",
		}
	}

	// merge the new run first event batch replication task
	// to current event batch replication task
	newRunTask := newWorkflowSnapshot.ReplicationTasks[0].(*persistence.HistoryReplicationTask)
	newWorkflowSnapshot.ReplicationTasks = nil

	newRunBranchToken := newRunTask.BranchToken
	newRunEventStoreVersion := newRunTask.EventStoreVersion
	taskUpdated := false
	for _, replicationTask := range currentWorkflowMutation.ReplicationTasks {
		if task, ok := replicationTask.(*persistence.HistoryReplicationTask); ok {
			taskUpdated = true
			task.NewRunBranchToken = newRunBranchToken
			task.NewRunEventStoreVersion = newRunEventStoreVersion
		}
	}
	if !taskUpdated {
		return &workflow.InternalServiceError{
			Message: "unable to find replication task from current workflow for continue as new replication",
		}
	}
	return nil
}

func (c *workflowExecutionContextImpl) persistFirstWorkflowEvents(
	workflowEvents *persistence.WorkflowEvents,
) (int64, error) {

	if len(workflowEvents.Events) == 0 {
		return 0, &workflow.InternalServiceError{
			Message: "cannot persist first workflow events with empty events",
		}
	}

	domainID := workflowEvents.DomainID
	workflowID := workflowEvents.WorkflowID
	runID := workflowEvents.RunID
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(workflowEvents.WorkflowID),
		RunId:      common.StringPtr(workflowEvents.RunID),
	}
	branchToken := workflowEvents.BranchToken
	events := workflowEvents.Events
	firstEvent := events[0]

	if len(branchToken) == 0 {
		size, err := c.appendHistoryEventsWithRetry(&persistence.AppendHistoryEventsRequest{
			DomainID:          domainID,
			Execution:         execution,
			FirstEventID:      firstEvent.GetEventId(),
			EventBatchVersion: firstEvent.GetVersion(),
			Events:            events,
			// TransactionID is set by shard context
		})
		return int64(size), err
	}

	size, err := c.appendHistoryV2EventsWithRetry(
		domainID,
		execution,
		&persistence.AppendHistoryNodesRequest{
			IsNewBranch: true,
			Info:        persistence.BuildHistoryGarbageCleanupInfo(domainID, workflowID, runID),
			BranchToken: branchToken,
			Events:      events,
			// TransactionID is set by shard context
		},
	)
	return int64(size), err
}

func (c *workflowExecutionContextImpl) persistNonFirstWorkflowEvents(
	workflowEvents *persistence.WorkflowEvents,
) (int64, error) {

	if len(workflowEvents.Events) == 0 {
		return 0, nil // allow update workflow without events
	}

	domainID := workflowEvents.DomainID
	execution := workflow.WorkflowExecution{
		WorkflowId: common.StringPtr(workflowEvents.WorkflowID),
		RunId:      common.StringPtr(workflowEvents.RunID),
	}
	branchToken := workflowEvents.BranchToken
	events := workflowEvents.Events
	firstEvent := events[0]

	if len(branchToken) == 0 {
		size, err := c.appendHistoryEventsWithRetry(&persistence.AppendHistoryEventsRequest{
			DomainID:          domainID,
			Execution:         execution,
			FirstEventID:      firstEvent.GetEventId(),
			EventBatchVersion: firstEvent.GetVersion(),
			Events:            events,
			// TransactionID is set by shard context
		})
		return int64(size), err
	}

	size, err := c.appendHistoryV2EventsWithRetry(
		domainID,
		execution,
		&persistence.AppendHistoryNodesRequest{
			IsNewBranch: false,
			BranchToken: branchToken,
			Events:      events,
			// TransactionID is set by shard context
		},
	)
	return int64(size), err
}

func (c *workflowExecutionContextImpl) appendHistoryEventsWithRetry(
	request *persistence.AppendHistoryEventsRequest,
) (int64, error) {

	resp := 0
	op := func() error {
		var err error
		resp, err = c.shard.AppendHistoryEvents(request)
		return err
	}

	err := backoff.Retry(
		op,
		persistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	return int64(resp), err
}

func (c *workflowExecutionContextImpl) appendHistoryV2EventsWithRetry(
	domainID string,
	execution workflow.WorkflowExecution,
	request *persistence.AppendHistoryNodesRequest,
) (int64, error) {

	resp := 0
	op := func() error {
		var err error
		resp, err = c.shard.AppendHistoryV2Events(request, domainID, execution)
		return err
	}

	err := backoff.Retry(
		op,
		persistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	return int64(resp), err
}

func (c *workflowExecutionContextImpl) createWorkflowExecutionWithRetry(
	request *persistence.CreateWorkflowExecutionRequest,
) (*persistence.CreateWorkflowExecutionResponse, error) {

	var resp *persistence.CreateWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = c.shard.CreateWorkflowExecution(request)
		return err
	}

	err := backoff.Retry(
		op,
		persistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		return resp, nil
	case *persistence.WorkflowExecutionAlreadyStartedError:
		// it is possible that workflow already exists and caller need to apply
		// workflow ID reuse policy
		return nil, err
	default:
		c.logger.Error(
			"Persistent store operation failure",
			tag.StoreOperationCreateWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func (c *workflowExecutionContextImpl) getWorkflowExecutionWithRetry(
	request *persistence.GetWorkflowExecutionRequest,
) (*persistence.GetWorkflowExecutionResponse, error) {

	var resp *persistence.GetWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = c.executionManager.GetWorkflowExecution(request)

		return err
	}

	err := backoff.Retry(
		op,
		persistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		return resp, nil
	case *workflow.EntityNotExistsError:
		// it is possible that workflow does not exists
		return nil, err
	default:
		c.logger.Error(
			"Persistent fetch operation failure",
			tag.StoreOperationGetWorkflowExecution,
			tag.Error(err),
		)
		return nil, err
	}
}

func (c *workflowExecutionContextImpl) updateWorkflowExecutionWithRetry(
	request *persistence.UpdateWorkflowExecutionRequest,
) (*persistence.UpdateWorkflowExecutionResponse, error) {

	var resp *persistence.UpdateWorkflowExecutionResponse
	op := func() error {
		var err error
		resp, err = c.shard.UpdateWorkflowExecution(request)
		return err
	}

	err := backoff.Retry(
		op, persistenceOperationRetryPolicy,
		common.IsPersistenceTransientError,
	)
	switch err.(type) {
	case nil:
		return resp, nil
	case *persistence.ConditionFailedError:
		// TODO get rid of ErrConflict
		return nil, ErrConflict
	default:
		c.logger.Error(
			"Persistent store operation failure",
			tag.StoreOperationUpdateWorkflowExecution,
			tag.Error(err),
			tag.Number(c.updateCondition),
		)
		return nil, err
	}
}

// this reset is more complex than "resetMutableState", it involes currentMutableState and newMutableState:
// 1. append history to new run
// 2. append history to current run if current run is not closed
// 3. update mutableState(terminate current run if not closed) and create new run
func (c *workflowExecutionContextImpl) resetWorkflowExecution(
	currMutableState mutableState,
	updateCurr bool,
	closeTask persistence.Task,
	cleanupTask persistence.Task,
	newMutableState mutableState,
	newHistorySize int64,
	newTransferTasks []persistence.Task,
	newTimerTasks []persistence.Task,
	currReplicationTasks []persistence.Task,
	newReplicationTasks []persistence.Task,
	baseRunID string,
	baseRunNextEventID int64,
) (retError error) {

	now := c.timeSource.Now()
	currTransferTasks := []persistence.Task{}
	currTimerTasks := []persistence.Task{}
	if closeTask != nil {
		currTransferTasks = append(currTransferTasks, closeTask)
	}
	if cleanupTask != nil {
		currTimerTasks = append(currTimerTasks, cleanupTask)
	}
	setTaskInfo(currMutableState.GetCurrentVersion(), now, currTransferTasks, currTimerTasks)
	setTaskInfo(newMutableState.GetCurrentVersion(), now, newTransferTasks, newTimerTasks)

	// Since we always reset to decision task, there shouldn't be any buffered events.
	// Therefore currently ResetWorkflowExecution persistence API doesn't implement setting buffered events.
	if newMutableState.HasBufferedEvents() {
		retError = &workflow.InternalServiceError{
			Message: fmt.Sprintf("reset workflow execution shouldn't have buffered events"),
		}
		return
	}

	// call FlushBufferedEvents to assign task id to event
	// as well as update last event task id in ms state builder
	retError = currMutableState.FlushBufferedEvents()
	if retError != nil {
		return retError
	}
	retError = newMutableState.FlushBufferedEvents()
	if retError != nil {
		return retError
	}

	if updateCurr {
		hBuilder := currMutableState.GetHistoryBuilder()
		var size int64
		// TODO workflow execution reset logic generates replication tasks in its own business logic
		currentExecutionInfo := currMutableState.GetExecutionInfo()
		size, retError = c.persistNonFirstWorkflowEvents(&persistence.WorkflowEvents{
			DomainID:    currentExecutionInfo.DomainID,
			WorkflowID:  currentExecutionInfo.WorkflowID,
			RunID:       currentExecutionInfo.RunID,
			BranchToken: currMutableState.GetCurrentBranch(),
			Events:      hBuilder.GetHistory().GetEvents(),
		})
		if retError != nil {
			return
		}
		c.stats.HistorySize += int64(size)
	}

	// TODO refactor resetWorkflowExecution to use CloseTransactionAsSnapshot
	//  and CloseTransactionAsMutation correctly
	resetWorkflow, workflowEventsSeq, err := newMutableState.CloseTransactionAsSnapshot(
		c.shard.GetTimeSource().Now(),
		// the reason to use passive policy is because this resetWorkflowExecution function
		// mostly handcraft all parameters to be persisted
		transactionPolicyPassive,
	)
	if err != nil {
		return err
	}
	if len(workflowEventsSeq) != 1 {
		return &workflow.InternalServiceError{
			Message: "reset workflow execution should generate exactly 1 event batch",
		}
	}
	for _, workflowEvents := range workflowEventsSeq {
		eventsSize, err := c.persistNonFirstWorkflowEvents(workflowEvents)
		if err != nil {
			return err
		}
		newHistorySize += eventsSize
	}
	resetWorkflow.ExecutionStats = &persistence.ExecutionStats{
		HistorySize: newHistorySize,
	}
	resetWorkflow.TransferTasks = newTransferTasks
	resetWorkflow.ReplicationTasks = newReplicationTasks
	resetWorkflow.TimerTasks = newTimerTasks

	if len(resetWorkflow.ChildExecutionInfos) > 0 ||
		len(resetWorkflow.SignalInfos) > 0 ||
		len(resetWorkflow.SignalRequestedIDs) > 0 {
		return &workflow.InternalServiceError{
			Message: fmt.Sprintf("something went wrong, we shouldn't see any pending childWF, sending Signal or signal requested"),
		}
	}

	resetWFReq := &persistence.ResetWorkflowExecutionRequest{
		BaseRunID:          baseRunID,
		BaseRunNextEventID: baseRunNextEventID,

		CurrentRunID:          currMutableState.GetExecutionInfo().RunID,
		CurrentRunNextEventID: currMutableState.GetExecutionInfo().NextEventID,

		CurrentWorkflowMutation: nil,

		NewWorkflowSnapshot: *resetWorkflow,
	}

	if updateCurr {
		resetWFReq.CurrentWorkflowMutation = &persistence.WorkflowMutation{
			ExecutionInfo: currMutableState.GetExecutionInfo(),
			ExecutionStats: &persistence.ExecutionStats{
				HistorySize: c.stats.HistorySize,
			},
			ReplicationState: currMutableState.GetReplicationState(),

			UpsertActivityInfos:       []*persistence.ActivityInfo{},
			DeleteActivityInfos:       []int64{},
			UpserTimerInfos:           []*persistence.TimerInfo{},
			DeleteTimerInfos:          []string{},
			UpsertChildExecutionInfos: []*persistence.ChildExecutionInfo{},
			DeleteChildExecutionInfo:  nil,
			UpsertRequestCancelInfos:  []*persistence.RequestCancelInfo{},
			DeleteRequestCancelInfo:   nil,
			UpsertSignalInfos:         []*persistence.SignalInfo{},
			DeleteSignalInfo:          nil,
			UpsertSignalRequestedIDs:  []string{},
			DeleteSignalRequestedID:   "",
			NewBufferedEvents:         []*workflow.HistoryEvent{},
			ClearBufferedEvents:       false,

			TransferTasks:    currTransferTasks,
			ReplicationTasks: currReplicationTasks,
			TimerTasks:       currTimerTasks,

			Condition: c.updateCondition,
		}
	}

	err = c.shard.ResetWorkflowExecution(resetWFReq)
	if err != nil {
		return err
	}

	// notify reset workflow tasks
	c.notifyTasks(
		resetWorkflow.TransferTasks,
		resetWorkflow.ReplicationTasks,
		resetWorkflow.TimerTasks,
	)

	// notify current workflow tasks
	if resetWFReq.CurrentWorkflowMutation != nil {
		c.notifyTasks(
			resetWFReq.CurrentWorkflowMutation.TransferTasks,
			resetWFReq.CurrentWorkflowMutation.ReplicationTasks,
			resetWFReq.CurrentWorkflowMutation.TimerTasks,
		)
	}
	return nil
}
