package agentTelemetry

import (
	"io"
)

// Close closes things and logs errors if it fails
func Close(thing io.Closer) {
	err := thing.Close()
	if err != nil {
		l.Errorf("[agentTelemetry]: %v", err)
	}
}
