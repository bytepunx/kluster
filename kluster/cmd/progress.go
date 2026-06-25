package cmd

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/bytepunx/kluster-lib/cluster"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// renderer displays Cluster.Up() progress events on a terminal or plain writer.
// On a TTY it animates the current step in-place and uses colour. On a plain
// writer (pipe, CI) it prints a line per event, no ANSI codes.
type renderer struct {
	out io.Writer
	tty bool

	mu    sync.Mutex
	label string // in-progress label shown by the animation goroutine
	frame int    // current spinner frame index

	stopCh chan struct{}
	animWG sync.WaitGroup

	totalStart time.Time
	lastPhase  cluster.EventPhase
	firstPhase bool // true until the first event is received
}

func newRenderer(out io.Writer) *renderer {
	return &renderer{
		out:        out,
		tty:        isTTY(out),
		totalStart: time.Now(),
		firstPhase: true,
	}
}

func isTTY(w io.Writer) bool {
	f, ok := w.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

// Handle is passed as the ProgressFunc to Cluster.Up().
func (r *renderer) Handle(e cluster.ProgressEvent) {
	switch e.Kind {
	case cluster.EventStarted:
		r.onStarted(e)
	case cluster.EventProgress:
		r.onProgress(e)
	case cluster.EventDone:
		r.onDone(e)
	case cluster.EventFailed:
		r.onFailed(e)
	}
}

// Done prints the final "ready in X" summary line.
func (r *renderer) Done(clusterName string) {
	r.stopAnim()
	elapsed := formatDuration(time.Since(r.totalStart))
	if r.tty {
		fmt.Fprintf(r.out, "\n\033[1;32m✓\033[0m cluster \033[1m%q\033[0m ready in %s\n", clusterName, elapsed)
	} else {
		fmt.Fprintf(r.out, "\nCluster %q ready in %s\n", clusterName, elapsed)
	}
}

func (r *renderer) onStarted(e cluster.ProgressEvent) {
	r.stopAnim() // stop any previous step's animation first

	r.mu.Lock()
	// Blank line between phase groups, but not before the very first event.
	if !r.firstPhase && e.Phase != r.lastPhase {
		fmt.Fprintln(r.out)
	}
	r.firstPhase = false
	r.lastPhase = e.Phase
	r.label = e.Label
	if r.tty {
		fmt.Fprintf(r.out, "%s %s", spinnerFrames[r.frame], e.Label)
	} else {
		fmt.Fprintf(r.out, "  %s...\n", e.Label)
	}
	r.mu.Unlock()

	if !r.tty {
		return
	}
	r.stopCh = make(chan struct{})
	r.animWG.Add(1)
	go func() {
		defer r.animWG.Done()
		tick := time.NewTicker(80 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-r.stopCh:
				return
			case <-tick.C:
				r.mu.Lock()
				r.frame = (r.frame + 1) % len(spinnerFrames)
				if r.label != "" {
					fmt.Fprintf(r.out, "\r\033[K%s %s", spinnerFrames[r.frame], r.label)
				}
				r.mu.Unlock()
			}
		}
	}()
}

func (r *renderer) onProgress(e cluster.ProgressEvent) {
	r.mu.Lock()
	r.label = e.Label
	r.mu.Unlock()
	if !r.tty {
		fmt.Fprintf(r.out, "  %s...\n", e.Label)
	}
}

func (r *renderer) onDone(e cluster.ProgressEvent) {
	r.stopAnim()
	if r.tty {
		fmt.Fprintf(r.out, "\r\033[K\033[32m✓\033[0m %s (%s)\n", e.Name, formatDuration(e.Elapsed))
	} else {
		fmt.Fprintf(r.out, "  ✓ %s (%s)\n", e.Name, formatDuration(e.Elapsed))
	}
}

func (r *renderer) onFailed(e cluster.ProgressEvent) {
	r.stopAnim()
	// Print the step name only; the full error is returned by Up() and printed by Cobra.
	if r.tty {
		fmt.Fprintf(r.out, "\r\033[K\033[31m✗\033[0m %s\n", e.Name)
	} else {
		fmt.Fprintf(r.out, "  ✗ %s\n", e.Name)
	}
}

func (r *renderer) stopAnim() {
	if r.stopCh == nil {
		return
	}
	close(r.stopCh)
	r.animWG.Wait()
	r.stopCh = nil
	r.mu.Lock()
	r.label = ""
	r.mu.Unlock()
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Second {
		return "<1s"
	}
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%02ds", m, s)
}
