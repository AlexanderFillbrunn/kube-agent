package redisinfra

import (
	"context"
	"testing"
	"time"

	"fillbrunn.ai/kube-agent/pkg/agent"
)

// cancelledSoonerThan reports whether a watcher's channel closed within the
// wait, failing the test on a close that never came rather than leaving the
// goroutine behind.
func cancelledSoonerThan(t *testing.T, cancelled <-chan struct{}, wait time.Duration) bool {
	t.Helper()
	select {
	case <-cancelled:
		return true
	case <-time.After(wait):
		return false
	}
}

func TestNotifyClosesTheWatchChannel(t *testing.T) {
	client, _ := newRedis(t)
	cancels := NewRedisCancels(client)
	ref := agent.ArcRef{SessionID: "s", ArcID: "a"}

	cancelled, stop := cancels.Watch(context.Background(), ref)
	defer stop()

	if err := cancels.NotifyCancel(context.Background(), ref); err != nil {
		t.Fatalf("could not announce the cancel: %v", err)
	}
	if !cancelledSoonerThan(t, cancelled, 2*time.Second) {
		t.Fatal("the watcher was not told of the cancel")
	}
}

func TestNotifyClosesEveryWatcherOfTheArc(t *testing.T) {
	client, _ := newRedis(t)
	cancels := NewRedisCancels(client)
	ref := agent.ArcRef{SessionID: "s", ArcID: "a"}

	first, stopFirst := cancels.Watch(context.Background(), ref)
	defer stopFirst()
	second, stopSecond := cancels.Watch(context.Background(), ref)
	defer stopSecond()

	if err := cancels.NotifyCancel(context.Background(), ref); err != nil {
		t.Fatalf("could not announce the cancel: %v", err)
	}
	for name, cancelled := range map[string]<-chan struct{}{"first": first, "second": second} {
		if !cancelledSoonerThan(t, cancelled, 2*time.Second) {
			t.Fatalf("the %s watcher was not told of the cancel", name)
		}
	}
}

func TestNotifyReachesOnlyTheArcItNames(t *testing.T) {
	client, _ := newRedis(t)
	cancels := NewRedisCancels(client)
	watched := agent.ArcRef{SessionID: "s", ArcID: "a"}
	other := agent.ArcRef{SessionID: "s", ArcID: "b"}

	cancelled, stop := cancels.Watch(context.Background(), watched)
	defer stop()
	otherCancelled, stopOther := cancels.Watch(context.Background(), other)
	defer stopOther()

	if err := cancels.NotifyCancel(context.Background(), other); err != nil {
		t.Fatalf("could not announce the cancel: %v", err)
	}
	if !cancelledSoonerThan(t, otherCancelled, 2*time.Second) {
		t.Fatal("the watcher of the cancelled arc was not told")
	}
	if cancelledSoonerThan(t, cancelled, 100*time.Millisecond) {
		t.Fatal("a cancel for one arc closed the watcher of another")
	}
}

func TestStopEndsTheWatch(t *testing.T) {
	client, _ := newRedis(t)
	cancels := NewRedisCancels(client)
	ref := agent.ArcRef{SessionID: "s", ArcID: "a"}

	cancelled, stop := cancels.Watch(context.Background(), ref)
	stop()

	if err := cancels.NotifyCancel(context.Background(), ref); err != nil {
		t.Fatalf("could not announce the cancel: %v", err)
	}
	if cancelledSoonerThan(t, cancelled, 100*time.Millisecond) {
		t.Fatal("a watcher that stopped was told of the cancel anyway")
	}
}

func TestNotifyBeforeTheWatchCostsLatencyNotCorrectness(t *testing.T) {
	client, _ := newRedis(t)
	cancels := NewRedisCancels(client)
	ref := agent.ArcRef{SessionID: "s", ArcID: "a"}

	if err := cancels.NotifyCancel(context.Background(), ref); err != nil {
		t.Fatalf("could not announce the cancel: %v", err)
	}

	// A watcher that arrives after the announcement gets a channel nobody
	// closes, which is the contract's failure mode: the cancel is in the
	// log, and the guard refuses the run's next guarded write.
	cancelled, stop := cancels.Watch(context.Background(), ref)
	defer stop()
	if cancelledSoonerThan(t, cancelled, 100*time.Millisecond) {
		t.Fatal("a watcher was closed by an announcement it never saw")
	}
}
