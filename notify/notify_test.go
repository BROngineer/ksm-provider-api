package notify

import (
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

const waitTimeout = 5 * time.Second

type countingSink struct {
	events  []*corev1.Event
	creates int
	patches int
	mu      sync.Mutex
}

func (s *countingSink) Create(ev *corev1.Event) (*corev1.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.creates++
	s.events = append(s.events, ev.DeepCopy())
	return ev, nil
}

func (s *countingSink) Update(ev *corev1.Event) (*corev1.Event, error) {
	return ev, nil
}

func (s *countingSink) Patch(ev *corev1.Event, _ []byte) (*corev1.Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patches++
	return ev, nil
}

func (s *countingSink) delivered() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates + s.patches
}

func (s *countingSink) first() *corev1.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	return s.events[0]
}

func serviceSetRef() *corev1.ObjectReference {
	return &corev1.ObjectReference{
		Kind:       ServiceSetKind,
		APIVersion: ServiceSetGroup + "/v1beta1",
		Namespace:  "team-a",
		Name:       "cd-1-abcd",
		UID:        types.UID("uid-cd-1-abcd"),
	}
}

func waitDelivered(t *testing.T, sink *countingSink, want int) {
	t.Helper()
	deadline := time.Now().Add(waitTimeout)
	for sink.delivered() < want {
		if time.Now().After(deadline) {
			t.Fatalf("delivered %d notifications, want %d", sink.delivered(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStateChangedEmitsEventForServiceSet(t *testing.T) {
	sink := &countingSink{}
	rec, stop := newRecorder(sink, runtime.NewScheme(), "ksm-provider-sveltos")
	defer stop()

	StateChanged(rec, serviceSetRef())
	waitDelivered(t, sink, 1)

	ev := sink.first()
	if ev.Reason != Reason {
		t.Errorf("reason = %q, want %q", ev.Reason, Reason)
	}
	if ev.Type != corev1.EventTypeNormal {
		t.Errorf("type = %q, want %q", ev.Type, corev1.EventTypeNormal)
	}
	if ev.Source.Component != "ksm-provider-sveltos" {
		t.Errorf("source component = %q, want %q", ev.Source.Component, "ksm-provider-sveltos")
	}
	if ev.InvolvedObject.Kind != ServiceSetKind || ev.InvolvedObject.Name != "cd-1-abcd" || ev.InvolvedObject.Namespace != "team-a" {
		t.Errorf("involved object = %+v, want the ServiceSet", ev.InvolvedObject)
	}
	if ev.Namespace != "team-a" {
		t.Errorf("event namespace = %q, want the ServiceSet namespace", ev.Namespace)
	}
}

func TestRepeatedNotificationsAreNotDropped(t *testing.T) {
	sink := &countingSink{}
	rec, stop := newRecorder(sink, runtime.NewScheme(), "ksm-provider-sveltos")
	defer stop()

	const notifications = 50
	for range notifications {
		StateChanged(rec, serviceSetRef())
	}

	waitDelivered(t, sink, notifications)
}

func TestNewRecorderWritesThroughClientset(t *testing.T) {
	cs := fake.NewClientset()
	rec, stop := NewRecorder(cs, runtime.NewScheme(), "ksm-provider-sveltos")
	defer stop()

	StateChanged(rec, serviceSetRef())

	deadline := time.Now().Add(waitTimeout)
	for {
		list, err := cs.CoreV1().Events("team-a").List(t.Context(), metav1.ListOptions{})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		if len(list.Items) == 1 && list.Items[0].Reason == Reason {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("events = %+v, want one notification", list.Items)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestFieldSelector(t *testing.T) {
	sel, err := fields.ParseSelector(FieldSelector())
	if err != nil {
		t.Fatalf("FieldSelector() = %q does not parse: %v", FieldSelector(), err)
	}
	match := fields.Set{"involvedObject.kind": ServiceSetKind, "reason": Reason}
	if !sel.Matches(match) {
		t.Errorf("selector %q does not match a notification", sel)
	}
	for _, other := range []fields.Set{
		{"involvedObject.kind": "Pod", "reason": Reason},
		{"involvedObject.kind": ServiceSetKind, "reason": "ServiceSetEnsureProfileFailed"},
	} {
		if sel.Matches(other) {
			t.Errorf("selector %q matches %v", sel, other)
		}
	}
}

func TestRequestFor(t *testing.T) {
	notification := func(mutate func(*corev1.Event)) *corev1.Event {
		ev := &corev1.Event{Reason: Reason, InvolvedObject: *serviceSetRef()}
		if mutate != nil {
			mutate(ev)
		}
		return ev
	}
	tests := []struct {
		name string
		ev   *corev1.Event
		want bool
	}{
		{"notification", notification(nil), true},
		{"other reason", notification(func(e *corev1.Event) { e.Reason = "Something" }), false},
		{"other kind", notification(func(e *corev1.Event) { e.InvolvedObject.Kind = "Pod" }), false},
		{"ServiceSet of another group", notification(func(e *corev1.Event) { e.InvolvedObject.APIVersion = "example.com/v1" }), false},
		{"core group", notification(func(e *corev1.Event) { e.InvolvedObject.APIVersion = "v1" }), false},
		{"no name", notification(func(e *corev1.Event) { e.InvolvedObject.Name = "" }), false},
		{"nil event", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := RequestFor(tt.ev)
			if ok != tt.want {
				t.Fatalf("ok = %v, want %v", ok, tt.want)
			}
			if ok && got != (types.NamespacedName{Namespace: "team-a", Name: "cd-1-abcd"}) {
				t.Errorf("request = %v, want team-a/cd-1-abcd", got)
			}
		})
	}
}
