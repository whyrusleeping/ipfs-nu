# Product brief

[Spec overview](../spec.md) · [Operational contracts](operations.md) · [Migration](migration.md) · [Validation](validation.md)

## Promise

**Recover the magic of early IPFS, and make it dependable for real users from day one.**
Add files, give someone a link, fetch them on another machine, open them in a browser.
The easy path is the product, not a demo that users must replace with another stack once
availability, history or administration matters.

Nu is one Go daemon and CLI with integrated verification, storage, versioning and replication.
An individual can share a file or mirror a dataset; a team can preserve versions across its
machines and optionally publish them for independent mirrors. These use the same objects,
transfer machinery, API and status vocabulary. Clusters are built in, not a required sidecar.

Success requires both delight and trust. Archival must not turn the first share into an
administration exercise; sharing must not disguise incomplete replication as durability.
Nu should explain what it is doing without requiring users to learn NAT types, repository
proofs, DAG layouts or placement algorithms.

## Audience and jobs

The initial institutional audience is teams maintaining versioned datasets and archives,
alongside individuals publishing and mirroring datasets. Start with smaller deployments
shaped like Internet Archive or Wikipedia workflows, not promises of their largest scale.
Recruit concrete pilot users and workloads before freezing capacity and performance claims.
Assume substantial parallel engineering capacity; broad acceptance, not only a research
prototype, is the goal.

| User | Job | Successful outcome |
|---|---|---|
| Individual | Share a file/tree or small site | Add, share, retrieve and browse without an account or network configuration on supported networks |
| Publisher | Publish updates from a workstation | A stable public name and an exact readiness result before the workstation sleeps |
| Mirror operator | Follow a changing dataset | A consistent served snapshot, visible lag, bounded resources and resumable updates |
| Archive team | Preserve selected versions across machines | Explicit retention, policy-confirmed copies, historical retrieval and tested recovery |
| Collaborating operators | Share a fast-growing item collection, each node choosing what to hold | Local metadata rules, rolling holdings, visible copies per item and rescue of rare items ([D22](collections.md)) |
| Administrator | Keep the archive healthy | Understand and fix capacity, failures, authority loss and upgrades without assembling another control plane |

Linux servers and Linux/macOS workstations are initial targets. Trusted cluster members and
OS/disk protections are sufficient initially; per-dataset read permissions and content
confidentiality from members are not initial promises. Public content commands need no
atproto account. A local/private archive must not need a PDS, public relay or DHT.

## Progressive experience

### First magic

Install one binary, initialize/start the daemon, add a file or directory and share a link.
The recipient retrieves verified content; both can inspect a tree in the local browser.
Public HTTPS links are available when an explicitly public serving seed/gateway is present;
Nu must not silently upload or expose members-only content to create a browser link.
A gateway reader need not install Nu. Initial hinted/LAN retrieval must not wait for global
provider discovery or advanced hard-NAT optimization.

Default public announcements are explained concisely on the public path. Once a user has
chosen that mode, repeat operations must not require ritual confirmations. Private/isolated
mode suppresses public defaults. Unannounced is not private or access-controlled.

### Keep it available

Register an always-on seed, publish a name and wait for that exact target to be fully held
by the chosen seed. Show publication and transfer separately. The user may sleep only after
the requested readiness condition succeeds; `publish` by itself is not that condition.
Opening the same name from a fresh machine must still work while the publisher sleeps.
Names require atproto login. Account-free availability uses a cluster client/workstation and
storage seed with a fixed-CID dataset/pin and exact-content wait. For outsider retrieval,
use public mode and explicitly enable public serving (`announce`), then share CID/provider
hints or the opted-in seed's HTTPS link. Private/isolated seeds serve members only; unannounced
member-only pins do not become public through a share link. Neither workflow means the
project hosts content indefinitely.

### Keep versions

Create a dataset with a visible `mirror` or `archive` preset (`nu dataset create`, with no
cluster setup; a follow is a mirror dataset). Mirror tracks a current usable
version plus its update safeguard; archive enables explicit retention and historical access.
Neither preset requires understanding how MSTs or signed repositories work. A one-storage-
member archive preset explicitly selects `replicas: 1` with no spread requirement and displays “ready at one copy,”
not redundant storage. Team clusters default to two copies; an explicit higher policy is
never silently weakened. No additional daemon or networking setup is needed for local history.

Import a tree, inspect changes and required headroom, retain a version, retrieve its exact
snapshot, and restore it. Adding machines expands placement and repair inside the same
product. Cluster keys/PeerIds are distinct from optional public atproto identity.

### Operate and recover

Join/tag storage members, wait for policy readiness, update and drain without intentionally
releasing required copies early. Status shows blockers and an actionable next step.
Exercise disk pressure, node failure, authority backup/restore and a clean-machine recovery
before recommending production storage. Advanced topology, history schedules, bundles and
network tuning are progressive options, not prerequisites for a first successful journey.

## Product vocabulary

| Concept | Meaning |
|---|---|
| CID | Immutable content identity, not a promise someone still holds the bytes |
| Public name | A current pointer, not a public archive of releases |
| Follow | Maintain a current mirror; lazy mode is not an offline copy |
| Archive dataset | Preserve selected snapshots under an explicit policy |
| Version | An immutable snapshot with separately reported content availability |
| Replicated | The specified version satisfies the configured policy using fresh confirmations; not immunity from later failures |
| Published | A public name was written; not proof of replication or perpetual hosting |

The [operational contracts](operations.md) define the states and failure behavior behind
these words. Never use a successful metadata write as shorthand for safely stored content.

## Keep the system maintainable

- One daemon, common store, common byte/tree transfer layers and versioned local API.
- Reusable packages and negotiated extensions, but no feature requires users to assemble
  pinning sidecars, separate sync services or a parallel administrative datastore.
- Own focused libp2p implementation over stock QUIC/TLS; deployed libp2p wire compatibility
  is mandatory. Iroh/Tailscale inform behavior, not protocol substitution.
- atproto supplies identity and public signed names, not file-serving/PDS infrastructure.
- Compatibility statements distinguish transport, content, API and migration. Nu initially
  does not exchange UnixFS/Bitswap content; [one-way migration](migration.md) is a planned
  adoption path, not a promise that old CIDs or Kubo APIs become Nu-native.
- Expose new knobs only when measured workloads need them. Reuse the existing data and
  transfer model rather than create a protocol for each institutional requirement.

## Boundaries

Nu is not initially a POSIX filesystem, live collaborative database, general backup agent,
or project-operated perpetual preservation service. Version retention alone is not a
backup guarantee. Teams control their deployment and need independent recovery material.
Linearizable operations, byzantine-member durability, erasure coding, multi-cluster federation,
incentives, anonymity and member-confidential encryption are not initial promises.

## Release expectations

A developer preview can have declared limitations. A broadly recommended v1 must combine
simple sharing/browser access with useful versioned replication, tested operations and
Linux/macOS workstation support. Public reach over HTTPS-compatible paths and real default
infrastructure are release work, not indefinite future polish. IPFS migration follows the
first useful preview and gates recommendations to move existing IPFS deployments.

Public connectivity and trusted archival are parallel tracks sharing the foundations.
Neither the full NAT optimizer nor PB-scale benchmarks gate the first useful archive;
archival administration does not gate a first hinted share. Exit criteria and published
claims are owned by [milestones](../spec.md#milestones) and [validation](validation.md).
