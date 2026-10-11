package nudge

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/record"
)

const (
	// Reason marks Events that ask kcm to reconcile the involved ServiceSet.
	Reason = "ProviderStateChanged"

	ServiceSetKind  = "ServiceSet"
	ServiceSetGroup = "k0rdent.mirantis.com"

	message = "provider state may have changed"
)

// The spam filter is a per-object token bucket; these values make it unreachable for nudges,
// which arrive at most once per poller tick per ServiceSet.
const (
	nudgeBurst = 1 << 20
	nudgeQPS   = 1000
)

// NewRecorder returns a recorder whose correlator does not drop nudges: client-go's default
// spam filter allows 25 Events per object and then one per 5 minutes, which would silently
// swallow nudges for busy ServiceSets. Call the returned func on shutdown to flush and stop.
func NewRecorder(cs kubernetes.Interface, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	return newRecorder(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")}, scheme, component)
}

func newRecorder(sink record.EventSink, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	b := record.NewBroadcaster(record.WithCorrelatorOptions(record.CorrelatorOptions{
		BurstSize: nudgeBurst,
		QPS:       nudgeQPS,
	}))
	b.StartRecordingToSink(sink)
	return b.NewRecorder(scheme, corev1.EventSource{Component: component}), b.Shutdown
}

// Nudge asks kcm to reconcile obj, which must be a ServiceSet.
func Nudge(rec record.EventRecorder, obj runtime.Object) {
	rec.Event(obj, corev1.EventTypeNormal, Reason, message)
}

// FieldSelector limits kcm's Event watch to nudges.
func FieldSelector() string {
	return fields.AndSelectors(
		fields.OneTermEqualSelector("involvedObject.kind", ServiceSetKind),
		fields.OneTermEqualSelector("reason", Reason),
	).String()
}

// RequestFor returns the ServiceSet a nudge targets. kcm must handle both created and updated
// Events: the correlator patches the count of a repeated nudge instead of creating a new Event.
func RequestFor(ev *corev1.Event) (types.NamespacedName, bool) {
	if ev == nil || ev.Reason != Reason {
		return types.NamespacedName{}, false
	}
	ref := ev.InvolvedObject
	gv, err := schema.ParseGroupVersion(ref.APIVersion)
	if err != nil || gv.Group != ServiceSetGroup || ref.Kind != ServiceSetKind || ref.Name == "" {
		return types.NamespacedName{}, false
	}
	return types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, true
}
