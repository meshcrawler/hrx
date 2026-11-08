package coveragecontroller

import (
	"errors"
	"fmt"
	"testing"
)

func TestRuntimeStoppedCauseOnlyExposesTerminatedReport(t *testing.T) {
	for _, cause := range []error{nil, fmt.Errorf("bootstrap: %w", errThreadBoundary)} {
		t.Run(fmt.Sprint(cause), func(t *testing.T) {
			terminated := make(chan struct{})
			runtime := &Runtime{gateway: &gateway{done: terminated}}
			if observed, stopped := runtime.StoppedCause(); observed != nil || stopped {
				t.Fatal("live gateway exposed a termination report", observed, stopped)
			}
			// Model the report writer's publication order. Closing the gateway
			// signal, rather than sampling its fields, makes the report readable.
			go func() {
				runtime.gateway.report = serveReport{Cause: cause}
				close(terminated)
			}()
			<-terminated
			observed, stopped := runtime.StoppedCause()
			if !stopped || observed != cause {
				t.Fatal("terminated report lost its original cause", observed, stopped)
			}
			if cause != nil && !errors.Is(observed, errThreadBoundary) {
				t.Fatal("termination cause lost its bootstrap classification", observed)
			}
		})
	}
}
