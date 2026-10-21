package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/whyrusleeping/nu/api/apiv0"
	"github.com/whyrusleeping/nu/node"
)

// follow streams an operation's events until it finishes and returns the final snapshot.
// In --json mode the events are written to stdout as NDJSON (the last line is the done
// event, which carries the final operation). On a TTY, progress is rendered on one
// updating stderr line; otherwise stage changes are printed as lines. A failed or
// cancelled operation is returned as its error, so the command exits with its category.
func (e *env) follow(ctx context.Context, n node.Node, id string) (*apiv0.Operation, error) {
	st, err := n.OperationEvents(ctx, id, 0)
	if err != nil {
		return nil, err
	}
	defer st.Close()
	r := newRenderer(e)
	var final *apiv0.Operation
	for {
		ev, err := st.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			r.finish()
			return nil, err
		}
		if e.json {
			b, _ := json.Marshal(ev)
			fmt.Fprintln(e.stdout, string(b))
		} else {
			r.event(ev)
		}
		if ev.Type == apiv0.EventDone {
			final = ev.Operation
		}
	}
	r.finish()
	if final == nil {
		return nil, apiv0.Errorf(apiv0.CodeFailed, "the event stream ended without a result")
	}
	if final.State != apiv0.StateSucceeded {
		if final.Error != nil {
			return final, final.Error
		}
		return final, apiv0.Errorf(apiv0.CodeFailed, "operation %s %s", final.ID, final.State)
	}
	return final, nil
}

// renderer draws progress on stderr.
type renderer struct {
	w        io.Writer
	tty      bool
	kind     string
	target   string
	stage    string
	progress apiv0.Progress
	dirty    bool
	last     time.Time
}

func newRenderer(e *env) *renderer {
	r := &renderer{w: e.stderr}
	if f, ok := e.stderr.(*os.File); ok {
		if fi, err := f.Stat(); err == nil && fi.Mode()&os.ModeCharDevice != 0 {
			r.tty = true
		}
	}
	return r
}

func (r *renderer) event(ev *apiv0.Event) {
	switch ev.Type {
	case apiv0.EventStage:
		r.stage = ev.Stage
		if r.tty {
			r.draw(true)
		} else {
			fmt.Fprintf(r.w, "%s\n", ev.Stage)
		}
	case apiv0.EventProgress:
		if ev.Progress != nil {
			r.progress = *ev.Progress
		}
		if r.tty {
			r.draw(false)
		}
	case apiv0.EventLog:
		r.clear()
		fmt.Fprintln(r.w, ev.Message)
	case apiv0.EventDone:
		if ev.Operation != nil {
			r.kind, r.target = ev.Operation.Kind, ev.Operation.Target
		}
		r.clear()
	}
}

func (r *renderer) line() string {
	var b strings.Builder
	if r.stage != "" {
		b.WriteString(r.stage)
	}
	if r.progress.Done > 0 || r.progress.Total > 0 {
		if b.Len() > 0 {
			b.WriteString(" ")
		}
		unit := r.progress.Unit
		if unit == "bytes" {
			unit = "" // humanCount already carries K/M/G
		}
		if r.progress.Total > 0 {
			fmt.Fprintf(&b, "%s/%s %s(%d%%)", humanCount(r.progress.Done, r.progress.Unit), humanCount(r.progress.Total, r.progress.Unit),
				unit, r.progress.Done*100/r.progress.Total)
		} else {
			fmt.Fprintf(&b, "%s %s", humanCount(r.progress.Done, r.progress.Unit), unit)
		}
	}
	return b.String()
}

// draw redraws the status line, rate-limited unless forced.
func (r *renderer) draw(force bool) {
	if !force && time.Since(r.last) < 100*time.Millisecond {
		return
	}
	r.last = time.Now()
	fmt.Fprintf(r.w, "\r\033[K%s", r.line())
	r.dirty = true
}

func (r *renderer) clear() {
	if r.dirty {
		fmt.Fprint(r.w, "\r\033[K")
		r.dirty = false
	}
}

func (r *renderer) finish() { r.clear() }

func humanCount(n int64, unit string) string {
	if unit != "bytes" {
		return fmt.Sprint(n)
	}
	const k = 1024
	switch {
	case n >= k*k*k:
		return fmt.Sprintf("%.1fG", float64(n)/(k*k*k))
	case n >= k*k:
		return fmt.Sprintf("%.1fM", float64(n)/(k*k))
	case n >= k:
		return fmt.Sprintf("%.1fK", float64(n)/k)
	}
	return fmt.Sprint(n)
}
