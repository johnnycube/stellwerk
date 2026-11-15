package usecase

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/johnnycube/stellwerk/internal/core/domain"
	"github.com/johnnycube/stellwerk/internal/core/ports"
	"github.com/johnnycube/stellwerk/internal/core/ports/portstest"
)

func newDeliver(t *testing.T, p domain.DeliveryPolicy, senders map[string]ports.Sender) (*DeliverMessages, *portstest.Recorder) {
	t.Helper()
	rec := portstest.NewRecorder()
	d := NewDeliverMessages(senders, p, rec, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go d.Run(ctx)
	return d, rec
}

var fast = domain.DeliveryPolicy{QueueSize: 10, MaxRetries: 3, MaxBackoff: time.Millisecond}

func TestDeliverMessages_SendsAndRecords(t *testing.T) {
	s := portstest.NewSender()
	d, rec := newDeliver(t, fast, map[string]ports.Sender{"m/a": s})
	done, err := d.Enqueue(domain.Delivery{Route: "r", Destination: "crit", Endpoint: "m/a", Address: "!x", Message: domain.Message{Markdown: "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := s.Sent(); len(got) != 1 || got[0].Address != "!x" || got[0].Msg.Markdown != "hi" {
		t.Errorf("sent %+v", got)
	}
	if rec.DeliveryCount() != 1 || rec.Deliveries[0].Err != nil || rec.Deliveries[0].Delivery.Destination != "crit" {
		t.Errorf("recorded %+v", rec.Deliveries)
	}
}

func TestDeliverMessages_RetriesUntilSuccess(t *testing.T) {
	s := portstest.NewSender()
	s.FailNext(2)
	d, _ := newDeliver(t, fast, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; err != nil || s.Attempts() != 3 || len(s.Sent()) != 1 {
		t.Errorf("err=%v attempts=%d", err, s.Attempts())
	}
}

func TestDeliverMessages_GivesUpAfterMaxRetries(t *testing.T) {
	s := portstest.NewSender()
	s.FailAll(true)
	d, rec := newDeliver(t, fast, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; !errors.Is(err, portstest.ErrDown) || s.Attempts() != 3 {
		t.Errorf("err=%v attempts=%d", err, s.Attempts())
	}
	if rec.Deliveries[0].Err == nil {
		t.Error("the failure must be recorded")
	}
}

func TestDeliverMessages_UnknownEndpoint(t *testing.T) {
	d, _ := newDeliver(t, fast, map[string]ports.Sender{})
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "nope/x"}); !errors.Is(err, ErrUnknownEndpoint) {
		t.Errorf("got %v", err)
	}
}

func TestDeliverMessages_QueueFull(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	defer s.Release()
	d, _ := newDeliver(t, domain.DeliveryPolicy{QueueSize: 1, MaxRetries: 1}, map[string]ports.Sender{"m/a": s})
	d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	<-s.Attempted()
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); err != nil {
		t.Fatalf("one job fits: %v", err)
	}
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); !errors.Is(err, ErrQueueFull) {
		t.Errorf("got %v", err)
	}
}

func TestDeliverMessages_EndpointsAreIsolated(t *testing.T) {
	slow, ok := portstest.NewSender(), portstest.NewSender()
	slow.Block()
	defer slow.Release()
	d, _ := newDeliver(t, fast, map[string]ports.Sender{"m/slow": slow, "m/ok": ok})
	d.Enqueue(domain.Delivery{Endpoint: "m/slow"})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/ok"})
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("a blocked endpoint held up another one")
	}
}

func TestDeliverMessages_KeepsOrderPerEndpoint(t *testing.T) {
	s := portstest.NewSender()
	d, _ := newDeliver(t, domain.DeliveryPolicy{QueueSize: 50, MaxRetries: 1}, map[string]ports.Sender{"m/a": s})
	var last <-chan error
	for i := 0; i < 20; i++ {
		var err error
		if last, err = d.Enqueue(domain.Delivery{Endpoint: "m/a", Message: domain.Message{Markdown: fmt.Sprint(i)}}); err != nil {
			t.Fatal(err)
		}
	}
	<-last
	for i, got := range s.Sent() {
		if got.Msg.Markdown != fmt.Sprint(i) {
			t.Fatalf("position %d got %s", i, got.Msg.Markdown)
		}
	}
}

func TestDeliverMessages_StopsRetryingOnShutdown(t *testing.T) {
	s := portstest.NewSender()
	s.FailAll(true)
	rec := portstest.NewRecorder()
	d := NewDeliverMessages(map[string]ports.Sender{"m/a": s}, domain.DeliveryPolicy{QueueSize: 1, MaxRetries: 100, MaxBackoff: time.Hour}, rec, quiet())
	ctx, cancel := context.WithCancel(context.Background())
	go d.Run(ctx)
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	<-s.Attempted()
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Error("want an error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry loop ignored shutdown")
	}
}

func TestDeliverMessages_SaneDefaults(t *testing.T) {
	s := portstest.NewSender()
	s.FailNext(1)
	d, _ := newDeliver(t, domain.DeliveryPolicy{}, map[string]ports.Sender{"m/a": s})
	done, _ := d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if err := <-done; err == nil || s.Attempts() != 1 {
		t.Errorf("zero policy means one attempt and a queue of one: err=%v attempts=%d", err, s.Attempts())
	}
}

func TestDeliverMessages_DrainWaitsForQueuedWork(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	d, _ := newDeliver(t, fast, map[string]ports.Sender{"m/a": s})
	for i := 0; i < 3; i++ {
		if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); err != nil {
			t.Fatal(err)
		}
	}
	<-s.Attempted()
	if d.Pending() != 3 {
		t.Fatalf("pending %d", d.Pending())
	}
	short, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := d.Drain(short); err == nil {
		t.Fatal("drain must report unfinished work when its time is up")
	}
	s.Release()
	if err := d.Drain(context.Background()); err != nil || len(s.Sent()) != 3 || d.Pending() != 0 {
		t.Errorf("drain: err=%v sent=%d pending=%d", err, len(s.Sent()), d.Pending())
	}
}

func TestDeliverMessages_RejectedWorkIsNotPending(t *testing.T) {
	s := portstest.NewSender()
	s.Block()
	defer s.Release()
	d, _ := newDeliver(t, domain.DeliveryPolicy{QueueSize: 1, MaxRetries: 1}, map[string]ports.Sender{"m/a": s})
	d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	<-s.Attempted()
	d.Enqueue(domain.Delivery{Endpoint: "m/a"})
	if _, err := d.Enqueue(domain.Delivery{Endpoint: "m/a"}); err == nil {
		t.Fatal("queue should be full")
	}
	if d.Pending() != 2 {
		t.Errorf("pending %d, want 2", d.Pending())
	}
}
