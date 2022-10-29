package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/microsoft/durabletask-go/api"
	"github.com/microsoft/durabletask-go/internal/helpers"
	"github.com/microsoft/durabletask-go/internal/protos"
)

type OrchestratorExecutor interface {
	ExecuteOrchestrator(
		ctx context.Context,
		iid api.InstanceID,
		oldEvents []*protos.HistoryEvent,
		newEvents []*protos.HistoryEvent) (*ExecutionResults, error)
}

type orchestratorProcessor struct {
	be       Backend
	executor OrchestratorExecutor
	logger   Logger
}

func NewOrchestrationWorker(be Backend, executor OrchestratorExecutor, logger Logger, opts ...NewTaskWorkerOptions) TaskWorker {
	processor := &orchestratorProcessor{
		be:       be,
		executor: executor,
		logger:   logger,
	}
	return NewTaskWorker(be, processor, logger, opts...)
}

// Name implements TaskProcessor
func (*orchestratorProcessor) Name() string {
	return "orchestration-processor"
}

// FetchWorkItem implements TaskProcessor
func (p *orchestratorProcessor) FetchWorkItem(ctx context.Context) (WorkItem, error) {
	return p.be.GetOrchestrationWorkItem(ctx)
}

// ProcessWorkItem implements TaskProcessor
func (w *orchestratorProcessor) ProcessWorkItem(ctx context.Context, cwi WorkItem) error {
	wi := cwi.(*OrchestrationWorkItem)
	w.logger.Debugf("%v: received work item with %d new event(s): %v", wi.InstanceID, len(wi.NewEvents), helpers.HistoryListSummary(wi.NewEvents))

	// TODO: Caching
	// In the fullness of time, we should consider caching executors and runtime state
	// so that we can skip the loading of state and/or the creation of executors. A cached
	// executor should allow us to 1) skip runtime state loading and 2) execute only new events.
	if wi.State == nil {
		if state, err := w.be.GetOrchestrationRuntimeState(ctx, wi); err != nil {
			return fmt.Errorf("failed to load orchestration state: %w", err)
		} else {
			wi.State = state
		}
	}
	w.logger.Debugf("%v: got orchestration runtime state: %s", wi.InstanceID, getOrchestrationStateDescription(wi))

	if w.applyWorkItem(wi) {
		for continueAsNewCount := 0; ; continueAsNewCount++ {
			if continueAsNewCount > 0 {
				w.logger.Debugf("%v: continuing-as-new with %d event(s): %s", wi.InstanceID, len(wi.State.NewEvents()), helpers.HistoryListSummary(wi.State.NewEvents()))
			} else {
				w.logger.Debugf("%v: invoking orchestrator", wi.InstanceID)
			}

			// Run the user orchestrator code, providing the old history and new events together.
			results, err := w.executor.ExecuteOrchestrator(ctx, wi.InstanceID, wi.State.OldEvents(), wi.State.NewEvents())
			if err != nil {
				return fmt.Errorf("error executing orchestrator: %w", err)
			}
			w.logger.Debugf("%v: orchestrator returned %d action(s): %s", wi.InstanceID, len(results.Response.Actions), helpers.ActionListSummary(results.Response.Actions))

			// Apply the orchestrator outputs to the orchestration state.
			continuedAsNew, err := wi.State.ApplyActions(results.Response.Actions)
			if err != nil {
				return fmt.Errorf("failed to apply the execution result actions: %w", err)
			}
			wi.State.CustomStatus = results.Response.CustomStatus

			// When continuing-as-new, we re-execute the orchestrator from the beginning with a truncated state in a tight loop
			// until the orchestrator performs some non-continue-as-new action.
			if continuedAsNew {
				w.logger.Debugf("%v: continued-as-new with %d new event(s).", wi.InstanceID, len(wi.State.NewEvents()))

				const MaxContinueAsNewCount = 20
				if continueAsNewCount >= MaxContinueAsNewCount {
					return fmt.Errorf("exceeded tight-loop continue-as-new limit of %d iterations", MaxContinueAsNewCount)
				}
				continue
			}

			if wi.State.IsCompleted() {
				name, _ := wi.State.Name()
				w.logger.Infof("%v: '%s' completed with a %s status.", wi.InstanceID, name, helpers.ToRuntimeStatusString(wi.State.RuntimeStatus()))
			}
			break
		}
	}
	return nil
}

// CompleteWorkItem implements TaskProcessor
func (p *orchestratorProcessor) CompleteWorkItem(ctx context.Context, wi WorkItem) error {
	owi := wi.(*OrchestrationWorkItem)
	return p.be.CompleteOrchestrationWorkItem(ctx, owi)
}

// AbandonWorkItem implements TaskProcessor
func (p *orchestratorProcessor) AbandonWorkItem(ctx context.Context, wi WorkItem) error {
	owi := wi.(*OrchestrationWorkItem)
	return p.be.AbandonOrchestrationWorkItem(ctx, owi)
}

func (w *orchestratorProcessor) applyWorkItem(wi *OrchestrationWorkItem) bool {
	// Ignore work items for orchestrations that are completed or are in a corrupted state.
	if !wi.State.IsValid() {
		w.logger.Warnf("%v: orchestration state is invalid; dropping work item", wi.InstanceID)
		return false
	} else if wi.State.IsCompleted() {
		w.logger.Warnf("%v: orchestration already completed; dropping work item", wi.InstanceID)
		return false
	} else if len(wi.NewEvents) == 0 {
		w.logger.Warnf("%v: the work item had no events!", wi.InstanceID)
	}

	// The orchestrator started event is used primarily for updating the current time as reported
	// by the orchestration context APIs.
	wi.State.AddEvent(helpers.NewOrchestratorStartedEvent())

	// New events from the work item are appended to the orchestration state, with duplicates automatically
	// filtered out. If all events are filtered out, return false so that the caller knows not to execute
	// the orchestration logic for an empty set of events.
	added := 0
	for _, e := range wi.NewEvents {
		if err := wi.State.AddEvent(e); err != nil {
			if err == ErrDuplicateEvent {
				w.logger.Warnf("%v: dropping duplicate event: %v", wi.InstanceID, e)
			} else {
				w.logger.Warnf("%v: dropping event: %v, %v", wi.InstanceID, e, err)
			}
		} else {
			added++
		}

		if es := e.GetExecutionStarted(); es != nil {
			w.logger.Infof("%v: starting new '%s' instance.", wi.InstanceID, es.Name)
		}
	}

	if added == 0 {
		w.logger.Warnf("%v: all new events were dropped", wi.InstanceID)
		return false
	}

	return true
}

func (w *orchestratorProcessor) abortWorkItem(ctx context.Context, wi *OrchestrationWorkItem, err error, message string) {
	w.logger.Warnf("aborting work item: %v: %v: %v", wi, message, err)
	err = w.be.AbandonOrchestrationWorkItem(ctx, wi)
	if err != nil {
		w.logger.Errorf("failed to abort work item: %v", wi)
		return
	}
}

func getOrchestrationStateDescription(wi *OrchestrationWorkItem) string {
	name, err := wi.State.Name()
	if err != nil {
		if len(wi.NewEvents) > 0 {
			name = wi.NewEvents[0].GetExecutionStarted().GetName()
		}
	}
	if name == "" {
		name = "(unknown)"
	}

	ageStr := "(new)"
	createdAt, err := wi.State.CreatedTime()
	if err == nil {
		age := time.Now().Sub(createdAt)
		if age > 0 {
			ageStr = age.Round(time.Second).String()
		}
	}
	status := helpers.ToRuntimeStatusString(wi.State.RuntimeStatus())
	return fmt.Sprintf("name=%s, status=%s, events=%d, age=%s", name, status, len(wi.State.OldEvents()), ageStr)
}
