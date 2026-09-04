package handlers

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestRequestCompletionObserverRunsExactlyOnceAlongsidePluginHost(t *testing.T) {
	var observed []pluginapi.RequestCompletion
	var pluginObserved []pluginapi.RequestCompletion
	handler := NewBaseAPIHandlers(nil, nil)
	handler.SetRequestCompletionObserver(func(_ context.Context, completion pluginapi.RequestCompletion) {
		observed = append(observed, completion)
	})
	handler.SetPluginHost(&handlerInterceptorTestHost{
		completeRequest: func(_ context.Context, completion pluginapi.RequestCompletion) {
			pluginObserved = append(pluginObserved, completion)
		},
	})

	tracker := handler.newRequestLifecycleTracker(context.Background(), "openai", "model", "model", false, nil, "")
	tracker.complete(pluginapi.RequestCompletionFailed, http.StatusBadGateway, errors.New("upstream detail"))
	tracker.complete(pluginapi.RequestCompletionSucceeded, http.StatusOK, nil)

	if len(observed) != 1 || len(pluginObserved) != 1 {
		t.Fatalf("completion counts = observer %d, plugin %d", len(observed), len(pluginObserved))
	}
	if observed[0].RequestID == "" || observed[0].RequestID != pluginObserved[0].RequestID || observed[0].Outcome != pluginapi.RequestCompletionFailed {
		t.Fatalf("observer completion = %#v, plugin completion = %#v", observed[0], pluginObserved[0])
	}
}
