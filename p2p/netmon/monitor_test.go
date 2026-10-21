package netmon

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestOrderedPipeline(t *testing.T) {
	p, err := NewPipeline(50 * time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	var got []Stage
	failure := errors.New("AutoNAT unavailable")
	for stage := Rebind; stage < Complete; stage++ {
		_, err = p.Register(stage, func(context.Context, Event) error {
			got = append(got, stage)
			if stage == ConfirmAddresses {
				return failure
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	reactions, unsub := p.Subscribe()
	defer unsub()
	events := make(chan Event, 1)
	events <- Event{Kind: Address, At: time.Now()}
	close(events)
	if err = p.Run(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []Stage{Rebind, ConfirmAddresses, RefreshMappings, RenewRelay, IdentifyPush, RestartProbing}) {
		t.Fatal(got)
	}
	for stage := Rebind; stage <= Complete; stage++ {
		r := <-reactions
		if r.Stage != stage {
			t.Fatal(r)
		}
		if stage == Complete && !errors.Is(r.Err, failure) {
			t.Fatal("lost failure", r)
		}
	}
}
func TestCoalescingAndWake(t *testing.T) {
	m := &Monitor{subs: make(map[uint64]chan Event)}
	ch, stop := m.Subscribe()
	defer stop()
	m.Notify(Interface)
	m.Notify(Address)
	m.Notify(Route)
	e := <-ch
	if e.Kind != Resync|Interface|Address|Route {
		t.Fatal(e)
	}
	now := time.Now().Round(0)
	wake, ok := wallEvent(now, now.Add(10*time.Second), 2*time.Second)
	if !ok || wake.Kind != Sleep|Wake|Resync || wake.Gap != 10*time.Second {
		t.Fatal(wake)
	}
	if _, ok = wallEvent(now, now.Add(time.Second), 2*time.Second); ok {
		t.Fatal("false wake")
	}
}
