# PR draft: deterministic topology domain tie-breaking

Branch: topologygroup-deterministic-tiebreak (single commit on mainline,
also first commit of greenfield-provisioning-poc). NOT pushed; pushing and
opening the PR needs James's go-ahead.

## Title

fix: make topology domain tie-breaking deterministic per group

## Description

**Problem.** Two places in TopologyGroup resolve domain choices from Go map
iteration order. nextDomainAffinity bootstraps a self-selecting affinity
group by taking the first viable domain out of the domains map, and
nextDomainTopologySpread breaks equal-count ties by first-seen order. Map
iteration order varies run to run, so two solves of the identical problem
can anchor different domains. Anywhere domain choice has a price or
capacity consequence (zone-dependent offering prices, zonal capacity), the
scheduler's answer for the same inputs is not reproducible: in a property
test with zone-priced offerings and one zone-scoped required affinity pod,
identical inputs produced cost differences above 50 percent at roughly a
1-in-5 rate. Nondeterminism also frustrates any consumer that compares two
scheduling answers for the same batch.

**Why not lexicographic.** Sorting domains and picking the first makes
solves deterministic but biases every group toward the same first-sorting
domain: N deployments with zonal spread would all place their remainder
pod in the alphabetically-first zone, every cycle, concentrating capacity
fleet-wide where the previous behavior was balanced in expectation.

**Fix.** Rank domains by a hash seeded from the group's logical identity
(key, type, namespaces, selector). Identical solves of the same constraint
rank domains identically; distinct groups rank them differently, so the
de-facto balance across groups is preserved. maxSkew enforcement is
unchanged: the rank only selects among domains that already pass the skew
check.

**Evidence.** The full scheduling suite (367 specs) and provisioning suite
pass. The property test that exposed the nondeterminism went from ~1-in-5
failures to 0 across repeated runs (10/10, then 5/5 at 400 checks per run).

## Notes for review

- rankSeed is computed once in NewTopologyGroup via hashstructure over
  (Type, Key, namespaces, rawSelector); domainRank is fnv64a(seed, domain).
- The affinity bootstrap keeps its two-loop structure (intersection first,
  then any viable domain); only the iteration order changed.
- A price-aware anchor (pick the cheapest viable zone rather than a hashed
  one) would be strictly better provisioning and is left as follow-up; it
  needs price data TopologyGroup does not have today.
