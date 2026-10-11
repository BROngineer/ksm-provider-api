package nudge

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
)

const (
	// Reason marks Events that ask kcm to reconcile the involved ServiceSet.
	Reason = "ProviderStateChanged"

	ServiceSetKind  = "ServiceSet"
	ServiceSetGroup = "k0rdent.mirantis.com"
)

// NewRecorder returns a recorder whose correlator does not drop nudges: client-go's default
// spam filter allows 25 Events per object and then one per 5 minutes, which would silently
// swallow nudges for busy ServiceSets. Call the returned func on shutdown to flush and stop.
func NewRecorder(cs kubernetes.Interface, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	return nil, func() {}
}

func newRecorder(sink record.EventSink, scheme *runtime.Scheme, component string) (record.EventRecorder, func()) {
	return nil, func() {}
}

// Nudge asks kcm to reconcile obj, which must be a ServiceSet.
func Nudge(rec record.EventRecorder, obj runtime.Object) {}

// FieldSelector limits kcm's Event watch to nudges.
func FieldSelector() string {
	return ""
}

// RequestFor returns the ServiceSet a nudge targets. kcm must handle both created and updated
// Events: the correlator patches the count of a repeated nudge instead of creating a new Event.
func RequestFor(ev *corev1.Event) (types.NamespacedName, bool) {
	return types.NamespacedName{}, false
}
