package cloudevent

import (
	"context"
	"errors"
	"fmt"
	"k8s.io/apimachinery/pkg/util/json"
	"time"

	cdeevents "github.com/cdfoundation/sig-events/cde/sdk/go/pkg/cdf/events"
	cloudevents "github.com/cloudevents/sdk-go/v2"
	"github.com/tektoncd/pipeline/pkg/apis/pipeline/v1beta1"
	"github.com/tektoncd/pipeline/pkg/reconciler/events/cloudevent"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"knative.dev/pkg/apis"
	"knative.dev/pkg/controller"
	"knative.dev/pkg/logging"
)

// TODO(afrittoli) The valid statuses should be encoded in the SDK
// EvenStatus encodes valid statuses defined in https://github.com/cdfoundation/sig-events/blob/main/vocabulary-draft/core.md#continuous-delivery-core-events
type EventStatus string

const (
	StatusRunning  EventStatus = "Running"
	StatusFinished EventStatus = "Finished"
	StatusError    EventStatus = "Error"
)

// CEClient matches the `Client` interface from github.com/cloudevents/sdk-go/v2/cloudevents
type CEClient cloudevents.Client

type CDECloudEventData map[string]string

// getEventData returns a new instance of CDECloudEventData
func getEventData(runObject objectWithCondition) (CDECloudEventData, error) {
	cdeCloudEventData := map[string]string{}
	switch v := runObject.(type) {
	case *v1beta1.TaskRun:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		cdeCloudEventData["taskrun"] = string(data)
	case *v1beta1.PipelineRun:
		data, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		cdeCloudEventData["pipelinerun"] = string(data)
	}
	return cdeCloudEventData, nil
}

type EventType struct {
	Type   cdeevents.CDEventType
	Status EventStatus
}

// getEventType returns the event type and status
func getEventType(runObject objectWithCondition) (*EventType, error) {
	c := runObject.GetStatusCondition().GetCondition(apis.ConditionSucceeded)
	if c == nil {
		return nil, fmt.Errorf("no condition for ConditionSucceeded in %T", runObject)
	}
	eventType := EventType{}
	switch {
	case c.IsUnknown():
		eventType.Status = StatusRunning
		switch runObject.(type) {
		case *v1beta1.TaskRun:
			switch c.Reason {
			case v1beta1.TaskRunReasonStarted.String():
				eventType.Type = cdeevents.TaskRunStartedEventV1
			case v1beta1.TaskRunReasonRunning.String():
				eventType.Type = cdeevents.TaskRunStartedEventV1
			// Unknown status, unknown reason -> no event type
			default:
				return nil, fmt.Errorf("unknown status with unknown reason %s", c.Reason)
			}
		case *v1beta1.PipelineRun:
			switch c.Reason {
			case v1beta1.PipelineRunReasonStarted.String():
				eventType.Type = cdeevents.PipelineRunQueuedEventV1
			case v1beta1.PipelineRunReasonRunning.String():
				eventType.Type = cdeevents.PipelineRunStartedEventV1
			// Unknown status, unknown reason -> no event type
			default:
				return nil, fmt.Errorf("unknown status with unknown reason %s", c.Reason)
			}
		}
	case c.IsTrue():
		eventType.Status = StatusFinished
		switch runObject.(type) {
		case *v1beta1.TaskRun:
			eventType.Type = cdeevents.TaskRunFinishedEventV1 //TaskRunFailedEventV1
		case *v1beta1.PipelineRun:
			eventType.Type = cdeevents.PipelineRunFinishedEventV1 //PipelineRunFailedEventV1
		}
	case c.IsFalse():
		eventType.Status = StatusError
		switch runObject.(type) {
		case *v1beta1.TaskRun:
			eventType.Type = cdeevents.TaskRunFinishedEventV1 //TaskRunFailedEventV1
		case *v1beta1.PipelineRun:
			eventType.Type = cdeevents.PipelineRunFinishedEventV1 //PipelineRunFailedEventV1
		}
	default:
		return nil, fmt.Errorf("unknown condition for in %T.Status %s", runObject, c.Status)
	}
	return &eventType, nil
}

// eventForObjectWithCondition creates a new event based for a objectWithCondition,
// or return an error if not possible.
func eventForObjectWithCondition(runObject objectWithCondition) (*cloudevents.Event, error) {
	var event cloudevents.Event
	var err error
	meta := runObject.GetObjectMeta()
	data, err := getEventData(runObject)
	if err != nil {
		return nil, err
	}
	etype, err := getEventType(runObject)
	if err != nil {
		return nil, err
	}
	switch runObject.(type) {
	case *v1beta1.TaskRun:
		event, err = cdeevents.CreateTaskRunEvent(etype.Type, string(meta.GetUID()), meta.GetName(), "", data)
		if err != nil {
			return nil, err
		}
	case *v1beta1.PipelineRun:
		event, err = cdeevents.CreatePipelineRunEvent(etype.Type, string(meta.GetUID()), meta.GetName(), string(etype.Status), "", "", data)
		if err != nil {
			return nil, err
		}
	}
	event.SetSubject(runObject.GetObjectMeta().GetName())
	source := runObject.GetObjectMeta().GetSelfLink()
	if source == "" {
		gvk := runObject.GetObjectKind().GroupVersionKind()
		source = fmt.Sprintf("/apis/%s/%s/namespaces/%s/%s/%s",
			gvk.Group,
			gvk.Version,
			meta.GetNamespace(),
			gvk.Kind,
			meta.GetName())
	}
	event.SetSource(source)

	return &event, nil
}

// SendCloudEventWithRetries sends a cloud event for the specified resource.
// It does not block and it perform retries with backoff using the cloudevents
// sdk-go capabilities.
// It accepts a runtime.Object to avoid making objectWithCondition public since
// it's only used within the events/cloudevents packages.
func SendCloudEventWithRetries(ctx context.Context, object runtime.Object) error {
	var (
		o  objectWithCondition
		ok bool
	)
	if o, ok = object.(objectWithCondition); !ok {
		return errors.New("input object does not satisfy objectWithCondition")
	}
	logger := logging.FromContext(ctx)
	ceClient := cloudevent.Get(ctx)
	if ceClient == nil {
		return errors.New("no cloud events client found in the context")
	}
	event, err := eventForObjectWithCondition(o)
	if err != nil {
		return err
	}

	wasIn := make(chan error)
	go func() {
		wasIn <- nil
		logger.Debugf("Sending cloudevent of type %q", event.Type())
		if result := ceClient.Send(cloudevents.ContextWithRetriesExponentialBackoff(ctx, 10*time.Millisecond, 10), *event); !cloudevents.IsACK(result) {
			logger.Warnf("Failed to send cloudevent: %s", result.Error())
			recorder := controller.GetEventRecorder(ctx)
			if recorder == nil {
				logger.Warnf("No recorder in context, cannot emit error event")
			} else {
				recorder.Event(object, corev1.EventTypeWarning, "Cloud Event Failure", result.Error())
			}
		}
	}()

	return <-wasIn
}