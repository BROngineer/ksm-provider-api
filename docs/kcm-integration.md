# kcm integration guide

> **Status: planned.** This describes how kcm is *supposed* to use this module.
> The kcm side is being implemented now; this guide will be updated where the
> implementation diverges.

kcm plays two roles:

- it **serves the registry**, where adapters register and send heartbeats;
- it **calls adapters** through the provider API from its ServiceSet
  reconciler, and **watches state-change notifications** to know when to call
  again.

See the [API contract](api.md) for the wire-level rules this guide relies on.

---

## 1. Packages used by kcm

| Package | Used for |
|---|---|
| `registry/server` | the registry store and the register handler |
| `httpserver` | serving the registry handler over TLS (plain HTTP in insecure mode) |
| `auth` | authenticating adapters' heartbeats |
| `provider/client` | calling adapters |
| `problem` | branching on error types |
| `notify` | watching state-change notifications |
| `providertest` | `FakeProvider` and `TestServer` in kcm's tests |

Snippets import `registry/server` as `registryserver` and `provider/client` as
`providerclient`, and the generated `api/provider` package as `provider`.

## 2. Serving the registry

### 2.1 Wiring

```go
cs := kubernetes.NewForConfigOrDie(mgr.GetConfig())

// Transitions are handed to the controller through a channel source (section 2.3).
transitions := make(chan event.GenericEvent, 1024)

reg := registryserver.NewRegistry(registryserver.WithTransitionHandler(func(t registryserver.Transition) {
	transitions <- event.GenericEvent{Object: &metav1.PartialObjectMetadata{
		ObjectMeta: metav1.ObjectMeta{Name: t.Name},
	}}
}))

authn := auth.NewCachedAuthenticator(
	auth.NewTokenReviewAuthenticator(cs, auth.RegistryAudience),
	time.Minute,
)

h, err := registryserver.NewHandler(registryserver.HandlerConfig{
	Registry:      reg,
	Authenticator: authn,
	Allowed:       allowedProviders, // provider name -> ServiceAccount username, from Helm values
	IsLeader:      registryserver.LeaderFromElected(mgr.Elected()),
	Logger:        slog.New(logr.ToSlogHandler(ctrl.Log.WithName("ksm-registry"))),
})
if err != nil {
	return err
}

// The sweeper (expiry) runs on the leader only: Registry.NeedLeaderElection() is true.
if err := mgr.Add(reg); err != nil {
	return err
}
// The HTTP server runs on every replica, so non-leaders can answer 503 not-leader:
// httpserver.Server.NeedLeaderElection() is false.
if err := mgr.Add(&httpserver.Server{
	Handler:  h,
	Addr:     ":8443",
	CertFile: "/etc/ksm-registry/tls/tls.crt",
	KeyFile:  "/etc/ksm-registry/tls/tls.key",
}); err != nil {
	return err
}
```

### 2.2 Configuration

| Setting | Source |
|---|---|
| allow-list (`Allowed`) | Helm values, e.g. `ksm.providers: [{name: ksm-projectsveltos, serviceAccount: projectsveltos/ksm-provider-sveltos}]`, turned into usernames with `auth.ServiceAccountUsername(namespace, name)` |
| serving certificate | a cert-manager `Certificate` mounted into the pod; `httpserver.Server` reloads it when rotated |
| registry `Service` | exposes the port above; adapters use its DNS name as `RegistryURL` |
| CA for adapters | the registry certificate's CA, handed to adapters (their `RegistryCABundle`) |
| RBAC | `create` on `tokenreviews.authentication.k8s.io` |
| insecure mode | a Helm value (e.g. `ksm.registry.insecure`) setting `httpserver.Server{Insecure: true}` and dropping the certificate; adapters then use an `http://` `RegistryURL` and no CA. Testing and demos only, see [API contract §5.1](api.md#51-insecure-mode-testing-and-demos-only) |

Adapters in insecure mode announce an `http://` endpoint and no `caBundle`;
`provider/client` calls such endpoints as they are, so kcm needs no switch for
that direction.

### 2.3 Re-enqueueing ServiceSets on transitions

Each transition (registered, updated, suspended, resumed, expired) must
reconcile **all ServiceSets of that provider**. Without it, ServiceSets
reconciled while the provider was not yet registered (e.g. right after a kcm
restart) keep a stale `status.provider` until something else touches them.

```go
ctrl.NewControllerManagedBy(mgr).
	For(&kcmv1.ServiceSet{}).
	WatchesRawSource(source.Channel(transitions, handler.EnqueueRequestsFromMapFunc(
	func(ctx context.Context, o client.Object) []reconcile.Request {
		var list kcmv1.ServiceSetList
		if err := mgr.GetClient().List(ctx, &list,
			client.MatchingFields{kcmv1.ServiceSetProviderIndexKey: o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for _, ss := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&ss)})
		}
		return reqs
	},
))).
	Complete(r)
```

The transition handler runs synchronously inside heartbeat handling: keep it
non-blocking in practice (a buffered channel drained by the controller).

## 3. Calling adapters

### 3.1 Looking up the provider

```go
entry, ok := reg.Lookup(serviceSet.Spec.Provider.Name)
switch {
case !ok:
	// Not registered: unknown, not failed. Leave status untouched and return;
	// the registration transition re-enqueues this ServiceSet.
case entry.State == registryserver.StateSuspended:
	// Set status.provider.suspended, emit the event on transition, do not call.
}
```

### 3.1.1 Checking the target cluster

Whether the target cluster exists is kcm's concern, not the provider's: check
it before calling. An adapter whose provider objects match no cluster returns
the services it last reported, unchanged (the Sveltos adapter does exactly what
kcm's in-tree code did), so their states may be stale. kcm knows the
ClusterDeployment and resolves the target cluster itself; when it is missing,
kcm sets `status.deployed=false` and does not trust the reported states.

### 3.2 Building clients

Build one `provider/client.Client` per provider and reuse it; rebuild it when
the entry's connection details change (a `TransitionUpdated` arrives, or the
cached client's endpoint/CA/timeout no longer match the entry).

```go
c, err := providerclient.New(providerclient.Config{
	Endpoint:  entry.Endpoint,
	CABundle:  entry.CABundle,
	TokenFile: "/var/run/secrets/ksm/provider/token", // projected, audience auth.ProviderAudience
	Timeout:   min(entry.Timeout, maxProviderTimeout),
})
```

`maxProviderTimeout` is kcm's cap, so one slow adapter cannot hold all
reconcile workers.

The kcm Pod needs a projected ServiceAccount token volume with
`audience: ksm-provider` (`auth.ProviderAudience`) at that path. The client
re-reads it on every call, so kubelet rotation is picked up.

### 3.3 Deploy

```go
res, err := c.DeployServices(ctx, provider.DeployServicesRequest{
	Name:       serviceSet.Name,
	Namespace:  serviceSet.Namespace,
	Generation: serviceSet.Generation,
})
if err != nil {
	return ctrl.Result{}, err // see 3.5
}
if res.ObservedGeneration < serviceSet.Generation {
	return ctrl.Result{}, errStaleResult // the adapter acted on an older spec; retry
}
// res.Applied / res.Reason / res.Message -> ProviderApplied condition
// res.Collected / res.CollectMessage     -> ServiceSetStatusesCollected condition
// res.Services                           -> status.services (kcm keeps per-service conditions)
// res.Services[i].DeployedHash           -> input to the health verifier
```

After writing the status, return an error when `Applied` or `Collected` is
false. kcm's in-tree reconciler returned the errors of `ensureProfile` and
`collectServiceStatuses`, so the rate limiter retried; the contract reports
those failures as results, and kcm must keep the retry itself.

When `Collected` is false the adapter has not fingerprinted anything: every
`DeployedHash` is empty and the verifier must not stamp, exactly as kcm skipped
verification after a failed collection.

### 3.4 Delete

```go
res, err := c.DeleteServices(ctx, provider.DeleteServicesRequest{
	Name:      serviceSet.Name,
	Namespace: serviceSet.Namespace,
})
if err != nil {
	return ctrl.Result{}, err
}
if !res.Done {
	return ctrl.Result{RequeueAfter: deleteRequeueInterval}, nil
}
// remove the finalizer
```

### 3.5 Errors

| Error | Meaning | Handling |
|---|---|---|
| `*providerclient.ProblemError` with `Type == problem.NotFound` or `problem.StaleGeneration` | the adapter is behind kcm | return the error; the rate limiter retries |
| `*providerclient.ProblemError`, `Type == problem.InvalidRequest` | kcm sent a malformed request | log as a kcm bug; return the error |
| `*providerclient.ProblemError`, `Type == problem.Unauthenticated` / `problem.Forbidden` | token or allow-list misconfiguration | log with the type; return the error |
| `*providerclient.ProblemError`, `Type == problem.Internal` | adapter or proxy failure; `Status` holds the real code | keep status; return the error |
| `providerclient.ErrInvalidResult` | the adapter returned a `200` that breaks the contract | log as an adapter bug; return the error |
| anything else (`context.DeadlineExceeded`, connection refused) | no answer at all | keep status; return the error |

```go
if pe, ok := errors.AsType[*providerclient.ProblemError](err); ok {
	log.Info("provider call failed", "type", pe.Type, "status", pe.Status, "detail", pe.Detail)
}
```

## 4. Watching state-change notifications

Adapters emit an Event with reason `ProviderStateChanged` on a ServiceSet
whenever its provider-side state may have changed. kcm maps each one to a
reconcile of that ServiceSet.

Restrict the Event informer to notifications:

```go
cache.Options{
	ByObject: map[client.Object]cache.ByObject{
		&corev1.Event{}: {Field: fields.ParseSelectorOrDie(notify.FieldSelector())},
	},
}
```

Map Events to requests:

```go
Watches(&corev1.Event{}, handler.EnqueueRequestsFromMapFunc(
	func(_ context.Context, o client.Object) []reconcile.Request {
		ev, ok := o.(*corev1.Event)
		if !ok {
			return nil
		}
		nn, ok := notify.RequestFor(ev)
		if !ok {
			return nil
		}
		return []reconcile.Request{{NamespacedName: nn}}
	},
))
```

- React to **updates as well as creates**: a repeated notification patches the
  `count` of the existing Event. Do not add a generation-changed predicate;
  Events have no generation and every update would be dropped.
- RBAC: `get`, `list`, `watch` on `events`.

## 5. Testing kcm with `providertest`

```go
fake := &providertest.FakeProvider{
	DeployFunc: func(_ context.Context, req provider.DeployServicesRequest) (provider.DeployServicesResult, error) {
		return provider.DeployServicesResult{
			ObservedGeneration: req.Generation,
			Applied:            false,
			Reason:             new(provider.DeployServicesResultReasonSourceUnavailable),
			Message:            new("chart repository unreachable"),
			Services:           []provider.ServiceState{},
		}, nil
	},
}
srv := providertest.NewTestServer(t, fake)

reg.Register(registryserver.Registration{
	Name:        "ksm-projectsveltos",
	Endpoint:    srv.URL,
	State:       registryserver.StateActive,
	APIVersions: []string{"v1"},
	TTL:         time.Hour,
	Timeout:     10 * time.Second,
})
```

- `srv.HTTPClient` trusts the test server's certificate and `srv.TokenFile`
  holds the accepted token: kcm's client construction must let tests inject
  both (`providerclient.Config.HTTPClient`, `TokenFile`).
- `fake.DeployCalls()` / `fake.DeleteCalls()` show what kcm sent.
- Leave `DeployFunc` nil for the default: applied at the requested generation,
  no services.
