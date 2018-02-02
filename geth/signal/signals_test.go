package signal

import (
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNodeCrashEventJSONMarshalling(t *testing.T) {
	errorMsg := "TestNodeCrashEventJSONMarshallingError"
	expectedJSON := fmt.Sprintf(`{"error":"%s"}`, errorMsg)
	nodeCrashEvent := &NodeCrashEvent{
		Error: errors.New(errorMsg),
	}
	marshalled, err := json.Marshal(nodeCrashEvent)
	require.NoError(t, err)
	require.Equal(t, expectedJSON, string(marshalled))
}

func TestEnvelopeJSONMarshalling(t *testing.T) {
	errorMsg := "TestEnvelopeJSONMarshallingError"
	expectedJSON := fmt.Sprintf(`{"event":{"error":"%s"},"type":"node.crashed"}`, errorMsg)
	envelope := &Envelope{
		Type: EventNodeCrashed,
		Event: NodeCrashEvent{
			Error: errors.New(errorMsg),
		},
	}
	marshalled, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.Equal(t, expectedJSON, string(marshalled))
}

func TestEnvelopeJSONMarshallingWithoutNodeCrashEvent(t *testing.T) {
	expectedJSON := `{"type":"node.started","event":{"otherField":"OtherValue"}}`
	envelope := &Envelope{
		Type: EventNodeStarted,
		Event: &struct {
			OtherField string `json:"otherField"`
		}{
			OtherField: "OtherValue",
		},
	}
	marshalled, err := json.Marshal(envelope)
	require.NoError(t, err)
	require.Equal(t, expectedJSON, string(marshalled))
}
