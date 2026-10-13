# KSM provider API contract

This document is the contract between kcm and a k0rdent state management
provider (an *adapter*). It covers the two HTTP APIs, their error model, how
the two sides authenticate each other, and how an adapter tells kcm that a
ServiceSet needs attention.

The OpenAPI specs are the source of truth for the wire format:

- [`api/provider/openapi.yaml`](../api/provider/openapi.yaml): served by the adapter, called by kcm
- [`api/registry/openapi.yaml`](../api/registry/openapi.yaml): served by kcm, called by the adapter

Related documents:

- [kcm integration guide](kcm-integration.md)
- [Adapter developer guide](adapter-guide.md)

---

## 1. Overview

```text
           heartbeat (registry API)
 adapter ───────────────────────────────▶ kcm registry
    ▲                                         │ Lookup(provider name)
    │  deploy / delete (provider API)         ▼
    └──────────────────────────────────── kcm ServiceSet reconciler
                                              ▲
 adapter ── Event "ProviderStateChanged" ─────┘  (state-change notification)
```

1. The adapter registers itself in kcm's registry and keeps the registration
   alive with heartbeats.
2. kcm's ServiceSet reconciler looks up the provider named in
   `ServiceSet.spec.provider.name` and calls the adapter's endpoint.
3. The adapter deploys or deletes the ServiceSet's services with its provider,
   collects their state and returns it. kcm turns the result into the
   ServiceSet status.
4. When provider-side state may have changed, the adapter emits a Kubernetes
   Event on the ServiceSet so kcm reconciles it again.

All calls are synchronous.

## 2. Responsibilities

| Concern | Owner |
|---|---|
| Writing `ServiceSet.status` (conditions, service states, `deployed`) | kcm only. The adapter never updates the ServiceSet. |
| Reading the ServiceSet and the objects it references | the adapter, from the Kubernetes API, including `status` |
| Health assessment of deployed services | kcm |
| Fingerprinting what the provider has applied (`deployedHash`) | the adapter |
| Deciding when a ServiceSet must be reconciled again | the adapter, through state-change notifications (section 6) |

The request carries only the ServiceSet's key and generation. The adapter
reads everything else itself, so it needs read access to ServiceSets and to
whatever they reference (see the [adapter guide](adapter-guide.md#rbac)).

## 3. Provider API (served by the adapter)

Base path `/v1`. Both operations are `POST` with a JSON body.

### 3.1 `POST /v1/services:deploy` (`deployServices`)

Creates or updates the provider objects for a ServiceSet, collects the state
of its services and returns it.

Request:

```yaml
DeployServicesRequest:
  required: [name, namespace, generation]
  properties:
    name: {type: string, minLength: 1}
    namespace: {type: string, minLength: 1}
    generation: {type: integer, format: int64}
```

Response `200`:

```yaml
DeployServicesResult:
  required: [observedGeneration, applied, collected, services]
  properties:
    observedGeneration: {type: integer, format: int64}
    applied: {type: boolean}
    reason: {type: string, enum: [InvalidInput, SourceUnavailable, TargetUnreachable, ProviderError]}
    message: {type: string}
    collected: {type: boolean}
    collectMessage: {type: string}
    services:
      type: array
      items: {$ref: '#/components/schemas/ServiceState'}

ServiceState:
  required: [name, namespace, template, type, state]
  properties:
    name: {type: string}
    namespace: {type: string}
    template: {type: string}
    type: {type: string, enum: [Helm, Kustomize, Resource]}
    state: {type: string, enum: [Deployed, Provisioning, Failed, Pending, Deleting, Deleted]}
    version: {type: string}
    failureMessage: {type: string}
    lastStateTransitionTime: {type: string, format: date-time}
    lastDeployedHash: {type: string}
    deployedHash: {type: string}
```

Rules:

- **Deployment failures are results, not errors.** If the adapter cannot
  create or update its provider objects, it answers `200` with
  `applied: false`, a `reason` and a `message`. kcm shows them on the
  ServiceSet as the `ProviderApplied` condition. An HTTP error would only be
  logged and retried, and the user would see nothing.
- `applied: false` **requires** both `reason` and `message`. In that case
  `collected` is `false` and `services` may be empty.
- `collected` and `collectMessage` report whether the adapter could read the
  state of its provider objects.
- `observedGeneration` is the ServiceSet generation the adapter actually
  acted on. kcm discards results older than the generation it asked for.
- `deployedHash` is an opaque fingerprint of the provider-side configuration
  currently applied for the service. kcm only compares it for equality. Empty
  means "not available yet".
- The call must be **idempotent**. kcm sends it on every reconcile; an adapter
  that has nothing to change must not write.
- `services` must be an array, never `null`.

`reason` values:

| `reason` | Use when |
|---|---|
| `InvalidInput` | the ServiceSet or the templates it references cannot be converted into provider objects |
| `SourceUnavailable` | a chart, repository or other artifact cannot be fetched |
| `TargetUnreachable` | applying requires reaching the target cluster, and it cannot be reached |
| `ProviderError` | the provider rejected the objects or failed internally |

The enum is closed: adapters map their failures onto it and must not invent
values.

### 3.2 `POST /v1/services:delete` (`deleteServices`)

Removes the provider objects of a ServiceSet that is being deleted.

```yaml
DeleteServicesRequest:
  required: [name, namespace]
  properties:
    name: {type: string, minLength: 1}
    namespace: {type: string, minLength: 1}

DeleteServicesResult:          # 200
  required: [done]
  properties:
    done: {type: boolean}
```

- `done: false` while provider objects still exist or are being deleted;
  `done: true` once they are gone. kcm removes the ServiceSet finalizer only
  after `done: true`.
- Deleting an already deleted ServiceSet must return `done: true`: kcm may
  call again after a restart.
- There is no `generation`: deletion does not depend on the spec revision.

### 3.3 Status codes

| Status | `type` | Operations | Meaning | kcm reaction |
|---|---|---|---|---|
| `200` | — | both | call processed, including `applied: false` | apply the result |
| `400` | `invalid-request` | both | malformed request | log, retry later |
| `401` | `unauthenticated` | both | missing or invalid kcm token | log, retry later |
| `403` | `forbidden` | both | token valid, but not kcm's ServiceAccount | log, retry later |
| `404` | `not-found` | deploy | the adapter cannot see the ServiceSet yet (its cache lags) | retry (transient) |
| `409` | `stale-generation` | deploy | the adapter read an older generation than requested | retry (transient) |
| `default` | `internal` | both | unexpected adapter failure; also proxy errors | keep status, retry later |

### 3.4 Scaling

Any adapter replica may serve both operations: kcm never reconciles the same
ServiceSet concurrently. Only the heartbeat and the code emitting
state-change notifications must run on a single (leader) replica.

## 4. Registry API (served by kcm)

Base path `/v1`.

### 4.1 `POST /v1/providers/register` (`registerKSMProvider`)

Registers a provider or refreshes its registration. There is no separate
heartbeat endpoint: the heartbeat **is** this call, repeated.

```yaml
RegisterProviderRequest:
  required: [name, endpoint, state, apiVersions, ttlSeconds]
  properties:
    name: {type: string, minLength: 2, maxLength: 63, pattern: '^[a-z]([-a-z0-9]*[a-z0-9])?$'}
    endpoint: {type: string, format: uri}
    state: {type: string, enum: [Active, Suspended]}
    caBundle: {type: string, format: byte}
    ttlSeconds: {type: integer, minimum: 1}
    timeoutSeconds: {type: integer, minimum: 1, default: 30}
    apiVersions: {type: array, items: {type: string}, minItems: 1}
```

| Field | Meaning |
|---|---|
| `name` | the provider name, matched against `ServiceSet.spec.provider.name` |
| `endpoint` | absolute `http`/`https` base URL of the adapter's provider API |
| `state` | `Active`, or `Suspended` when the adapter is scaled to zero (section 4.3) |
| `caBundle` | PEM bundle kcm uses to verify the adapter's TLS certificate |
| `ttlSeconds` | how long the registration stays valid without another heartbeat |
| `timeoutSeconds` | how long kcm waits for one provider call (kcm may cap it) |
| `apiVersions` | provider API versions the adapter serves, currently `v1` |

| Status | `type` | Meaning |
|---|---|---|
| `204` | — | registered or refreshed |
| `400` | `invalid-request` | malformed request; `errors[]` lists every invalid field |
| `401` | `unauthenticated` | missing or invalid caller token |
| `403` | `forbidden` | the caller may not register this `name` |
| `503` | `not-leader` | the receiving kcm replica is not the leader; retry |
| `default` | `internal` | unexpected failure; retry |

**TTL rule.** `ttlSeconds` must be at least **3 × the heartbeat interval**, so
one late heartbeat does not expire the registration. The `registry/client`
`Heartbeater` sends exactly `ceil(3 × interval)` and rejects intervals below
one second.

### 4.2 Registry semantics

- Registrations live in kcm's memory. After a kcm restart they are rebuilt by
  the next heartbeats. Until then a provider is *not registered*, which kcm
  treats as **unknown**, never as failed: ServiceSet status is left untouched.
- An `Active` registration expires `ttlSeconds` after the last heartbeat.
- A `Suspended` registration never expires; the next `Active` heartbeat
  replaces it.
- Only the kcm leader holds the registry. Other replicas answer `503`; the
  heartbeat reaches the leader on a later attempt.
- Every change that matters to kcm (registered, suspended, resumed, endpoint
  or connection details changed, expired) makes kcm reconcile all ServiceSets
  of that provider. A plain refresh changes nothing.

### 4.3 Suspension

Suspending a provider means scaling the adapter to zero. On shutdown the
adapter sends one last heartbeat with `state: Suspended`; kcm then stops
calling it and reports the ServiceSets as suspended.

- Send `Suspended` only when the adapter's Deployment really has
  `spec.replicas == 0`. Rollouts, evictions and node drains also stop the
  pod and must not suspend the provider.
- A late `Suspended` heartbeat from an old pod is harmless: the new pod's next
  `Active` heartbeat overwrites it.
- A crashed adapter sends nothing; its registration expires.

## 5. Authentication

Both directions use projected ServiceAccount tokens checked with a Kubernetes
`TokenReview`, each with its own audience so a token cannot be replayed in the
other direction.

| Direction | Token audience | Checked by | Authorized identity |
|---|---|---|---|
| adapter → registry | `ksm-registry` (`auth.RegistryAudience`) | kcm | the ServiceAccount allowed to register the request's `name` |
| kcm → adapter | `ksm-provider` (`auth.ProviderAudience`) | the adapter | kcm's ServiceAccount |

- The token is sent as `Authorization: Bearer <token>`.
- A token is accepted only if the `TokenReview` authenticates it **for that
  audience**.
- kcm → adapter traffic is TLS, verified with the `caBundle` from the
  adapter's heartbeat. Trusting that bundle is safe because the heartbeat is
  authenticated.
- Both sides use TLS 1.2 or later.

### 5.1 Insecure mode (testing and demos only)

Both servers can serve plain HTTP (`httpserver.Server.Insecure`). The
endpoints are then `http://` URLs and the heartbeat carries no `caBundle`;
authentication is unchanged.

**Do not use it in production.** Bearer tokens travel in clear text. Anyone
who can read pod traffic can replay the adapter's registry token to register
their own endpoint under the provider's name, receive kcm's calls, and replay
kcm's provider token against the real adapter until the tokens expire. The
separate audiences do not help against an attacker who captures both. Where
pod-to-pod traffic is already encrypted (a service mesh with mTLS), insecure
mode only removes a redundant layer.

The server logs a warning at startup in insecure mode.

## 6. State-change notifications

kcm cannot watch provider objects, so the adapter tells kcm when a ServiceSet
needs to be reconciled again: whenever the provider-side state of a ServiceSet
**may** have changed (a deployment progressed, finished, failed, drifted).

A notification is a Kubernetes Event:

| Field | Value |
|---|---|
| `involvedObject` | the ServiceSet (`kind: ServiceSet`, group `k0rdent.mirantis.com`) |
| `type` | `Normal` |
| `reason` | `ProviderStateChanged` |
| `source.component` | the provider name |

Requirements:

- **Without notifications a ServiceSet is not reconciled again** until its spec
  changes. kcm only requeues on its own errors.
- client-go's default event recorder drops repeated Events for the same object
  (25, then one per 5 minutes). Adapters must use `notify.NewRecorder`, or
  configure an equivalent correlator, so notifications are never dropped.
- Repeated notifications patch the `count` of the same Event, so the number of
  Event objects does not grow.
- Emit notifications only after the first accepted heartbeat: earlier ones
  reach a registry that does not know the provider yet.

## 7. Error format

Every non-2xx response of both APIs is `application/problem+json`, a subset of
[RFC 9457](https://www.rfc-editor.org/rfc/rfc9457):

```yaml
Problem:
  required: [type, title, status]
  properties:
    type: {type: string, format: uri}     # the only field clients branch on
    title: {type: string}
    status: {type: integer}
    detail: {type: string}
    errors:                               # 400 only: one entry per invalid field
      type: array
      items:
        required: [field, message]
        properties:
          field: {type: string}
          message: {type: string}
```

| `type` | Status | API |
|---|---|---|
| `urn:ksm:problem:invalid-request` | 400 | both |
| `urn:ksm:problem:unauthenticated` | 401 | both |
| `urn:ksm:problem:forbidden` | 403 | both |
| `urn:ksm:problem:not-found` | 404 | provider |
| `urn:ksm:problem:stale-generation` | 409 | provider |
| `urn:ksm:problem:internal` | 500 and anything undeclared | both |
| `urn:ksm:problem:not-leader` | 503 | registry |

- `detail` never contains internal error chains or values from Secrets.
- Proxies in front of a server may answer with their own bodies. Clients must
  treat any non-problem error body as `internal` with the actual status; the
  `problem.FromResponse` helper does this.

## 8. Versioning

- Both APIs are versioned in the path (`/v1`). Breaking changes get a new
  path version.
- Adapters list the provider API versions they serve in `apiVersions`; kcm
  uses the highest version both sides support and treats a provider without
  a common version as not registered.
