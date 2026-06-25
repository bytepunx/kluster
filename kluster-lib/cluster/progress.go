package cluster

import "time"

// EventKind describes the lifecycle stage of a ProgressEvent.
type EventKind int

const (
	EventStarted  EventKind = iota // a step is beginning
	EventProgress                  // label update while step is still running
	EventDone                      // step completed successfully
	EventFailed                    // step failed
)

// EventPhase groups steps so renderers can insert visual separators between phases.
type EventPhase int

const (
	PhaseSetup   EventPhase = iota // chart version resolution + cluster creation
	PhaseAddon                     // helm install + readiness wait per addon
	PhaseProfile                   // profile.Configure calls
)

// ProgressEvent is emitted during Cluster.Up() at each step boundary.
// Calls within a single Up() are sequential and from a single goroutine.
type ProgressEvent struct {
	Kind    EventKind
	Phase   EventPhase
	Name    string        // short name for done/failed lines: "cert-manager"
	Label   string        // in-progress text: "cert-manager: installing"
	Elapsed time.Duration // set on EventDone and EventFailed
	Err     error         // set on EventFailed
}

// ProgressFunc receives events emitted by Cluster.Up().
type ProgressFunc func(ProgressEvent)
