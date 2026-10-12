package notify

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

// The spam filter is a per-object token bucket; these values make it unreachable for
// notifications, which arrive at most once per poller tick per ServiceSet.
const (
	notifyBurst = 1 << 20
	notifyQPS   = 1000
)

// NewRecorder returns a recorder whose correlator does not drop notifications: client-go's
// default spam filter allows 25 Events per object and then one per 5 minutes, which would
// silently swallow notifications for busy ServiceSets. Call the returned func on shutdown to
// flush and stop.
func NewRecorder(cs kubernetes.Interface, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	return newRecorder(&typedcorev1.EventSinkImpl{Interface: cs.CoreV1().Events("")}, scheme, component)
}

func newRecorder(sink record.EventSink, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	b := record.NewBroadcaster(record.WithCorrelatorOptions(record.CorrelatorOptions{
		BurstSize: notifyBurst,
		QPS:       notifyQPS,
	}))
	b.StartRecordingToSink(sink)
	return b.NewRecorder(scheme, corev1.EventSource{Component: component}), b.Shutdown
}

// StateChanged tells kcm that the provider-side state of obj, which must be a ServiceSet, may
// have changed. kcm reconciles the ServiceSet in response.
func StateChanged(rec record.EventRecorder, obj runtime.Object) {
	rec.Event(obj, corev1.EventTypeNormal, Reason, message)
}

// FieldSelector limits kcm's Event watch to state-change notifications.
func FieldSelector() string {
	return fields.AndSelectors(
		fields.OneTermEqualSelector("involvedObject.kind", ServiceSetKind),
		fields.OneTermEqualSelector("reason", Reason),
	).String()
}

// RequestFor returns the ServiceSet a notification targets. kcm must handle both created and
// updated Events: the correlator patches the count of a repeated notification instead of
// creating a new Event.
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
