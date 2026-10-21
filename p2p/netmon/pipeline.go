package netmon

import (
	"context"
	"errors"
	"sync"
	"time"
)

type Stage uint8

const (
	Rebind Stage = iota
	ConfirmAddresses
	RefreshMappings
	RenewRelay
	IdentifyPush
	RestartProbing
	Complete
)

type Hook func(context.Context, Event) error

// Reaction is emitted after each stage, including unregistered stages; Complete
// carries the joined errors. Subscribers must not infer success from silence.
type Reaction struct {
	Event Event
	Stage Stage
	Err   error
}
type registered struct {
	id   uint64
	hook Hook
}

// Pipeline runs D12 stages serially; failures do not suppress later recovery
// steps. Hooks must honor their contexts. Run one loop per Pipeline. Registrations
// changed during a reaction take effect on the next reaction.
type Pipeline struct {
	timeout time.Duration
	mu      sync.Mutex
	next    uint64
	hooks   [Complete][]registered
	subs    map[uint64]chan Reaction
}

func NewPipeline(stepTimeout time.Duration) (*Pipeline, error) {
	if stepTimeout <= 0 {
		return nil, errors.New("netmon: positive hook timeout required")
	}
	return &Pipeline{timeout: stepTimeout, subs: make(map[uint64]chan Reaction)}, nil
}
func (p *Pipeline) Register(stage Stage, hook Hook) (func(), error) {
	if stage >= Complete || hook == nil {
		return nil, errors.New("netmon: invalid hook")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := p.next
	p.hooks[stage] = append(p.hooks[stage], registered{id, hook})
	return func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		for i, h := range p.hooks[stage] {
			if h.id == id {
				p.hooks[stage] = append(p.hooks[stage][:i], p.hooks[stage][i+1:]...)
				break
			}
		}
	}, nil
}

// Subscribe returns a bounded latest-event queue. On overflow the oldest reaction
// is dropped; Complete includes the full outcome of that reaction. Unsubscribe
// closes the queue. The owner should unsubscribe after Run terminates.
func (p *Pipeline) Subscribe() (<-chan Reaction, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.next++
	id := p.next
	ch := make(chan Reaction, 32)
	p.subs[id] = ch
	return ch, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if _, ok := p.subs[id]; ok {
			delete(p.subs, id)
			close(ch)
		}
	}
}
func (p *Pipeline) emit(r Reaction) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, ch := range p.subs {
		select {
		case ch <- r:
		default:
			select {
			case <-ch:
			default:
			}
			ch <- r
		}
	}
}
func (p *Pipeline) Run(ctx context.Context, events <-chan Event) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case e, ok := <-events:
			if !ok {
				return nil
			}
			p.mu.Lock()
			var hooks [Complete][]registered
			for stage := Rebind; stage < Complete; stage++ {
				hooks[stage] = append([]registered(nil), p.hooks[stage]...)
			}
			p.mu.Unlock()
			var all []error
			for stage := Rebind; stage < Complete; stage++ {
				var errs []error
				for _, h := range hooks[stage] {
					step, cancel := context.WithTimeout(ctx, p.timeout)
					err := h.hook(step, e)
					if err == nil {
						err = step.Err()
					}
					cancel()
					if err != nil {
						errs = append(errs, err)
					}
				}
				err := errors.Join(errs...)
				if err != nil {
					all = append(all, err)
				}
				p.emit(Reaction{Event: e, Stage: stage, Err: err})
			}
			p.emit(Reaction{Event: e, Stage: Complete, Err: errors.Join(all...)})
		}
	}
}
