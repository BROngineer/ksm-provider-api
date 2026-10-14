# Adapter developer guide

> **Reference implementation:** [`ksm-provider-sveltos`](https://github.com/brongineer/ksm-provider-sveltos)
> follows this guide; its `cmd/main.go`, `internal/sveltos/provider.go` and Helm chart are
> working examples of every section below.

An adapter connects k0rdent to a state management provider (Sveltos, Argo CD,
Flux, …). kcm tells the adapter which ServiceSet to deploy or delete; the
adapter translates the ServiceSet into provider objects, collects their state
and reports it back. kcm owns the ServiceSet status; the adapter never writes
it.

Read the [API contract](api.md) first: this guide shows how to meet it with
this module.

---

## 1. What you implement

```go
type Provider interface {
	DeployServices(ctx context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error)
	DeleteServices(ctx context.Context, req provider.DeleteServicesRequest) (provider.DeleteServicesResult, error)
}
```

from `provider/server`. Snippets below import the module's packages under the
aliases `providerserver` (`provider/server`) and `registryclient`
(`registry/client`) to avoid clashing with controller-runtime's `client`.
Everything else (HTTP, validation, authentication,
error mapping, TLS, heartbeats, notifications) comes from this module.

### 1.1 `DeployServices`

1. Read the ServiceSet `req.Namespace/req.Name`, including its `status`, and
   the objects it references (templates, sources, credentials, clusters).
2. If you cannot find it, return an error wrapping `providerserver.ErrNotFound`.
3. If the generation you read is lower than `req.Generation`, return an error
   wrapping `providerserver.ErrStaleGeneration`.
4. Create or update your provider objects. Skip writes when nothing changed:
   kcm calls you on every reconcile.
5. If that fails, **return a result, not an error**:

   ```go
   return provider.DeployServicesResult{
   	ObservedGeneration: serviceSet.Generation,
   	Applied:            false,
   	Reason:             new(provider.DeployServicesResultReasonSourceUnavailable),
   	Message:            new("chart repository oci://example.com/charts is unreachable"),
   }, nil
   ```

6. Otherwise collect the state of every service and return it with
   `Applied: true`, `Collected` set to whether collection worked, and
   `ObservedGeneration` set to the generation you acted on.
7. Fill `DeployedHash` per service with a fingerprint of what the provider has
   actually applied, if your provider exposes it. Leave it empty otherwise.

Any other error you return becomes a `500` with a generic message. The real
error is logged by the server, never sent to kcm, so it may safely contain
sensitive values.

### 1.2 `DeleteServices`

Delete your provider objects for the ServiceSet. Return `Done: false` while
they still exist, `Done: true` once they are gone, including when they were
already gone.

### 1.3 Reporting state changes

kcm does not watch your provider objects. Whenever the provider-side state of a
ServiceSet **may** have changed, call:

```go
notify.StateChanged(recorder, serviceSet)
```

Without it kcm never reconciles the ServiceSet again until its spec changes.
A typical adapter runs a poller or a watch on its provider objects and notifies
for the ServiceSet that owns them.

## 2. Wiring

```go
func run(ctx context.Context) error {
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                        scheme, // includes kcm's API types
		LeaderElection:                true,
		LeaderElectionID:              "ksm-provider-example",
		LeaderElectionReleaseOnCancel: true, // the Suspended heartbeat is sent before the lease is released
	})
	if err != nil {
		return err
	}
	cs := kubernetes.NewForConfigOrDie(mgr.GetConfig())

	// 1. Provider API: serves kcm's calls on every replica.
	authn := auth.NewCachedAuthenticator(
		auth.NewTokenReviewAuthenticator(cs, auth.ProviderAudience),
		time.Minute,
	)
	h, err := providerserver.NewHandler(providerserver.HandlerConfig{
		Provider:       &myProvider{client: mgr.GetClient()},
		Authenticator:  authn,
		AllowedCallers: []string{auth.ServiceAccountUsername("kcm-system", "kcm-controller-manager")},
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(&httpserver.Server{
		Handler:  h,
		Addr:     ":9443",
		CertFile: "/etc/ksm-provider/tls/tls.crt",
		KeyFile:  "/etc/ksm-provider/tls/tls.key",
	}); err != nil {
		return err
	}

	// 2. Registration: leader only.
	hb, err := registryclient.NewHeartbeater(registryclient.Config{
		RegistryURL:      "https://kcm-ksm-registry.kcm-system.svc:8443",
		RegistryCABundle: registryCA,
		TokenFile:        "/var/run/secrets/ksm/registry/token", // audience auth.RegistryAudience
		Name:             "ksm-provider-example",
		Endpoint:         "https://ksm-provider-example.example-system.svc:9443",
		EndpointCABundle: servingCA,
		APIVersions:      []string{"v1"},
		Interval:         10 * time.Second,
		SuspendCheck: registryclient.DeploymentScaledToZero(cs, types.NamespacedName{
			Namespace: podNamespace, Name: "ksm-provider-example",
		}),
		Logger: logger,
	})
	if err != nil {
		return err
	}
	if err := mgr.Add(hb); err != nil {
		return err
	}

	// 3. State-change notifications: leader only, after the first accepted heartbeat.
	recorder, stopRecorder := notify.NewRecorder(cs, scheme, "ksm-provider-example")
	defer stopRecorder()
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		select {
		case <-hb.Registered():
		case <-ctx.Done():
			return nil
		}
		return runPoller(ctx, recorder) // calls notify.StateChanged(recorder, serviceSet)
	})); err != nil {
		return err
	}

	return mgr.Start(ctx)
}
```

Notes:

- `httpserver.Server` runs on every replica; the `Heartbeater` and the
  `RunnableFunc` (which, like any runnable without `NeedLeaderElection`, is
  leader-only) run on the leader.
- Use the same name for the heartbeat `Name`, the notification component and
  the value users put in `ServiceSet.spec.provider.name`.
- `Interval` must be at least 1s. The heartbeat sends `ttlSeconds = 3 × Interval`.
- `SuspendCheck` makes a scale-to-zero suspend the provider; any other
  shutdown just lets the registration expire.
- Gate whatever **produces** notifications, not only the code emitting them.
  A poller that starts before `Registered()` treats every ServiceSet as changed
  on its first tick; if its output is buffered until registration, the buffer
  fills and drops events, and a fully deployed ServiceSet is never notified
  again. `ksm-provider-sveltos` wraps its poller in a runnable that waits for
  `Registered()` and drains the poller's channel without a gate.
- Notify only for ServiceSets whose `spec.provider.name` is your provider name:
  several adapters may run against the same kcm.

## 3. Deployment

### 3.1 Tokens

The adapter needs one projected ServiceAccount token, for heartbeats:

```yaml
volumes:
  - name: ksm-registry-token
    projected:
      sources:
        - serviceAccountToken:
            audience: ksm-registry   # auth.RegistryAudience
            expirationSeconds: 3600
            path: token
containers:
  - volumeMounts:
      - name: ksm-registry-token
        mountPath: /var/run/secrets/ksm/registry
        readOnly: true
```

Tokens sent by kcm are checked with a `TokenReview` for audience
`ksm-provider`; the adapter does not need a token of its own for that.

### 3.2 TLS

- A cert-manager `Certificate` for the adapter's `Service` DNS name, mounted
  at the `CertFile`/`KeyFile` paths. `httpserver.Server` reloads it on rotation.
- The issuing CA goes into `EndpointCABundle`, so kcm can verify the adapter.
- The registry's CA (from the kcm installation) goes into `RegistryCABundle`.

**Insecure mode (testing and demos only).** Without certificates:

```go
mgr.Add(&httpserver.Server{Handler: h, Addr: ":9443", Insecure: true})

registryclient.NewHeartbeater(registryclient.Config{
	RegistryURL: "http://kcm-ksm-registry.kcm-system.svc:8443", // when kcm's registry is insecure too
	Endpoint:    "http://ksm-provider-example.example-system.svc:9443",
	// no RegistryCABundle, no EndpointCABundle
	…
})
```

Make it an explicit opt-in (a flag or chart value, off by default) and never
infer it from missing certificate files: a missing mount must fail startup, not
silently downgrade to clear text. The risks are in
[API contract §5.1](api.md#51-insecure-mode-testing-and-demos-only).

### 3.3 Registration on the kcm side

kcm only accepts heartbeats from the ServiceAccount declared for your provider
name. Ship a `StateManagementProvider` named like your provider (usually in your
chart) whose `spec.registration.serviceAccount` is your adapter's ServiceAccount:

```yaml
apiVersion: k0rdent.mirantis.com/v1beta1
kind: StateManagementProvider
metadata:
  name: ksm-provider-example
spec:
  registration:
    serviceAccount:
      namespace: example-system
      name: ksm-provider-example
```

`registration` is the only field kcm uses; the CRD's other spec fields are kept
for compatibility and ignored.

kcm reads it on every heartbeat, so the provider is allowed as soon as the
object exists, without restarting kcm.

### 3.4 RBAC

| Resource | Verbs | Why |
|---|---|---|
| `servicesets.k0rdent.mirantis.com` | get, list, watch | read the requested ServiceSet, including status |
| objects ServiceSets reference: `servicetemplates`, `clusterdeployments`, `credentials`, `regions` (`k0rdent.mirantis.com`) | get, list, watch | build provider objects for the right target |
| chart sources (e.g. `helmcharts`, `helmrepositories`, `gitrepositories` in `source.toolkit.fluxcd.io`) | get, list, watch | resolve what to deploy |
| `secrets`, `configmaps` | get, list, watch | kubeconfigs, credentials, `valuesFrom` |
| `events` | create, patch | state-change notifications |
| `tokenreviews.authentication.k8s.io` | create | authenticate kcm |
| `deployments.apps` (its own) | get | `DeploymentScaledToZero` |
| `leases.coordination.k8s.io` | get, create, update | leader election |
| your provider's CRDs | as needed | provider objects, possibly in regional clusters |

The k0rdent rows depend on what your adapter reads; the list above is what an
adapter following kcm's own service semantics needs. A complete example,
including the Sveltos rows, is `charts/ksm-provider-sveltos/templates/rbac.yaml`
in `ksm-provider-sveltos`: a ClusterRole for cluster-wide reads, provider
objects, events and TokenReviews, and a namespaced Role for leases and `get` on
the adapter's own Deployment only (`resourceNames`).

## 4. Testing

### 4.1 Contract suite

Run the contract against your provider. Each scenario prepares the environment
(e.g. creates a ServiceSet in envtest) and returns the request kcm would send:

```go
func TestContract(t *testing.T) {
	env := startEnvtest(t)

	providertest.RunContract(t, providertest.Scenarios{
		NewProvider: func(t *testing.T) providerserver.Provider {
			return &myProvider{client: env.client}
		},
		Deployable: func(t *testing.T) provider.DeployServicesRequest {
			ss := env.createServiceSet(t, withValidTemplate())
			return provider.DeployServicesRequest{Name: ss.Name, Namespace: ss.Namespace, Generation: ss.Generation}
		},
		Failing: func(t *testing.T) provider.DeployServicesRequest {
			ss := env.createServiceSet(t, withMissingTemplate())
			return provider.DeployServicesRequest{Name: ss.Name, Namespace: ss.Namespace, Generation: ss.Generation}
		},
		Missing: func(t *testing.T) provider.DeployServicesRequest {
			return provider.DeployServicesRequest{Name: "does-not-exist", Namespace: "default", Generation: 1}
		},
		Stale: func(t *testing.T) provider.DeployServicesRequest {
			ss := env.createServiceSet(t, withValidTemplate())
			return provider.DeployServicesRequest{Name: ss.Name, Namespace: ss.Namespace, Generation: ss.Generation + 1}
		},
		DeleteTimeout: time.Minute,
	})
}
```

The suite talks to your provider through the real server and client, and
checks that:

| Check | Requirement |
|---|---|
| `deployable` | applied, with `observedGeneration` equal to the requested generation |
| `failing` | a deployment failure is a `200` with `applied: false`, not an error |
| `missing` | an invisible ServiceSet yields `ErrNotFound` |
| `stale` | a stale read yields `ErrStaleGeneration` |
| `idempotent` | deploying twice succeeds and reports the same services |
| `delete` | delete reaches `done` within `DeleteTimeout` and stays `done` |

`NewProvider` and `Deployable` are required; checks for scenarios left nil
are skipped.

### 4.2 Heartbeats

`providertest.NewFakeRegistry` runs the real registry handler, so you can test
your heartbeat configuration end to end:

```go
reg := providertest.NewFakeRegistry(t, "ksm-provider-example")
hb, err := registryclient.NewHeartbeater(registryclient.Config{
	RegistryURL: reg.URL,
	TokenFile:   reg.TokenFile,
	HTTPClient:  reg.HTTPClient,
	Name:        "ksm-provider-example",
	Endpoint:    "https://ksm-provider-example.example-system.svc:9443",
	APIVersions: []string{"v1"},
	Interval:    time.Second,
})
// start hb, wait for hb.Registered(), then:
entry, ok := reg.Registry.Lookup("ksm-provider-example")
```

## 5. Checklist

- [ ] `DeployServices` reports deployment failures as `applied: false` with a reason and a message
- [ ] `ErrNotFound` / `ErrStaleGeneration` are returned where they apply
- [ ] `ObservedGeneration` is the generation actually acted on
- [ ] repeated deploys do not rewrite unchanged provider objects
- [ ] `DeleteServices` is idempotent
- [ ] `notify.StateChanged` is called whenever provider-side state may have changed, after `Registered()`
- [ ] heartbeat, poller and notifications run on the leader only; `LeaderElectionReleaseOnCancel` is on
- [ ] the projected token uses `auth.RegistryAudience`; TLS certificate and CA are wired
- [ ] a `StateManagementProvider` named like the provider declares the adapter's ServiceAccount in `spec.registration.serviceAccount`
- [ ] `providertest.RunContract` passes
