package backend

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/wrapperspb"

	"github.com/microsoft/durabletask-go/api"
	"github.com/microsoft/durabletask-go/internal/helpers"
	"github.com/microsoft/durabletask-go/internal/protos"
)

type TaskHubClient interface {
	ScheduleNewOrchestration(ctx context.Context, orchestrator interface{}, opts ...api.NewOrchestrationOptions) (api.InstanceID, error)
	FetchOrchestrationMetadata(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error)
	WaitForOrchestrationStart(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error)
	WaitForOrchestrationCompletion(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error)
	TerminateOrchestration(ctx context.Context, id api.InstanceID, reason string) error
}

type backendClient struct {
	be Backend
}

func NewTaskHubClient(be Backend) TaskHubClient {
	return &backendClient{
		be: be,
	}
}

func (c *backendClient) ScheduleNewOrchestration(ctx context.Context, orchestrator interface{}, opts ...api.NewOrchestrationOptions) (api.InstanceID, error) {
	name := helpers.GetTaskFunctionName(orchestrator)
	req := &protos.CreateInstanceRequest{Name: name}
	for _, configure := range opts {
		configure(req)
	}
	if req.InstanceId == "" {
		req.InstanceId = uuid.NewString()
	}
	e := helpers.NewExecutionStartedEvent(-1, req.Name, req.InstanceId, req.Input, nil)
	if err := c.be.CreateOrchestrationInstance(ctx, e); err != nil {
		return api.EmptyInstanceID, fmt.Errorf("failed to start orchestration: %w", err)
	}
	return api.InstanceID(req.InstanceId), nil
}

// FetchOrchestrationMetadata fetches metadata for the specified orchestration from the configured task hub.
//
// ErrInstanceNotFound is returned when the specified orchestration doesn't exist.
func (c *backendClient) FetchOrchestrationMetadata(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error) {
	metadata, err := c.be.GetOrchestrationMetadata(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("Failed to fetch orchestration metadata: %w", err)
	}
	return metadata, nil
}

// WaitForOrchestrationStart waits for an orchestration to start running and returns an [OrchestrationMetadata] object that contains
// metadata about the started instance.
//
// ErrInstanceNotFound is returned when the specified orchestration doesn't exist.
func (c *backendClient) WaitForOrchestrationStart(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error) {
	return c.waitForOrchestrationCondition(ctx, id, func(metadata *api.OrchestrationMetadata) bool {
		return metadata.RuntimeStatus != protos.OrchestrationStatus_ORCHESTRATION_STATUS_PENDING
	})
}

// WaitForOrchestrationCompletion waits for an orchestration to complete and returns an [OrchestrationMetadata] object that contains
// metadata about the completed instance.
//
// ErrInstanceNotFound is returned when the specified orchestration doesn't exist.
func (c *backendClient) WaitForOrchestrationCompletion(ctx context.Context, id api.InstanceID) (*api.OrchestrationMetadata, error) {
	return c.waitForOrchestrationCondition(ctx, id, func(metadata *api.OrchestrationMetadata) bool {
		return metadata.IsComplete()
	})
}

func (c *backendClient) waitForOrchestrationCondition(ctx context.Context, id api.InstanceID, condition func(metadata *api.OrchestrationMetadata) bool) (*api.OrchestrationMetadata, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(1 * time.Second):
			metadata, err := c.FetchOrchestrationMetadata(ctx, id)
			if err != nil {
				return nil, err
			}
			if metadata != nil && condition(metadata) {
				return metadata, nil
			}
		}
	}
}

// TerminateOrchestration enqueues a message to terminate a running orchestration, causing it to stop receiving new events and
// go directly into the TERMINATED state. This operation is asynchronous. An orchestration worker must
// dequeue the termination event before the orchestration will be terminated.
func (c *backendClient) TerminateOrchestration(ctx context.Context, id api.InstanceID, reason string) error {
	e := helpers.NewExecutionTerminatedEvent(wrapperspb.String(reason))
	if err := c.be.AddNewOrchestrationEvent(ctx, id, e); err != nil {
		return fmt.Errorf("failed to add terminate event: %w", err)
	}
	return nil
}
