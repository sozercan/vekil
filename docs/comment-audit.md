# Comment audit

This is the first cleanup batch. The rule inventories the whole supported source tree, but the remaining grandfathered prose has not been individually classified. Run the inventory in [Development](development.md#lint) to choose the next batch. Build directives, interpreter directives, and vendor banners are not part of the prose audit.

The locations below refer to the pre-cleanup revision `d58737123a325d873f633bf283faa669b3710985`. A range classifies every comment line in that range; “all” means all prose comments the inventory found in that file. The batch removed 157 tautology lines, 4 stale lines, and 11 excuse lines, and migrated the rationale from 130 decision lines. Four of those lines contained code as well and remain as code-only lines.

| Inventory | Comment-only lines | Lines with inline comments |
| --- | ---: | ---: |
| Before cleanup | 6,579 | 166 |
| After this batch | 6,281 | 162 |

The comment-only column estimates the gross source lines that could be shed. Removing inline comments shortens their lines without deleting them; ADRs and lint tooling are separate additions.

| Original source | Tautology | Stale | Excuse → issue | Decision → ADR |
| --- | --- | --- | --- | --- |
| `auth/authenticator_test.go` | 656, 1192, 1197, 1251 | — | — | — |
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
| `proxy/state_binding_durable_darwin.go` | — | — | — | 18–20, 44–47, 49–50 → [0004](adr/0004-descriptor-based-macos-state-validation.md) |
| `proxy/streaming_test.go` | 898, 919, 934, 943, 958, 1052, 1073, 1085, 1099, 1108, 1117, 1126, 1173, 1185, 1197, 1226, 1758 | — | — | — |
| `scripts/live-policy-routing-smoke.sh` | — | 37 | 1227–1230 → [#470](https://github.com/sozercan/vekil/issues/470) | — |
