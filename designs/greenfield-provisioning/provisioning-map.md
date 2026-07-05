# Karpenter Provisioning Map (for the Greenfield NodeClaim POC)

Repo: /home/jamesmt/src/greenfield-provisioning/karpenter (upstream kubernetes-sigs/karpenter, fresh clone).
All paths below are relative to that repo root. Line numbers are from this checkout.

Packages that matter:

- pkg/controllers/provisioning        - batcher, Provisioner (the controller loop)
- pkg/controllers/provisioning/scheduling - Scheduler, NodeClaim, ExistingNode, Topology (the simulation)
- pkg/scheduling                      - Requirements algebra (label/requirement intersection)
- pkg/cloudprovider                   - InstanceType, Offering (prices live here)
- pkg/cloudprovider/fake              - fake cloudprovider used by unit tests and benchmarks
- kwok/cloudprovider                  - kwok cloudprovider with 144 instance types + real-looking prices
- pkg/test, pkg/test/expectations     - envtest harness and Expect* helpers

## 1. Where the batch of pending pods is formed

Trigger path:

- pkg/controllers/provisioning/controller.go:64-78 - PodController.Reconcile fires on every
  provisionable pod and calls `c.provisioner.Trigger(p.UID)` (line 70). NodeController does the
  same for nodes tainted for disruption (line 119).
- pkg/controllers/provisioning/provisioner.go:112-114 - `Provisioner.Trigger` forwards to the batcher.

Batcher:

- pkg/controllers/provisioning/batcher.go:33-48 - `Batcher[T]` (generic over pod UID). `Trigger`
  (line 52) idempotently arms a channel; `Wait` (line 72) implements the batch window: it opens on
  the first trigger, extends on each new distinct trigger by `BatchIdleDuration`, and closes at
  `BatchMaxDuration` (lines 89-108). Both durations come from operator options.

Entry point into scheduling:

- pkg/controllers/provisioning/provisioner.go:127-165 - `Provisioner.Reconcile`. Sequence:
  `batcher.Wait` (131) -> `cluster.Synced` check (137) -> `p.Schedule(ctx)` (142) ->
  `p.CreateNodeClaims(...)` (161) which launches the NodeClaims chosen by scheduling.
- pkg/controllers/provisioning/provisioner.go:350-458 - `Provisioner.Schedule` is the real batch
  entry. It snapshots the cluster (`p.cluster.DeepCopyNodes()`, line 363), collects the pod batch
  (`GetPendingPods`, line 366, plus pods on deleting nodes, line 375), builds a Scheduler
  (`p.NewScheduler`, line 396), and runs the simulation `s.Solve(timeoutCtx, pods)` (line 418)
  under a hardcoded 1-minute timeout (line 415).
- pkg/controllers/provisioning/provisioner.go:192-221 - `GetPendingPods` = provisionable pods from
  `nodeutils.GetProvisionablePods` (pkg/utils/node/node.go:171) filtered by `Validate`.

## 2. Where pods are simulated against nodes (the pods-times-nodes loop)

Scheduler construction: pkg/controllers/provisioning/scheduling/scheduler.go

- `NewScheduler` (scheduler.go:126-215). Notable steps:
  - Pre-filters each NodePool's instance types against the NodePool requirements to build
    `NodeClaimTemplate`s (scheduler.go:155-170, calling `filterInstanceTypesByRequirements`).
  - Computes per-template daemonset overhead: `getDaemonOverhead` (scheduler.go:177 and 964-975)
    and `getDaemonHostPortUsage` (scheduler.go:178 and 977-995).
  - Wraps every state node into an `ExistingNode`: `calculateExistingNodeClaims`
    (scheduler.go:793-803) -> `NewExistingNode` per node (scheduler.go:799).
- `Scheduler` struct (scheduler.go:231-260): holds `newNodeClaims []*NodeClaim` (inflight claims
  built this run), `existingNodes []*ExistingNode` (every node in the cluster snapshot),
  `nodeClaimTemplates []*NodeClaimTemplate` (one per NodePool), `topology *Topology`,
  `daemonOverhead map[*NodeClaimTemplate]corev1.ResourceList`, `remainingResources` (NodePool
  limits), `cachedPodData map[types.UID]*PodData`.

The solve loop: `Scheduler.Solve` (scheduler.go:441-520)

- Pods go into a `Queue` sorted by CPU then memory descending
  (scheduler.go:462; queue.go:37-43, byCPUAndMemoryDescending at queue.go:72).
- Loop: `q.Pop()` -> `trySchedule` (scheduler.go:522-553), which calls `add` and on failure relaxes
  pod preferences (`preferences.Relax`) and retries; failed pods are pushed back on the queue so
  the whole batch is re-attempted as long as progress is made.

The per-pod placement walk: `Scheduler.add` (scheduler.go:583-613). Order is exactly what the RFC
describes: existing nodes first, then inflight NodeClaims, then a new NodeClaim, ending early only
on a successful fit:

1. `addToExistingNode` (scheduler.go:615-657). THIS IS THE PODS-TIMES-NODES LOOP:
   `parallelizeUntil(s.numConcurrentReconciles, len(s.existingNodes), ...)` at scheduler.go:628
   evaluates `s.existingNodes[i].CanAdd(...)` (scheduler.go:634) for the pod against EVERY node in
   the cluster snapshot (with early-exit only once a lowest-index success is found). A pod that
   needs new capacity fails against all N nodes every solve cycle. `parallelizeUntil` itself is at
   scheduler.go:940-962; concurrency defaults to 1 and scales with CPU
   (provisioner.go:390 `NumConcurrentReconciles(ceil(CPURequests/1000))`).
2. `addToInflightNode` (scheduler.go:659-693) - the same pattern over `s.newNodeClaims`
   (claims opened earlier in this batch).
3. `addToNewNodeClaim` (scheduler.go:696-791) - the same pattern over `s.nodeClaimTemplates`
   (one per NodePool, weight-ordered); on success the new claim is appended to `s.newNodeClaims`
   and NodePool remaining resources are debited pessimistically (`subtractMax`, scheduler.go:1023).

Per-candidate fit checks:

- `ExistingNode` (existingnode.go:32-45; constructor NewExistingNode at existingnode.go:47-74).
  `ExistingNode.CanAdd` (existingnode.go:81-139): taints -> volume limits -> host ports ->
  resource fit (`resources.Fits(podData.Requests, n.remainingResources)`, existingnode.go:96) ->
  label-requirements compatibility (existingnode.go:100) -> topology
  (`topology.AddRequirements`, existingnode.go:159). The RFC's claim that an existing-node check
  is cheap-but-repeated is this function. `ExistingNode.Add` commits (existingnode.go:172-185).
- `NodeClaim` in the scheduling package (nodeclaim.go:43-63; NewNodeClaim at nodeclaim.go:86-112).
  `NodeClaim.CanAdd` (nodeclaim.go:117-156) -> `tryVolumeAlternative` (nodeclaim.go:162-240):
  taints -> host ports -> requirements intersection -> topology -> instance-type filtering
  (`filterInstanceTypesByRequirements`, nodeclaim.go:211) -> reserved-offering handling.
  `NodeClaim.Add` commits (nodeclaim.go:245-276). `FinalizeScheduling` (nodeclaim.go:350) strips
  the placeholder hostname requirement before launch.
- Results type (scheduler.go:282-287): `NewNodeClaims []*NodeClaim`,
  `ExistingNodes []*ExistingNode`, `PodErrors map[*corev1.Pod]error`.
  `scheduler.NodeClaim.ToNodeClaim()` (via embedded NodeClaimTemplate,
  nodeclaimtemplate.go:94-155) converts a scheduling NodeClaim into the v1.NodeClaim API object
  actually created by `Provisioner.Create` (provisioner.go:460-513).

## 3. Where the RFC's five inputs live today

(a) Pod requests + constraints

- `PodData` (scheduler.go:217-229): `Requests` (via `resources.RequestsForPods`,
  pkg/utils/resources/resources.go:30), `Requirements` / `StrictRequirements` (via
  `scheduling.NewPodRequirements` / `NewStrictPodRequirements`,
  pkg/scheduling/requirements.go:74/79), volume topology alternatives, DRA claims.
  Cached per pod UID in `Scheduler.cachedPodData`, refreshed on preference relaxation
  (`updateCachedPodData`, scheduler.go:555-581).

(b) NodePool specs

- Listed and filtered in `Provisioner.NewScheduler` (provisioner.go:268-289), ordered by weight
  (`nodepoolutils.OrderByWeight`, provisioner.go:289).
- Distilled into `NodeClaimTemplate` (nodeclaimtemplate.go:46-77): NodePool requirements +
  template labels become a `scheduling.Requirements`, plus taints, weight, and the pre-filtered
  `InstanceTypeOptions`.
- NodePool resource limits: `Scheduler.remainingResources` (scheduler.go:183-185, 236) and the
  create-time re-check in `Provisioner.Create` (provisioner.go:467).

(c) Prices and instance availability (instance types / offerings)

- Types: `cloudprovider.InstanceType` (pkg/cloudprovider/types.go:123-144) with `Requirements`,
  `Capacity`, `Overhead`, and `Offerings`. `Offering` (types.go:470) has `Price float64`
  (types.go:472), `Available bool`, and `Requirements` (zone, capacity-type).
- Fetched per NodePool via `cloudProvider.GetInstanceTypes` in `Provisioner.NewScheduler`
  (provisioner.go:291-310) into `map[string][]*cloudprovider.InstanceType`.
- Price helpers: `InstanceTypes.OrderByPrice(reqs)` (types.go:336-355, cheapest compatible
  available offering per instance type), `Offerings.Available()` (types.go:547),
  `Offerings.Compatible(reqs)` (types.go:554), `Offerings.Cheapest()` (types.go:571),
  `Offerings.WorstLaunchPrice(reqs)` (types.go:587), `InstanceType.OfferingPrice(zone, capacityType)`
  (types.go:298).

(d) Daemonset footprint / overhead

- Daemonset pod templates: `Provisioner.getDaemonSetPods` (provisioner.go:530-555), using
  `cluster.GetDaemonSetPod` (pkg/controllers/state/cluster.go:657) or
  `daemonset.PodForDaemonSet` (pkg/utils/daemonset/daemonset.go:24).
- Per-NodePool footprint for NEW nodes: `getDaemonOverhead` (scheduler.go:964-975) - sums requests
  of daemon pods compatible with each NodeClaimTemplate (`isDaemonPodCompatible`,
  scheduler.go:998-1017). Seeded into every new NodeClaim as
  `template.Spec.Resources.Requests = daemonResources` (nodeclaim.go:101).
- Per-EXISTING-node footprint: `NewExistingNode` subtracts not-yet-scheduled daemon resources from
  node available (existingnode.go:50-66), with the compatible-daemon filter at scheduler.go:806-833.

(e) Topology domain state (spread / affinity / anti-affinity domains)

- `Topology` (topology.go:47-66): `topologyGroups` and `inverseTopologyGroups`
  (hash -> *TopologyGroup), `domainGroups map[string]TopologyDomainGroup` (the universe of domains
  per key, derived from NodePools x instance types in `buildDomainGroups`, topology.go:105-143),
  `excludedPods` (batch pods excluded from counts).
- `NewTopology` (topology.go:68-103) does the precompute the RFC wants to reuse: it walks
  the pods, creates TopologyGroups, and calls `countDomains` (topology.go:361-459), which counts
  matching EXISTING pods per domain by listing pods from the API server and reading their nodes'
  labels. Inverse anti-affinity of running pods: `updateInverseAffinities` (topology.go:313) via
  `cluster.ForPodsWithAntiAffinity`.
- `TopologyGroup` (topologygroup.go:56-73): exactly the counts the RFC describes -
  `domains map[string]int32` (pod count per domain) + `emptyDomains` + `owners`. Skew math in
  `nextDomainTopologySpread` (topologygroup.go:229) and `domainMinCount` (topologygroup.go:300);
  affinity/anti-affinity domain selection at topologygroup.go:324/404.
- Runtime maintenance during a solve: `Topology.AddRequirements` (topology.go:226-248) tightens a
  candidate's requirements from the groups; `Topology.Record` (topology.go:197-220) bumps counts
  when a pod is committed; `Register/Unregister` (topology.go:284-309) add/remove domains
  (e.g. each new NodeClaim hostname, nodeclaim.go:251).
- NOTE for the RFC: topology state is rebuilt from scratch every batch (NewTopology is called per
  Schedule, provisioner.go:321) - there is no continuously-maintained count store today. The
  "watch node labels and pod locations" component would be new; `countDomains` is the batch-time
  equivalent.

## 4. Envtest infrastructure

pkg/test/environment.go

- `Environment` (environment.go:44-53) wraps `envtest.Environment` with a `client.Client`, a
  `FakeClock`, and a kubernetes.Interface. `NewEnvironment` (environment.go:141-201) starts a real
  API server (K8S_VERSION defaulting to 1.36.x), installs CRDs passed via `WithCRDs`
  (environment.go:62), and optionally builds a cache-syncing client for field indexers
  (`WithFieldIndexers`, environment.go:72).

How suites bootstrap (pattern is identical across the tree):

- pkg/controllers/provisioning/suite_test.go:77-90 (BeforeSuite):
  `env = test.NewEnvironment(test.WithCRDs(apis.CRDs...), test.WithCRDs(v1alpha1.CRDs...))`,
  `cloudProvider = fake.NewCloudProvider()`,
  `cluster = state.NewCluster(env.Clock, env.Client, cloudProvider)`,
  `prov = provisioning.NewProvisioner(env.Client, recorder, cloudProvider, cluster, env.Clock, deviceallocation.NewController(env.Client))`,
  plus informer controllers (`informer.NewNodeController`, `informer.NewDaemonSetController`).
- pkg/controllers/provisioning/scheduling/suite_test.go:91-103 does the same for the scheduling
  suite. (Note scheduling/suite_test.go:2046 also shows building a Provisioner against a pure
  fake client - envtest is not mandatory for the Scheduler itself.)

The fake cloudprovider (used by these suites and the benchmark):

- pkg/cloudprovider/fake/cloudprovider.go + instancetype.go. `fake.InstanceTypes(total)`
  (instancetype.go:411-425) generates instance types with linearly growing CPU/memory/pods;
  `PriceFromResources` (instancetype.go:426-439) prices them (0.1/cpu, 0.1/GB), so prices are real
  and summable in tests.

The kwok cloudprovider (a more realistic instance catalog):

- kwok/cloudprovider/helpers.go:70-92 `ConstructInstanceTypes` parses the embedded
  kwok/cloudprovider/instance_types.json (144 instance types; per-offering `Price`,
  `Available`, zone and capacity-type requirements) into `[]*cloudprovider.InstanceType`
  (`newInstanceType`, helpers.go:156-214). Overridable via `--instance-types-file-path`.
- kwok/cloudprovider/cloudprovider.go:47-57 `NewCloudProvider(ctx, kubeClient, instanceTypes)`;
  `GetInstanceTypes` returns the fixed catalog (cloudprovider.go:143-145). `Create`
  (cloudprovider.go:59-87) turns a NodeClaim into a real Node object; `toNode`
  (cloudprovider.go:186-236) picks the CHEAPEST compatible available offering across the
  NodeClaim's instance-type requirement (cloudprovider.go:199-215) - i.e. kwok already implements
  "launch = cheapest offering", useful for pricing assertions end to end.
- There is no envtest suite in kwok/cloudprovider itself; unit/perf tests all use
  pkg/cloudprovider/fake. Kwok is wired up only in kwok/main.go for a live cluster.

How a test creates NodePools/nodes/pods and invokes scheduling (pkg/test + pkg/test/expectations):

- Object factories: `test.NodePool` (pkg/test/nodepool.go:35), `test.Pod` (pkg/test/pods.go:88),
  `test.UnschedulablePod` (pods.go:231), `test.Node`, `test.NodeClaim`, etc.
- `ExpectApplied` (pkg/test/expectations/expectations.go:148) persists objects to envtest.
- `ExpectProvisioned` (expectations.go:324) / `ExpectProvisionedNoBinding` (expectations.go:351):
  applies the pods, calls `provisioner.Schedule(ctx)` directly (expectations.go:364), then
  `provisioner.Create` per NodeClaim (expectations.go:372) and manually binds pods.
  `ExpectProvisionedResults` (expectations.go:402) returns the raw `scheduling.Results` - handy
  for a POC that wants to inspect/price results without launching.
- `ExpectScheduled` (expectations.go:127) asserts a pod got a node;
  `ExpectMakeNodesInitialized` (expectations.go:783) matures nodes so they count as existing
  capacity in the next scheduling round.

## 5. Machinery reusable for building greenfield NodeClaims without the full simulation

All of the following is in importable packages; the catch is that several key helpers are
package-private to `pkg/controllers/provisioning/scheduling` ("scheduling pkg" below).

- Instance-type filtering: `filterInstanceTypesByRequirements(instanceTypes, requirements,
  podRequests, daemonRequests, totalRequests, relaxMinValues)` - nodeclaim.go:477-535, with
  `compatible` (nodeclaim.go:537) and `fits` (nodeclaim.go:541, checks allocatable per offering
  group and offering compatibility). UNEXPORTED - needs export or extraction. It is pure: no
  cluster state, exactly what a greenfield builder needs.
- Instance-type ordering by price: `InstanceTypes.OrderByPrice(reqs)`
  (pkg/cloudprovider/types.go:336) and `Offerings.Cheapest()` (types.go:571). EXPORTED.
- Requirements intersection: pkg/scheduling/requirements.go - `Requirements.Add` (line 133,
  intersects on insert), `Compatible` (181), `IsCompatible` (176), `Intersects` (254),
  `NewPodRequirements` (74). All EXPORTED; this is the workhorse and is fully reusable.
- Daemonset overhead: `getDaemonOverhead` (scheduler.go:964) and `isDaemonPodCompatible`
  (scheduler.go:998). UNEXPORTED but tiny and self-contained (inputs: []*NodeClaimTemplate,
  []*corev1.Pod); trivial to export or copy. `NewNodeClaimTemplate` (nodeclaimtemplate.go:57) and
  `NodeClaimTemplate.ToNodeClaim` (nodeclaimtemplate.go:94) ARE exported, so the
  NodePool -> template -> v1.NodeClaim pipeline is directly reusable.
- Topology domain tracking: `NewTopology` (topology.go:68), `Topology.AddRequirements`
  (topology.go:226), `Topology.Record` (topology.go:197), `NewTopologyGroup` (topologygroup.go:75),
  `TopologyDomainGroup` (topologydomaingroup.go:30). All EXPORTED. `NewTopology` does the
  domain-count precompute (topology.go:184 `countDomains`) once per batch; a greenfield builder
  can consume a `*Topology` as its input (5) without touching existing nodes otherwise -
  note `countDomains` does list pods/nodes through the kube client, which is the part the RFC
  proposes replacing with maintained counts.
- Binpacking commit path: `NewNodeClaim` (nodeclaim.go:86), `NodeClaim.CanAdd` (nodeclaim.go:117),
  `NodeClaim.Add` (nodeclaim.go:245), `FinalizeScheduling` (nodeclaim.go:350) are all EXPORTED.
  A minimal greenfield builder can literally be: build templates + topology + daemon overhead,
  then run only step 3 of `Scheduler.add` (addToInflightNode over its own open claims +
  addToNewNodeClaim) and never construct `existingNodes`. Equivalently: call
  `scheduling.NewScheduler` with `stateNodes = nil` - the benchmark already does exactly this
  (scheduling_benchmark_test.go:242-256 passes nil stateNodes), proving the simulation degrades
  gracefully into a greenfield-only computation with zero code changes. That is the cheapest
  possible POC baseline; a dedicated builder then replaces the greedy per-pod loop.

## 6. Recommended integration seam for the POC

Option A - library seam in the scheduling package (recommended):

Both answers are computed from the same inputs that `Provisioner.NewScheduler` already assembles
(provisioner.go:261-348): `pods`, `nodePools`, `instanceTypes`, `topology`, `daemonSetPods`,
`stateNodes`. Define the greenfield builder as a sibling of `NewScheduler`:

    greenfield := scheduling.NewGreenfieldBuilder(nodePools, topology, instanceTypes, daemonSetPods, ...)
    incumbent  := greenfield.Build(ctx, pods)     // phase one
    results, _ := scheduler.Solve(ctx, pods)      // existing full simulation

Tests construct both from identical inputs the way scheduling_benchmark_test.go:217-257
(`setupScheduler`) does today - fake client, `state.NewCluster`, `scheduling.NewTopology`, and
`scheduling.NewScheduler`, optionally with N synthetic `stateNodes` to model cluster size. Timing
each side is a `time.Since` around two function calls; pricing is a sum over each side's claims
(see below). No envtest needed for the perf comparison, and the benchmark harness is reusable
almost verbatim.
Tradeoffs: + zero controller changes, + trivially benchmarkable, + isolates the comparison from
API-server noise; - does not exercise `GetPendingPods`/batcher/create path, so it validates the
computation, not the plumbing. CAUTION: `Topology` and `ReservationManager` are stateful within a
solve (Record/Reserve mutate them) - build a fresh Topology per side or run greenfield first,
since it must not consume simulation state anyway.

Option B - controller seam inside Provisioner.Schedule:

Insert phase one between building the scheduler and `s.Solve` (provisioner.go:396-418): compute
greenfield claims from the same `pods`, run `s.Solve` as phase two under the improvement budget,
compare costs, return the winner in `scheduler.Results`. Tests drive it through
`ExpectProvisionedResults` (expectations.go:402) against envtest.
Tradeoffs: + end-to-end, exercises snapshotting, NodePool listing, and the create path; + it is
where the shipped feature must eventually live; - envtest makes timing noisy, seeding thousands
of nodes into envtest is slow, and every experiment pays controller overhead.

Recommendation: Option A for the POC, with one envtest smoke test through Option B's call site
(`ExpectProvisionedResults`) to prove the greenfield claims are launchable v1.NodeClaims. The
benchmark file gives the perf harness for free; add node-count sweeps (see below).

## Pricing the scheduler's results

There is no single "chosen offering" recorded on a scheduling result. A `scheduling.NodeClaim`
carries `InstanceTypeOptions` (a set); the concrete instance is chosen at launch by the
cloudprovider (kwok picks the cheapest compatible available offering, cloudprovider.go:199-215).
The standard cost proxy, used by consolidation as well, is cheapest-possible launch price:

    for _, nc := range results.NewNodeClaims {
        it := nc.InstanceTypeOptions.OrderByPrice(nc.Requirements)[0]           // types.go:336
        price := it.Offerings.Available().Compatible(nc.Requirements).Cheapest().Price  // types.go:547,554,571
        total += price
    }

`RemoveInstanceTypeOptionsByPriceAndMinValues` (nodeclaim.go:374) and `WorstLaunchPrice`
(types.go:587) show the same pattern used in-tree for price ceilings. With the fake provider,
prices come from `PriceFromResources` (fake/instancetype.go:426); with kwok, from
instance_types.json. So yes: tests can sum a well-defined price for both the greenfield answer
and the simulation answer and compare.

## Existing benchmarks

pkg/controllers/provisioning/scheduling/scheduling_benchmark_test.go (build tag
`test_performance`, line 1; run with `go test -tags=test_performance -run=XXX -bench=.`):

- `BenchmarkScheduling1` ... `BenchmarkScheduling20000` (lines 77-103): pod-count sweep
  {1, 50, 100, 500, 1000, 2000, 5000, 10000, 20000} of diverse pods (generic, zonal/hostname
  spread, affinity, anti-affinity - makeDiversePods, line 259) against 400 fake instance types
  and ONE NodePool. `TestSchedulingProfile` (line 114) does the same sweep with pprof CPU/heap
  profiles and reports pods/sec and nodes.
- `benchmarkScheduler` (line 162) calls `scheduler.Solve` directly; `setupScheduler` (line 217)
  builds Topology + Scheduler with a fake client and `stateNodes = nil`.
- IMPORTANT GAP for the RFC: the sweep is over PENDING POD count only. `stateNodes` is always nil,
  so the existing benchmark never measures the pods-times-existing-nodes term the RFC targets.
  The POC perf plan needs a second axis: seed M `state.StateNode`s (initialized nodes with
  capacity) and sweep M in {0, 100, 1000, 5000, ...} x pending pods, then compare full simulation
  vs greenfield builder. The harness (fake client, fake.InstanceTypes, test.Pod generators,
  benchstat workflow documented at lines 64-76) is all reusable; only StateNode seeding is new -
  `calculateExistingNodeClaims` (scheduler.go:793) consumes whatever stateNodes are passed to
  `NewScheduler`.

## Seam decision (2026-07-04, closes bead gfp-goal-t8s.4)

Adopted: Option A from section 6. The greenfield builder is a sibling invocation of
scheduling.NewScheduler with stateNodes=nil, fed the same inputs Provisioner.NewScheduler
assembles (provisioner.go:261-348) restricted to claim-eligible pods, with a fresh Topology
per side (Topology is mutated during Solve). Rationale: every constructor needed is exported
and scheduling_benchmark_test.go:217-257 already proves the pattern outside the provisioner;
no piecemeal extraction of unexported helpers is needed for the core path (exporting daemonset
overhead helpers is tracked as a small explicit diff). Tests and benchmarks call both paths on
the same state; the production Provisioner is untouched for the POC.
