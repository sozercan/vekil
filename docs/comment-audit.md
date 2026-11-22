# Comment audit

The [classification ledger](comment-classifications.tsv) accounts for every prose comment in the supported first-party source inventory. Executable directives, vendored assets, and text inside data fixtures are outside this audit. The final inventory is empty; CI enforces the zero baseline. Run the inventory in [Development](development.md#lint) to rescan the working tree.

| Category | First cleanup lines | Final cleanup lines | Total comment lines |
| --- | ---: | ---: | ---: |
| Tautology | 228 | 667 | 895 |
| Excuse | 13 | 17 | 30 |
| Stale | 6 | 14 | 20 |
| Decision Record | 424 | 5,377 | 5,801 |
| **Total** | **671** | **6,075** | **6,746** |

Both passes removed **6,584 comment-only source lines** and shortened **162 inline lines**. These are gross source counts; the ADRs, ledger, and lint tooling add separate lines.

The TSV groups adjacent comment-only lines at the same column and records inline comments separately. `start_line`, `end_line`, and `start_column` are 1-based. `removed` locations refer to the first-cleanup revision below; `final-cleanup` locations refer to `57939d5905190de91a5b76633df6f18bcba22425`. The digest is the first 12 hex characters of SHA-256 over normalized comment text. Use `git show <revision>:<path>` and the recorded range to recover the original wording. The `disposition` column names the related ADR, issue, or deletion action.

## Final Cleanup

The final pass removed 5,941 comment-only lines (including six newly inventoried lines inside literal `jq` filters) and shortened 134 source lines. Lasting rationale was consolidated by topic in the [ADR index](adr/README.md), not reproduced as a second copy of the source. The newly documented compaction publication race is tracked in [#476](https://github.com/sozercan/vekil/issues/476); the WebSocket test timing assumptions and broad control-proxy catches are tracked in [#475](https://github.com/sozercan/vekil/issues/475) and [#474](https://github.com/sozercan/vekil/issues/474), respectively.

Evidence is separate from category: `context-checked` means source context was checked; `rule-assigned` means a code-context or wording rule assigned the label; `provisional` means best fit, not a verified decision. Of the 2,424 final-pass blocks, 130 are context-checked, 1,490 are rule-assigned, and 804 are provisional. The final-pass Excuse and Stale classifications were checked against the implementation. For tentative Decision Records, the disposition points to the related ADR topic, while the immutable source location preserves the detail without asserting it as a design requirement.

## First Cleanup

The locations below refer to the pre-cleanup revision `d58737123a325d873f633bf283faa669b3710985`. A range classifies every comment line in that range; “all” means all prose comments the inventory found in that file. A mixed comment is classified by its actionable content.

| Inventory | Comment-only lines | Lines with inline comments |
| --- | ---: | ---: |
| Before cleanup | 6,578 | 162 |
| After this batch | 5,935 | 134 |

These first-pass counts predate the jq inventory expansion. The batch deleted 643 comment-only lines and shortened 28 source lines.

| Original source | Tautology | Stale | Excuse → issue | Decision → ADR |
| --- | --- | --- | --- | --- |
| `auth/authenticator_test.go` | 656, 1192, 1197, 1251 | — | — | — |
| `cmd/menubar/dialog_linux.go` | all remaining | 159–160 | — | 26–34, 46–47, 52–54, 73–76, 81–84, 88–91, 119–122, 139–145, 157–158, 161, 229–234, 265–267, 285–292, 297–303, 341–350, 368–370, 382–384 → [0008](adr/0008-linux-tray-fallback-boundaries.md) |
| `k8s/vekil.yaml` | — | — | — | 58–60 → [0006](adr/0006-ephemeral-kubernetes-example-cache.md) |
| `logger/logger.go` | 1, 13, 32, 53, 58, 66, 89, 97, 105, 113, 121, 127, 133, 138 | 46 | — | — |
| `logger/rotating_file.go` | 10, 23–24 | — | — | 13–14, 41, 63–64 → [0003](adr/0003-bounded-launch-logs.md) |
| `logger/rotating_file_test.go` | all | — | — | — |
| `models/anthropic.go` | 7, 27, 34–36, 53, 61, 76, 82, 94, 102–103, 108, 118, 129, 137 | 1–2 | — | 28, 47–48, 69–70, 130 → [0002](adr/0002-anthropic-wire-shapes.md) |
| `models/gemini.go`, `models/openai.go` | all | — | — | — |
| `proxy/dashboard/dashboard.html` | 377–378, 399, 445, 459, 488, 504–505, 640, 662, 674–675, 769–771, 813, 836 | — | — | 395–396, 467–468, 666–668, 714–717, 742, 758–760 → [0005](adr/0005-dashboard-without-inference.md) |
| `proxy/dashboard_insight_test.go`, `proxy/dashboard_review_fixes_test.go` | 92 and 438, respectively | — | — | — |
| `proxy/gemini.go` | — | — | 1054–1058 → [#468](https://github.com/sozercan/vekil/issues/468) | — |
| `proxy/handler_test.go` | 618, 625, 2062, 2072, 11564, 12725, 12733, 12765, 12794, 12800, 13178, 13800, 13813, 13822, 13836, 13874 | — | — | — |
| `proxy/responses_over_chat.go` | 35–36, 72, 143–145, 152, 294–296 | — | — | 18–25, 28–30, 47–48, 53–56, 85–87, 95–96, 121–123, 150, 185–187, 204–205, 250–253, 311–313, 320–322, 332–333, 363–364 → [0001](adr/0001-native-chat-responses-adapter.md) |
| `proxy/responses_over_chat_stream.go` | 191–192, 471, 506, 535–537, 671, 683–684 | — | — | 18–20, 38–40, 61–63, 83–85, 380–381, 386–387, 409–413, 420–421, 499–500, 554–555, 712–715, 742–743, 747–750, 800–801 → [0001](adr/0001-native-chat-responses-adapter.md) |
| `proxy/responses_over_chat_test.go` | 209, 258, 683, 692, 952, 956, 1002, 1185, 1251 | — | 1438–1439 → [#469](https://github.com/sozercan/vekil/issues/469) | 384–385, 957–958, 1215–1216, 1521–1522 → [0001](adr/0001-native-chat-responses-adapter.md) |
| `proxy/retry_test.go` | — | — | 488, 492 → [#472](https://github.com/sozercan/vekil/issues/472) | — |
| `proxy/state_binding_durable_darwin.go` | — | — | — | 18–20, 44–47, 49–50, 57 (prose only) → [0004](adr/0004-descriptor-based-macos-state-validation.md) |
| `proxy/stats.go` | all remaining | — | — | 23–28, 33–40, 55, 70–72, 89–90, 101–104, 188–196, 205, 207–208, 213–214, 235, 253–256, 284–288, 297–298, 304–305, 332–339, 365–368, 384–386, 461–462, 470–493, 648–651, 722–723, 734–735, 783, 872–880, 889–891, 922–926, 967, 1002–1005, 1029, 1088–1090, 1178–1179, 1201, 1213, 1265–1267, 1297–1298, 1309–1310, 1363–1364, 1478–1483, 1501–1508, 1591–1602, 1630–1632, 1647–1648, 1681–1688, 1721–1755, 1774–1784, 1809–1813, 1821–1828, 1858–1863, 1889–1891, 1919–1921 → [0007](adr/0007-separate-client-and-attempt-metrics.md) |
| `proxy/streaming_test.go` | 898, 919, 934, 943, 958, 1052, 1073, 1085, 1099, 1108, 1117, 1126, 1173, 1185, 1197, 1226, 1758 | — | — | — |
| `scripts/live-policy-routing-smoke.sh` | — | 37 | 1227–1230 → [#470](https://github.com/sozercan/vekil/issues/470) | — |
