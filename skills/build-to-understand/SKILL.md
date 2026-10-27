---
name: build-to-understand
description: Use whenever the user wants a systems concept explained in depth or wants to build a system from scratch to understand it — databases, vector search, storage engines, WAL, replication, sharding, consensus/Raft, queues, schedulers, caches, compilers, distributed systems generally. Triggers on "explain", "teach me", "how does X work", "why do we need X", "what are the trade-offs", "design X from scratch", "let's build X", and on any deep-dive or interview-prep question about how a real system works. Explains motivation-first — the failure a mechanism prevents, how real systems solved it and where they were publicly wrong — and, when building, scopes to the minimal honest build in layers.
---

# Build to understand

The goal is conceptual mastery, not a product. The output is a small system plus
the reasoning that produced it. A learner who can run the code but cannot say
what breaks without it has learned nothing.

**Governing rule: never build a mechanism before you can state the failure it
prevents.** If the failure cannot be stated, the mechanism is cargo cult — cut
it or postpone it until the failure appears.

## Explain-only mode

Most requests are "explain this", not "build this". Skip to Phase 4 output and
use the **concept structure** directly. Every explanation answers, in order:

1. **The problem.** What breaks or is impossible without this. State the failure
   concretely before naming the mechanism. If the mechanism is introduced first,
   the explanation has already failed.
2. **How it works.** The mechanism, precisely — ordering, invariants, what is
   durable when.
3. **Parameters and knobs**, and what each one trades away.
4. **How real systems do it.** Mandatory. Name 3–6 systems, diff them, and say
   where they were publicly wrong (Jepsen, post-mortems, reversals). Convergent
   design across independent systems is evidence; divergence is the interesting
   trade-off.
5. **Complexity and cost** — time, memory, network, disk.
6. **Failure modes**, including the ones this mechanism does *not* cover.
7. **How to test it** — the fault you would inject to prove it works.
8. **The minimal version.** Mandatory. The smallest thing that demonstrates the
   idea, and what it deliberately omits. This is the bridge to building it.
9. **Open questions** — what is still genuinely unsettled.

Then offer the build. If the concept is worth understanding, an afternoon
implementing the minimal version teaches more than a second explanation, and
the ladder below says where it fits.

## Phase 0 — Interrogate the need

Do this before any design. Ask, and get real answers:

1. **Why this system?** Learning depth, interview fluency, a real workload? These
   produce different builds.
2. **What is "done"?** "I can explain every trade-off" and "it survives a chaos
   test" are different finish lines.
3. **Budget.** Hours, honestly. This is the single largest constraint and the one
   users most often refuse to state.
4. **Which concepts are the point?** In a distributed system the answer is
   usually durability + replication + failover, not the domain algorithm.
5. **Real data or synthetic?** No real workload means no cluster, no Docker, no
   ops surface — an in-process simulation teaches the same lessons far cheaper.

Then **challenge the scope out loud**. Learning projects are almost always
over-scoped: production deployment, real datasets, benchmarks against real
systems, and a full feature matrix are the first things to cut. Say plainly what
you are cutting and what is lost. Do not silently downscope.

Resolve budget-vs-ambition by redefining *done per milestone*, not by deleting
milestones. Three tiers:

- **BUILD** — implement, test, break, measure. Reserve for the concepts that are
  the point.
- **SPIKE** — a throwaway prototype or simulation that proves the idea, then
  written up. No production shape.
- **STUDY** — a written analysis of how real systems do it, with a stated reason
  for not building it.

Every milestone gets a tier before any code exists.

## Phase 1 — Research real systems

Learning value comes from contact with what real systems actually did, including
their mistakes. Three lenses per topic:

- **Breadth** — how do 4–6 real systems solve this? Name them and diff them.
- **Depth** — one primary source read properly: a paper, a design doc, the actual
  source, an RFC.
- **Adversarial** — *where were they publicly wrong?* Jepsen reports, post-mortems,
  data-loss issues, documented reversals, "we removed this in v2" changelogs.

The adversarial lens is the one that converts trivia into understanding. A
mechanism's justification is a specific historical failure: Kafka's
`min.insync.replicas` exists because acks=all with a shrunk ISR silently lost
data; fencing tokens exist because leases alone do not stop a paused process.

Two heuristics that repeatedly pay off:

- **Convergent design is evidence.** When independent systems land on the same
  answer, adopt it and say why. When they diverge, that is the interesting
  trade-off — document both.
- **Measure before claiming.** No performance statement without a number from
  your own machine.

Record research as durable notes keyed to concepts, not to milestones. The notes
outlive the code.

## Phase 2 — Scope the minimal build

Write the plan down before coding. It must contain:

- **Requirements** split into functional / non-functional, and separately
  **explicit non-goals**.
- **Assumptions**, each labelled — measured, cited, or arbitrary. No silent
  numbers.
- **The failure model**: what failures are handled, and what is explicitly out
  of scope.
- **The data model** with a justification per field.
- **The milestone ladder**, each with its tier, its acceptance criteria, and —
  mandatory — **the failure it exists to fix**.

Prefer in-process simulation over deployment. A seeded, virtual-clock simulation
of five nodes with injected drops and partitions teaches more about consensus in
an afternoon than a Docker Compose cluster teaches in a week, and it is
reproducible from a seed.

Keep dependencies near zero. Every imported library is a concept not learned.

## Phase 3 — The layer loop

For each milestone, in order, and **stop at the end of each one**:

1. **State the failure.** The concrete, nameable thing that breaks without this
   layer. If you cannot, do not build it.
2. **Build the broken version first when the gap is the lesson.** Ad-hoc failover
   before Raft; single-controller split brain before consensus; fsync-per-write
   before group commit. Feeling the failure is the pedagogy — a mechanism
   introduced before its failure is memorised, not understood.
3. **Design**: interfaces, data structures, wire format, ordering guarantees.
   Be precise about what is durable when.
4. **Implement the smallest thing that works.** No premature abstraction. Small
   packages, explicit errors, no hidden goroutines, clear ownership of mutable
   state.
5. **Test correctness**, then **test failure** — see `references/failure-testing.md`.
6. **Measure**, if the milestone makes a performance claim.
7. **Review**: correctness, concurrency, durability, distributed behaviour,
   performance, API, coverage. Name the weaknesses you are choosing to live
   with.
8. **Write the concept up** while it is fresh: the problem, how it works, how
   real systems do it, failure modes, how to test it, the minimal version.
9. **Acceptance criteria met? Then stop and check in** before the next layer.

The layers themselves, for distributed systems, are a fixed ladder — see
`references/distributed-systems-ladder.md`. Do not reorder it. Each rung exists
because the rung below it has a specific hole.

## Phase 4 — Deliver

The deliverable is three things, not one:

1. **The code** — runnable, tested, with a README that states the architecture,
   consistency model, failure model, measured numbers, and known limitations.
2. **The concept write-ups** — one file per concept, in a consistent structure.
   These are what survive the project.
3. **A demo that shows the failure and the fix** — a crash that loses nothing, a
   partition that refuses writes, a promotion that fences the old leader. A
   passing test suite is not a demo; the demo is what makes the lesson visible,
   and in practice it finds bugs the tests missed.

## Anti-patterns

- Building a mechanism whose failure you cannot state.
- Production shape (Docker, gRPC, service discovery, TLS) in a learning project
  with no users — it costs weeks and teaches operations, not concepts.
- Importing the hard part (a Raft library, an ANN library) and calling it done.
- Over-analysing. When the user says start building, stop researching and build.
- Tests that pass vacuously — see the failure-testing reference.
- Claiming a trade-off you did not measure.
