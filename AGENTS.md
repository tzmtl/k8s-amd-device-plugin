# Fork Objective

This fork exists to add simple AMD GPU sharing to the upstream
`k8s-amd-device-plugin`.

The feature is controlled by the device plugin `-replica` flag. With
`-replica=N`, each detected physical AMD GPU is advertised to Kubernetes as
`N` logical device plugin IDs. For example, one physical GPU with
`-replica=8` should make the node report eight allocatable `amd.com/gpu`
devices, allowing multiple pods that request `amd.com/gpu: 1` to be scheduled
onto the same physical GPU.

# Feature Semantics

- `replica=1` must preserve upstream behavior.
- A replica is a scheduler-visible logical slot only. It does not create GPU
  memory isolation, compute isolation, MIG-style partitioning, cgroup limits,
  or per-process throttling.
- Replicas of the same physical GPU intentionally map to the same host device
  nodes, including `/dev/kfd` and the physical GPU's `/dev/dri/card*` and
  `/dev/dri/renderD*` devices.
- Health is physical-device health. If a physical GPU is unhealthy, every
  logical replica for that GPU should be reported unhealthy.
- The primary expected workload shape is many pods or containers each
  requesting one logical AMD GPU. Multi-GPU requests are supported by spreading
  logical replicas across distinct physical GPUs first, then using additional
  replicas only when the request exceeds the number of available physical GPUs.

# Maintenance Notes

- Keep the logical device ID scheme stable unless every consumer is updated.
  The current scheme appends a replica index to the physical device ID:
  `<physical-device-id>-<replica-index>`.
- Any change to replica ID generation must be reflected in health normalization,
  allocation, preferred allocation, tests, Helm values, and user-facing docs.
- Allocation must always translate logical replica IDs back to the physical AMD
  GPU data before returning device specs to kubelet.
- Preferred allocation is topology-aware in upstream code and should receive
  physical device IDs, not replica IDs. Replica-aware code should group logical
  IDs by physical GPU, call the topology policy for physical choices when it is
  available, then expand the result back to logical replica IDs.
- Keep Helm and manifest defaults at `replica: 1` unless intentionally changing
  backwards compatibility.
- Document GPU sharing honestly as oversubscription. Do not describe it as
  isolation, quota enforcement, or hardware partitioning.

# Version And Branch Management

- Treat upstream release tags as immutable bases. This fork should be described
  as "official AMD device plugin version X plus GPU-sharing patch version Y".
- Keep `master` aligned with the selected upstream official release tag. Do not
  place downstream GPU-sharing commits on `master`.
- Keep `gpu-sharing` as the active downstream feature branch. It should be
  rebased onto the selected upstream release tag, not merged with upstream
  `master`.
- Keep the downstream feature branch as a single logical patch commit on top of
  the upstream release tag when practical. Squash fixup/review commits before
  publishing the maintained branch.
- When updating to a new upstream release:
  1. `git fetch upstream --tags`
  2. verify the target tag exists, for example `v1.31.0.10`
  3. move `master` to that upstream tag and push it to the fork
  4. rebase or recreate `gpu-sharing` on that tag
  5. squash the downstream changes into one feature commit
  6. validate with tests/builds
  7. update `origin/gpu-sharing` with `git push --force-with-lease`
- Prefer `--force-with-lease` over plain force-push when replacing the remote
  feature branch history. The intention is to remove outdated downstream
  history while protecting against overwriting someone else's new push.
- For released downstream builds, create an optional frozen release branch and
  annotated tag, for example:
  `release/v1.31.0.10-gpushare.1` and `v1.31.0.10-gpushare.1`.
- Docker image tags should include both the upstream base version and the
  downstream feature version. Use tags such as:
  `1.31.0.10-gpushare.1`, `rhubi-1.31.0.10-gpushare.1`, and
  `labeller-1.31.0.10-gpushare.1`.
- Do not use SemVer build metadata with `+` in Docker tags; use hyphenated tag
  suffixes instead.

# Useful Validation

- Run allocator tests after topology or preferred-allocation changes:
  `GOCACHE=/private/tmp/k8s-amd-device-plugin-gocache go test ./internal/pkg/allocator`
- Full plugin and command tests require local `pkg-config` entries for
  `libdrm`, `libdrm_amdgpu`, and `hwloc`.
