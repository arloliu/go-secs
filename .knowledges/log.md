# Log

## 2026-08-05
* **Creation**: bundle bootstrapped at `bc97919` — seven unit skeletons, `hsms` / `hsmsss` / `secs2` seeded.
* **Deprecation**: the first seeding of `hsms` / `hsmsss` / `secs2` was replaced wholesale. Three broad entries (`linktest-suppression`, `send-path-reply-correlation`, `decode-ownership`) failed the novelty gate and the one-mechanic rule; two carried factual errors inherited from the doc comments they restated.
* **Creation**: [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) stamp state, per-generation reset, and the reducer's real trigger.
* **Creation**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) pre-write gate, write-boundary re-check, counted chokepoint.
* **Creation**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) lifecycle events excluded from the transaction error counter.
* **Creation**: [The W-bit inflight gauge](/hsms/inflight-gauge.md) increment discipline and its liveness coupling to `hsmsss`.
* **Creation**: [What a decoded item aliases](/secs2/decode-aliasing.md) per-leaf aliasing, the scalar/values split, the capability-gated entry point.
* **Creation**: [Item slab carving and its retention model](/secs2/item-slab-retention.md) chunk growth rule and the pointer-free property that bounds retention.
* **Creation**: [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md) synchronous-path counterpart to the doc's async stale-frame guarantee.
* **Update**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md) corrected — four `dropNotSelected` call sites across three send entry points, not two. Draft pending independent verification.
* **Update**: [Send error accounting — which outcomes count](/hsms/send-error-accounting.md) corrected — the error increment sits inside an `isData` check, so control T6 timeouts count nowhere. Draft pending independent verification.
* **Update**: [Activity stamps — the state behind linktest suppression](/hsmsss/activity-stamps.md) corrected — the per-generation reset is a rebaseline, not a fence; a straggler can stamp once after it. Draft pending independent verification.
* **Update**: [What a decoded item aliases](/secs2/decode-aliasing.md) cited `secs2/item.go` for the retention claim.
* **Update**: [The B1/B2 Selected gates on the send path](/hsms/selected-gates.md), [Send error accounting](/hsms/send-error-accounting.md), and [Activity stamps](/hsmsss/activity-stamps.md) independently verified against source by a second model and promoted to stable.
* **Update**: recorded the independent verification of [The W-bit inflight gauge](/hsms/inflight-gauge.md), [The I1 stale-epoch write guard](/hsms/stale-epoch-write-guard.md), [What a decoded item aliases](/secs2/decode-aliasing.md), and [Item slab carving and its retention model](/secs2/item-slab-retention.md), which had happened but was missing from their frontmatter. Every entry now carries a verification by an actor other than its author.
