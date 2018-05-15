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
	"fmt"
	"time"

	"github.com/uber-common/bark"
	m "github.com/uber/cadence/.gen/go/matching"
	workflow "github.com/uber/cadence/.gen/go/shared"
	"github.com/uber/cadence/client/matching"
	"github.com/uber/cadence/common"
	"github.com/uber/cadence/common/logging"
	"github.com/uber/cadence/common/metrics"
	"github.com/uber/cadence/common/persistence"
)

type (
	timerQueueActiveProcessorImpl struct {
		shard                   ShardContext
		historyService          *historyEngineImpl
		cache                   *historyCache
		timerTaskFilter         timerTaskFilter
		logger                  bark.Logger
		metricsClient           metrics.Client
		currentClusterName      string
		matchingClient          matching.Client
		timerGate               LocalTimerGate
		timerQueueProcessorBase *timerQueueProcessorBase
		timerQueueAckMgr        timerQueueAckMgr
	}
)

func newTimerQueueActiveProcessor(shard ShardContext, historyService *historyEngineImpl, matchingClient matching.Client, logger bark.Logger) *timerQueueActiveProcessorImpl {
	clusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	timeNow := func() time.Time {
		return shard.GetCurrentTime(clusterName)
	}
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowCluster: clusterName,
	})
	timerTaskFilter := func(timer *persistence.TimerTaskInfo) (bool, error) {
		domainEntry, err := shard.GetDomainCache().GetDomainByID(timer.DomainID)
		if err != nil {
			// it is possible that domain is deleted,
			// we should treat that domain being active
			if _, ok := err.(*workflow.EntityNotExistsError); !ok {
				return false, err
			}
			return true, nil
		}
		if domainEntry.IsGlobalDomain() && clusterName != domainEntry.GetReplicationConfig().ActiveClusterName {
			// timer task does not belong to cluster name
			return false, nil
		}
		return true, nil
	}

	timerGate := NewLocalTimerGate()
	// this will trigger a timer gate fire event immediately
	timerGate.Update(time.Time{})
	timerQueueAckMgr := newTimerQueueAckMgr(shard, historyService.metricsClient, clusterName, logger)
	processor := &timerQueueActiveProcessorImpl{
		shard:                   shard,
		historyService:          historyService,
		cache:                   historyService.historyCache,
		timerTaskFilter:         timerTaskFilter,
		logger:                  logger,
		matchingClient:          matchingClient,
		metricsClient:           historyService.metricsClient,
		currentClusterName:      clusterName,
		timerGate:               timerGate,
		timerQueueProcessorBase: newTimerQueueProcessorBase(shard, historyService, timerQueueAckMgr, timeNow, logger),
		timerQueueAckMgr:        timerQueueAckMgr,
	}
	processor.timerQueueProcessorBase.timerProcessor = processor
	return processor
}

func newTimerQueueFailoverProcessor(shard ShardContext, historyService *historyEngineImpl, domainID string, standbyClusterName string, matchingClient matching.Client, logger bark.Logger) *timerQueueActiveProcessorImpl {
	clusterName := shard.GetService().GetClusterMetadata().GetCurrentClusterName()
	timeNow := func() time.Time {
		// should use current cluster's time when doing domain failover
		return shard.GetCurrentTime(clusterName)
	}
	logger = logger.WithFields(bark.Fields{
		logging.TagWorkflowCluster: clusterName,
	})
	timerTaskFilter := func(timer *persistence.TimerTaskInfo) (bool, error) {
		if timer.DomainID == domainID {
			return true, nil
		}
		return false, nil
	}

	timerQueueAckMgr := newTimerQueueFailoverAckMgr(shard, historyService.metricsClient, standbyClusterName, logger)
	processor := &timerQueueActiveProcessorImpl{
		shard:                   shard,
		historyService:          historyService,
		cache:                   historyService.historyCache,
		timerTaskFilter:         timerTaskFilter,
		logger:                  logger,
		metricsClient:           historyService.metricsClient,
		matchingClient:          matchingClient,
		timerGate:               NewLocalTimerGate(),
		timerQueueProcessorBase: newTimerQueueProcessorBase(shard, historyService, timerQueueAckMgr, timeNow, logger),
		timerQueueAckMgr:        timerQueueAckMgr,
	}
	processor.timerQueueProcessorBase.timerProcessor = processor
	return processor
}

func (t *timerQueueActiveProcessorImpl) Start() {
	t.timerQueueProcessorBase.Start()
}

func (t *timerQueueActiveProcessorImpl) Stop() {
	t.timerGate.Close()
	t.timerQueueProcessorBase.Stop()
}

func (t *timerQueueActiveProcessorImpl) getTimerFiredCount() uint64 {
	return t.timerQueueProcessorBase.getTimerFiredCount()
}

func (t *timerQueueActiveProcessorImpl) getTimerGate() TimerGate {
	return t.timerGate
}

// NotifyNewTimers - Notify the processor about the new active timer events arrival.
// This should be called each time new timer events arrives, otherwise timers maybe fired unexpected.
func (t *timerQueueActiveProcessorImpl) notifyNewTimers(timerTasks []persistence.Task) {
	t.timerQueueProcessorBase.notifyNewTimers(timerTasks, metrics.NewActiveTimerCounter)
}

func (t *timerQueueActiveProcessorImpl) process(timerTask *persistence.TimerTaskInfo) error {
	ok, err := t.timerTaskFilter(timerTask)
	if err != nil {
		return err
	} else if !ok {
		t.timerQueueAckMgr.completeTimerTask(timerTask)
		return nil
	}

	taskID := TimerSequenceID{VisibilityTimestamp: timerTask.VisibilityTimestamp, TaskID: timerTask.TaskID}
	t.logger.Debugf("Processing timer: (%s), for WorkflowID: %v, RunID: %v, Type: %v, TimeoutType: %v, EventID: %v, Attempt: %v",
		taskID, timerTask.WorkflowID, timerTask.RunID, t.timerQueueProcessorBase.getTimerTaskType(timerTask.TaskType),
		workflow.TimeoutType(timerTask.TimeoutType).String(), timerTask.EventID, timerTask.ScheduleAttempt)

	scope := metrics.TimerQueueProcessorScope
	switch timerTask.TaskType {
	case persistence.TaskTypeUserTimer:
		scope = metrics.TimerTaskUserTimerScope
		err = t.processExpiredUserTimer(timerTask)

	case persistence.TaskTypeActivityTimeout:
		scope = metrics.TimerTaskActivityTimeoutScope
		err = t.processActivityTimeout(timerTask)

	case persistence.TaskTypeDecisionTimeout:
		scope = metrics.TimerTaskDecisionTimeoutScope
		err = t.processDecisionTimeout(timerTask)

	case persistence.TaskTypeWorkflowTimeout:
		scope = metrics.TimerTaskWorkflowTimeoutScope
		err = t.processWorkflowTimeout(timerTask)

	case persistence.TaskTypeRetryTimer:
		scope = metrics.TimerTaskRetryTimerScope
		err = t.processRetryTimer(timerTask)

	case persistence.TaskTypeDeleteHistoryEvent:
		scope = metrics.TimerTaskDeleteHistoryEvent
		err = t.timerQueueProcessorBase.processDeleteHistoryEvent(timerTask)
	}

	if err != nil {
		if _, ok := err.(*workflow.EntityNotExistsError); ok {
			// Timer could fire after the execution is deleted.
			// In which case just ignore the error so we can complete the timer task.
			t.timerQueueAckMgr.completeTimerTask(timerTask)
			err = nil
		}
		if err != nil {
			t.metricsClient.IncCounter(scope, metrics.TaskFailures)
		}
	} else {
		t.timerQueueAckMgr.completeTimerTask(timerTask)
	}

	return err
}

func (t *timerQueueActiveProcessorImpl) processExpiredUserTimer(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerTaskUserTimerScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerTaskUserTimerScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)

		if !msBuilder.isWorkflowExecutionRunning() {
			// Workflow is completed.
			return nil
		}

		var timerTasks []persistence.Task
		scheduleNewDecision := false

	ExpireUserTimers:
		for _, td := range tBuilder.GetUserTimers(msBuilder) {
			hasTimer, ti := tBuilder.GetUserTimer(td.TimerID)
			if !hasTimer {
				t.logger.Debugf("Failed to find in memory user timer: %s", td.TimerID)
				return fmt.Errorf("Failed to find in memory user timer: %s", td.TimerID)
			}

			if isExpired := tBuilder.IsTimerExpired(td, task.VisibilityTimestamp); isExpired {
				// Add TimerFired event to history.
				if msBuilder.AddTimerFiredEvent(ti.StartedID, ti.TimerID) == nil {
					return errFailedToAddTimerFiredEvent
				}

				scheduleNewDecision = !msBuilder.HasPendingDecisionTask()
			} else {
				// See if we have next timer in list to be created.
				if !td.TaskCreated {
					nextTask := tBuilder.createNewTask(td)
					timerTasks = []persistence.Task{nextTask}

					// Update the task ID tracking the corresponding timer task.
					ti.TaskID = nextTask.GetTaskID()
					msBuilder.UpdateUserTimer(ti.TimerID, ti)
					defer t.notifyNewTimers(timerTasks)
				}

				// Done!
				break ExpireUserTimers
			}
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
		// the history and try the operation again.
		err := t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, timerTasks, nil)
		if err != nil {
			if err == ErrConflict {
				continue Update_History_Loop
			}
		}
		return err
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processActivityTimeout(timerTask *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerTaskActivityTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerTaskActivityTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(timerTask))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)

		scheduleID := timerTask.EventID
		// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
		// some extreme cassandra failure cases.
		if scheduleID >= msBuilder.GetNextEventID() {
			t.metricsClient.IncCounter(metrics.TimerQueueProcessorScope, metrics.StaleMutableStateCounter)
			t.logger.Debugf("processActivityTimeout: scheduleID mismatch. MS NextEventID: %v, scheduleID: %v",
				msBuilder.GetNextEventID(), scheduleID)
			// Reload workflow execution history
			context.clear()
			continue Update_History_Loop
		}

		if !msBuilder.isWorkflowExecutionRunning() {
			// Workflow is completed.
			return nil
		}

		ai, running := msBuilder.GetActivityInfo(scheduleID)
		if !running {
			// activity already closed
			return nil
		}
		if int64(ai.Attempt) != timerTask.ScheduleAttempt && timerTask.TimeoutType != int(workflow.TimeoutTypeScheduleToClose) {
			// timer was created for older attempts
			return nil
		}

		// If current one is HB task then we may need to create the next heartbeat timer.  Clear the create flag for this
		// heartbeat timer so we can create it again if needed.
		// NOTE: When record activity HB comes in we only update last heartbeat timestamp, this is the place
		// where we create next timer task based on that new updated timestamp.
		isHeartBeatTask := timerTask.TimeoutType == int(workflow.TimeoutTypeHeartbeat)
		if isHeartBeatTask {
			ai.TimerTaskStatus = ai.TimerTaskStatus &^ TimerTaskStatusCreatedHeartbeat
			msBuilder.UpdateActivity(ai)
		}

		var timerTasks []persistence.Task
		updateHistory := false
		createNewTimer := false

	ExpireActivityTimers:
		for _, td := range tBuilder.GetActivityTimers(msBuilder) {
			ai, isRunning := msBuilder.GetActivityInfo(td.ActivityID)
			if !isRunning {
				//  We might have time out this activity already.
				continue ExpireActivityTimers
			}

			if isExpired := tBuilder.IsTimerExpired(td, timerTask.VisibilityTimestamp); isExpired {
				timeoutType := td.TimeoutType
				t.logger.Debugf("Activity TimeoutType: %v, scheduledID: %v, startedId: %v. \n",
					timeoutType, ai.ScheduleID, ai.StartedID)

				if td.Attempt < ai.Attempt && timeoutType != workflow.TimeoutTypeScheduleToClose {
					// retry could update ai.Attempt, and we should ignore further timeouts for previous attempt
					continue
				}

				if timeoutType != workflow.TimeoutTypeScheduleToStart {
					// ScheduleToStart (queue timeout) is not retriable. Instead of retry, customer should set larger
					// ScheduleToStart timeout.
					retryTask := msBuilder.CreateRetryTimer(ai, getTimeoutErrorReason(timeoutType))
					if retryTask != nil {
						timerTasks = append(timerTasks, retryTask)
						createNewTimer = true

						t.logger.Debugf("Ignore ActivityTimeout (%v) as retry is needed. New attempt: %v, retry backoff duration: %v.",
							timeoutType, ai.Attempt, retryTask.(*persistence.RetryTimerTask).VisibilityTimestamp.Sub(time.Now()))

						continue
					}
				}

				switch timeoutType {
				case workflow.TimeoutTypeScheduleToClose:
					{
						t.metricsClient.IncCounter(metrics.TimerTaskActivityTimeoutScope, metrics.ScheduleToCloseTimeoutCounter)
						if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
							return errFailedToAddTimeoutEvent
						}
						updateHistory = true
					}

				case workflow.TimeoutTypeStartToClose:
					{
						t.metricsClient.IncCounter(metrics.TimerTaskActivityTimeoutScope, metrics.StartToCloseTimeoutCounter)
						if ai.StartedID != common.EmptyEventID {
							if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
								return errFailedToAddTimeoutEvent
							}
							updateHistory = true
						}
					}

				case workflow.TimeoutTypeHeartbeat:
					{
						t.metricsClient.IncCounter(metrics.TimerTaskActivityTimeoutScope, metrics.HeartbeatTimeoutCounter)
						if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, ai.Details) == nil {
							return errFailedToAddTimeoutEvent
						}
						updateHistory = true
					}

				case workflow.TimeoutTypeScheduleToStart:
					{
						t.metricsClient.IncCounter(metrics.TimerTaskActivityTimeoutScope, metrics.ScheduleToStartTimeoutCounter)
						if ai.StartedID == common.EmptyEventID {
							if msBuilder.AddActivityTaskTimedOutEvent(ai.ScheduleID, ai.StartedID, timeoutType, nil) == nil {
								return errFailedToAddTimeoutEvent
							}
							updateHistory = true
						}
					}
				}
			} else {
				// See if we have next timer in list to be created.
				// Create next timer task if we don't have one
				if !td.TaskCreated {
					nextTask := tBuilder.createNewTask(td)
					timerTasks = append(timerTasks, nextTask)
					at := nextTask.(*persistence.ActivityTimeoutTask)

					ai.TimerTaskStatus = ai.TimerTaskStatus | getActivityTimerStatus(workflow.TimeoutType(at.TimeoutType))
					msBuilder.UpdateActivity(ai)
					createNewTimer = true

					t.logger.Debugf("%s: Adding Activity Timeout: with timeout: %v sec, ExpiryTime: %s, TimeoutType: %v, EventID: %v",
						time.Now(), td.TimeoutSec, at.VisibilityTimestamp, td.TimeoutType.String(), at.EventID)
				}

				// Done!
				break ExpireActivityTimers
			}
		}

		if updateHistory || createNewTimer {
			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
			// the history and try the operation again.
			scheduleNewDecision := updateHistory && !msBuilder.HasPendingDecisionTask()
			err := t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, timerTasks, nil)
			if err != nil {
				if err == ErrConflict {
					continue Update_History_Loop
				}
			}

			t.notifyNewTimers(timerTasks)
			return nil
		}

		return nil
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processDecisionTimeout(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerTaskDecisionTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerTaskDecisionTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}
		if !msBuilder.isWorkflowExecutionRunning() {
			return nil
		}

		scheduleID := task.EventID
		di, found := msBuilder.GetPendingDecision(scheduleID)

		// First check to see if cache needs to be refreshed as we could potentially have stale workflow execution in
		// some extreme cassandra failure cases.
		if !found && scheduleID >= msBuilder.GetNextEventID() {
			t.metricsClient.IncCounter(metrics.TimerQueueProcessorScope, metrics.StaleMutableStateCounter)
			// Reload workflow execution history
			context.clear()
			continue Update_History_Loop
		}
		if !found {
			logging.LogDuplicateTransferTaskEvent(t.logger, persistence.TaskTypeDecisionTimeout, task.TaskID, scheduleID)
			return nil
		}
		ok, err := verifyTimerTaskVersion(t.shard, task.DomainID, di.Version, task)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		scheduleNewDecision := false
		switch task.TimeoutType {
		case int(workflow.TimeoutTypeStartToClose):
			t.metricsClient.IncCounter(metrics.TimerTaskDecisionTimeoutScope, metrics.StartToCloseTimeoutCounter)
			if di.Attempt == task.ScheduleAttempt {
				// Add a decision task timeout event.
				msBuilder.AddDecisionTaskTimedOutEvent(scheduleID, di.StartedID)
				scheduleNewDecision = true
			}
		case int(workflow.TimeoutTypeScheduleToStart):
			t.metricsClient.IncCounter(metrics.TimerTaskDecisionTimeoutScope, metrics.ScheduleToStartTimeoutCounter)
			// decision schedule to start timeout only apply to sticky decision
			// check if scheduled decision still pending and not started yet
			if di.Attempt == task.ScheduleAttempt && di.StartedID == common.EmptyEventID && msBuilder.isStickyTaskListEnabled() {
				timeoutEvent := msBuilder.AddDecisionTaskScheduleToStartTimeoutEvent(scheduleID)
				if timeoutEvent == nil {
					// Unable to add DecisionTaskTimedout event to history
					return &workflow.InternalServiceError{Message: "Unable to add DecisionTaskScheduleToStartTimeout event to history."}
				}

				// reschedule decision, which will be on its original task list
				scheduleNewDecision = true
			}
		}

		if scheduleNewDecision {
			// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
			// the history and try the operation again.
			err := t.updateWorkflowExecution(context, msBuilder, scheduleNewDecision, false, nil, nil)
			if err != nil {
				if err == ErrConflict {
					continue Update_History_Loop
				}
			}
			return err
		}

		return nil

	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processRetryTimer(task *persistence.TimerTaskInfo) error {
	t.metricsClient.IncCounter(metrics.TimerTaskRetryTimerScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerTaskRetryTimerScope, metrics.TaskLatency)
	defer sw.Stop()

	processFn := func() error {
		context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
		defer release(nil)
		if err0 != nil {
			return err0
		}
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			if _, ok := err1.(*workflow.EntityNotExistsError); ok {
				// this could happen if this is a duplicate processing of the task, and the execution has already completed.
				return nil
			}
			return err1
		}

		if !msBuilder.isWorkflowExecutionRunning() {
			return nil
		}

		// generate activity task
		scheduledID := task.EventID
		ai, running := msBuilder.GetActivityInfo(scheduledID)
		if !running || task.ScheduleAttempt < int64(ai.Attempt) {
			return nil
		}
		ok, err := verifyTimerTaskVersion(t.shard, task.DomainID, ai.Version, task)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		domainID := task.DomainID
		targetDomainID := domainID
		scheduledEvent, _ := msBuilder.GetActivityScheduledEvent(scheduledID)
		if scheduledEvent.ActivityTaskScheduledEventAttributes.Domain != nil {
			domainEntry, err := t.shard.GetDomainCache().GetDomain(scheduledEvent.ActivityTaskScheduledEventAttributes.GetDomain())
			if err != nil {
				return &workflow.InternalServiceError{Message: "Unable to re-schedule activity across domain."}
			}
			targetDomainID = domainEntry.GetInfo().ID
		}

		execution := workflow.WorkflowExecution{
			WorkflowId: common.StringPtr(task.WorkflowID),
			RunId:      common.StringPtr(task.RunID)}
		taskList := &workflow.TaskList{
			Name: &ai.TaskList,
		}
		scheduleToStartTimeout := ai.ScheduleToStartTimeout

		release(nil) // release earlier as we don't need the lock anymore
		err = t.matchingClient.AddActivityTask(nil, &m.AddActivityTaskRequest{
			DomainUUID:                    common.StringPtr(targetDomainID),
			SourceDomainUUID:              common.StringPtr(domainID),
			Execution:                     &execution,
			TaskList:                      taskList,
			ScheduleId:                    &scheduledID,
			ScheduleToStartTimeoutSeconds: common.Int32Ptr(scheduleToStartTimeout),
		})

		t.logger.Debugf("Adding ActivityTask for retry, WorkflowID: %v, RunID: %v, ScheduledID: %v, TaskList: %v, Attempt: %v, Err: %v",
			task.WorkflowID, task.RunID, scheduledID, taskList.GetName(), task.ScheduleAttempt, err)

		return err
	}

	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		if err := processFn(); err == nil {
			return nil
		}
	}

	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) processWorkflowTimeout(task *persistence.TimerTaskInfo) (retError error) {
	t.metricsClient.IncCounter(metrics.TimerTaskWorkflowTimeoutScope, metrics.TaskRequests)
	sw := t.metricsClient.StartTimer(metrics.TimerTaskWorkflowTimeoutScope, metrics.TaskLatency)
	defer sw.Stop()

	context, release, err0 := t.cache.getOrCreateWorkflowExecution(t.timerQueueProcessorBase.getDomainIDAndWorkflowExecution(task))
	if err0 != nil {
		return err0
	}
	defer func() { release(retError) }()

Update_History_Loop:
	for attempt := 0; attempt < conditionalRetryCount; attempt++ {
		msBuilder, err1 := context.loadWorkflowExecution()
		if err1 != nil {
			return err1
		}

		if !msBuilder.isWorkflowExecutionRunning() {
			return nil
		}

		ok, err := verifyTimerTaskVersion(t.shard, task.DomainID, msBuilder.GetStartVersion(), task)
		if err != nil {
			return err
		} else if !ok {
			return nil
		}

		if e := msBuilder.AddTimeoutWorkflowEvent(); e == nil {
			// If we failed to add the event that means the workflow is already completed.
			// we drop this timeout event.
			return nil
		}

		// We apply the update to execution using optimistic concurrency.  If it fails due to a conflict than reload
		// the history and try the operation again.
		err = t.updateWorkflowExecution(context, msBuilder, false, true, nil, nil)
		if err != nil {
			if err == ErrConflict {
				continue Update_History_Loop
			}
		}
		return err
	}
	return ErrMaxAttemptsExceeded
}

func (t *timerQueueActiveProcessorImpl) updateWorkflowExecution(
	context *workflowExecutionContext,
	msBuilder *mutableStateBuilder,
	scheduleNewDecision bool,
	createDeletionTask bool,
	timerTasks []persistence.Task,
	clearTimerTask persistence.Task,
) error {
	var transferTasks []persistence.Task
	if scheduleNewDecision {
		// Schedule a new decision.
		di := msBuilder.AddDecisionTaskScheduledEvent()
		transferTasks = []persistence.Task{&persistence.DecisionTask{
			DomainID:   msBuilder.executionInfo.DomainID,
			TaskList:   di.TaskList,
			ScheduleID: di.ScheduleID,
		}}
		if msBuilder.isStickyTaskListEnabled() {
			tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)
			stickyTaskTimeoutTimer := tBuilder.AddScheduleToStartDecisionTimoutTask(di.ScheduleID, di.Attempt,
				msBuilder.executionInfo.StickyScheduleToStartTimeout)
			timerTasks = append(timerTasks, stickyTaskTimeoutTimer)
		}
	}

	if createDeletionTask {
		tBuilder := t.historyService.getTimerBuilder(&context.workflowExecution)
		tranT, timerT, err := t.historyService.getDeleteWorkflowTasks(msBuilder.executionInfo.DomainID, tBuilder)
		if err != nil {
			return nil
		}
		transferTasks = append(transferTasks, tranT)
		timerTasks = append(timerTasks, timerT)
	}

	// Generate a transaction ID for appending events to history
	transactionID, err1 := t.historyService.shard.GetNextTransferTaskID()
	if err1 != nil {
		return err1
	}

	err := context.updateWorkflowExecutionWithDeleteTask(transferTasks, timerTasks, clearTimerTask, transactionID)
	if err != nil {
		if isShardOwnershiptLostError(err) {
			// Shard is stolen.  Stop timer processing to reduce duplicates
			t.timerQueueProcessorBase.Stop()
		}
	}
	t.notifyNewTimers(timerTasks)
	return err
}
