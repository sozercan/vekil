import SwiftUI
import VekilCore

public struct VekilRootView: View {
    @ObservedObject var app: VekilAppState
    @ObservedObject var analytics: AnalyticsViewModel

    public init(app: VekilAppState, analytics: AnalyticsViewModel) {
        self.app = app; self.analytics = analytics
    }

    public var body: some View {
        NavigationSplitView {
            List(selection: Binding(
                get: { app.selectedDestination }, set: { app.selectDestination($0) }
            )) {
                ForEach(VekilDestination.allCases, id: \.self) { destination in
                    Label(destination.title, systemImage: destination.icon).tag(destination)
                }
            }
            .navigationTitle("Vekil")
        } detail: {
            detail
        }
        .frame(minWidth: 860, minHeight: 580)
    }

    @ViewBuilder private var detail: some View {
        switch app.selectedDestination {
        case .overview: OverviewView(app: app, analytics: analytics)
        case .traffic: TrafficView(analytics: analytics)
        case .requests: RequestsView(analytics: analytics)
        case .providers: ProvidersView(app: app, analytics: analytics)
        case .settings: SettingsView(app: app)
        }
    }
}

private extension VekilDestination {
    var title: String {
        rawValue.capitalized
    }

    var icon: String {
        switch self {
        case .overview: "gauge.with.dots.needle.67percent"
        case .traffic: "chart.xyaxis.line"
        case .requests: "list.bullet.rectangle"
        case .providers: "externaldrive.connected.to.line.below"
        case .settings: "gearshape"
        }
    }
}

private struct PageTitle: View {
    let title: String, subtitle: String
    var body: some View {
        VStack(alignment: .leading) { Text(title).font(.largeTitle.bold()); Text(subtitle).foregroundStyle(.secondary) }.frame(maxWidth: .infinity, alignment: .leading)
    }
}

private struct Notice: View {
    let text: String
    var body: some View {
        Label(text, systemImage: "exclamationmark.triangle.fill").foregroundStyle(.orange).padding().frame(maxWidth: .infinity, alignment: .leading).background(.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 10)).accessibilityLabel("Warning: \(text)")
    }
}

public struct OverviewView: View {
    @ObservedObject var app: VekilAppState
    @ObservedObject var analytics: AnalyticsViewModel
    @State private var diagnosticsExpanded = false

    public var body: some View {
        ScrollView {
            VStack(alignment: .leading, spacing: 18) {
                PageTitle(title: "Overview", subtitle: "Status and setup at a glance.")

                if let notice = errorNotice {
                    Notice(text: notice)
                }
                if let notice = app.environmentTokenSignOutNotice {
                    Notice(text: notice)
                }

                HStack(spacing: 16) {
                    Label(app.presentation.title, systemImage: statusSystemImage)
                        .font(.title3.bold())
                        .accessibilityLabel("Proxy status: \(app.presentation.title)")
                    Spacer()
                    Button(app.primaryAction.title) { Task { await app.performPrimaryAction() } }
                        .buttonStyle(.borderedProminent)
                        .disabled(!app.primaryAction.isEnabled)
                }

                GroupBox("Setup") {
                    VStack(spacing: 12) {
                        summaryRow(
                            title: "Configuration",
                            systemImage: "slider.horizontal.3",
                            value: configurationSummary
                        )
                        Divider()
                        summaryRow(
                            title: "GitHub account",
                            systemImage: "person.crop.circle",
                            value: authenticationSummary
                        )
                    }
                    .padding(.vertical, 6)
                }

                if let baseURL = app.baseURL {
                    GroupBox("Local endpoint") {
                        VStack(alignment: .leading, spacing: 12) {
                            Text(baseURL.absoluteString)
                                .font(.callout.monospaced())
                                .foregroundStyle(.secondary)
                                .textSelection(.enabled)
                            HStack {
                                Button("Copy Base URL") { Task { await app.copyBaseURL() } }
                                Button("Open Dashboard") { Task { await app.openDashboard() } }
                            }
                        }
                        .padding(.vertical, 6)
                        .frame(maxWidth: .infinity, alignment: .leading)
                    }
                }

                if let code = app.deviceCode {
                    GroupBox("GitHub Device Sign In") {
                        HStack {
                            Text(code.userCode).font(.title2.monospaced())
                            Button("Copy") { Task { await app.copyDeviceCode() } }
                            Button("Open GitHub") { Task { await app.openDeviceVerificationPage() } }
                        }
                        .padding(.vertical, 6)
                    }
                }

                GroupBox {
                    DisclosureGroup(isExpanded: $diagnosticsExpanded) {
                        Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 8) {
                            diagnosticRow("Readiness", readinessSummary)
                            diagnosticRow("Startup stage", startupStageSummary)
                            diagnosticRow("Runtime generation", app.runtimeState.runtimeGeneration.map(String.init) ?? "Not started")
                            diagnosticRow("Helper build", app.helperBuild ?? "Unavailable")
                            diagnosticRow("Helper epoch", app.runtimeState.helperEpoch.isEmpty ? "Not connected" : app.runtimeState.helperEpoch)
                            diagnosticRow("Configuration revision", app.runtimeState.configRevision ?? "Unavailable")
                            diagnosticRow("Selected configuration revision", app.runtimeState.configuration.selectedRevision ?? "Unavailable")
                            diagnosticRow("Active configuration revision", app.runtimeState.configuration.activeRevision ?? "Unavailable")
                        }
                        .padding(.top, 10)
                        .textSelection(.enabled)
                    } label: {
                        Text("Diagnostics").font(.headline)
                    }
                }
            }
            .padding(24)
        }
        .onAppear { analytics.setVisible(.overview, true) }
        .onDisappear { analytics.setVisible(.overview, false) }
    }

    private var errorNotice: String? {
        if let message = app.lastError?.userMessage {
            return message
        }
        guard app.presentation.isWarning else {
            return nil
        }
        return app.presentation.detail
    }

    private var statusSystemImage: String {
        switch app.presentation.kind {
        case .ready:
            return "checkmark.circle.fill"
        case .failed, .helperUnavailable, .degraded, .authenticationRequired:
            return "exclamationmark.triangle.fill"
        case .initializing, .starting, .stopping:
            return "clock.arrow.circlepath"
        case .stopped:
            return "circle"
        }
    }

    private var configurationSummary: String {
        let name = app.runtimeState.configuration.displayName
        switch app.runtimeState.configuration.drift {
        case .none:
            return name
        case .changed:
            return "\(name) — changes pending"
        case .missing:
            return "\(name) — file missing"
        case .unsafe:
            return "\(name) — needs attention"
        case .invalid:
            return "\(name) — invalid"
        default:
            return "\(name) — \(humanized(app.runtimeState.configuration.drift.rawValue).lowercased())"
        }
    }

    private var authenticationSummary: String {
        guard app.runtimeState.configuration.requiresGitHubAuthentication else {
            return "Not required for this configuration"
        }

        switch app.runtimeState.authentication.state {
        case .notRequired:
            return "Not required for this configuration"
        case .signedOut:
            return "Not signed in"
        case .signingIn:
            return "Signing in…"
        case .failed:
            return "Sign-in failed"
        case .signedIn:
            switch app.runtimeState.authentication.source {
            case .environment:
                return "Using an environment token"
            case .vekil:
                return "Signed in with Vekil"
            case .githubCLI:
                return "Using the GitHub CLI account"
            case .none:
                return "Signed in"
            default:
                return "Signed in via \(humanized(app.runtimeState.authentication.source.rawValue).lowercased())"
            }
        default:
            return humanized(app.runtimeState.authentication.state.rawValue)
        }
    }

    private var readinessSummary: String {
        switch app.runtimeState.readiness {
        case .unknown:
            return "Unknown"
        case .checking:
            return "Checking"
        case .ready:
            return "Ready"
        case .notReady:
            return "Not ready"
        case .stale:
            return "Status unavailable"
        default:
            return humanized(app.runtimeState.readiness.rawValue)
        }
    }

    private var startupStageSummary: String {
        guard let phase = app.runtimeState.operation?.phase else {
            return "Not active"
        }
        switch phase {
        case .loadingConfiguration:
            return "Loading configuration"
        case .constructingServer:
            return "Preparing server"
        case .listenerStartup:
            return "Starting local listener"
        case .startupAuthentication:
            return "Checking authentication"
        case .dynamicProviderModelValidation:
            return "Validating provider models"
        case .policyRoutingPreflight:
            return "Checking routing policy"
        case .readinessCheck:
            return "Checking readiness"
        case .cleanup:
            return "Cleaning up"
        default:
            return humanized(phase.rawValue)
        }
    }

    private func summaryRow(title: String, systemImage: String, value: String) -> some View {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
            Label(title, systemImage: systemImage)
                .foregroundStyle(.secondary)
            Spacer(minLength: 24)
            Text(value)
                .multilineTextAlignment(.trailing)
        }
        .frame(maxWidth: .infinity)
    }

    private func diagnosticRow(_ title: String, _ value: String) -> some View {
        GridRow {
            Text(title).foregroundStyle(.secondary)
            Text(value)
        }
    }

    private func humanized(_ value: String) -> String {
        value
            .replacingOccurrences(of: "_", with: " ")
            .replacingOccurrences(of: "-", with: " ")
            .capitalized
    }
}

public struct TrafficView: View {
    @ObservedObject var analytics: AnalyticsViewModel
    public var body: some View {
        ScrollView { VStack(alignment: .leading, spacing: 18) {
            PageTitle(title: "Traffic", subtitle: "Current-run, generation-scoped in-memory analytics.")
            Text(analytics.state.snapshotState.presentationLabel).font(.headline)
            if case let .stale(_, failure) = analytics.state.snapshotState {
                Notice(text: "Analytics are stale: \(failure.message)")
            }
            if let capture = analytics.state.snapshotState.capture {
                let totals = capture.snapshot.totals
                HStack { metric("Requests", totals.requests); metric("Errors", totals.errors); metric("Tokens", totals.totalTokens); metric("Retries", capture.snapshot.retries); metric("Failovers", capture.snapshot.targetSwitches) }
                Text("Latency p50 / p95 / p99: \(totals.latencyP50Milliseconds) / \(totals.latencyP95Milliseconds) / \(totals.latencyP99Milliseconds) ms")
                Text("180-second series: \(capture.snapshot.series.count) samples").accessibilityLabel("Traffic chart summary, \(capture.snapshot.series.count) samples")
                breakdown("Providers", capture.snapshot.byProvider.map { ($0.provider, $0.requests) })
                breakdown("Models", capture.snapshot.byModel.map { ($0.model, $0.requests) })
            } else {
                Text("No current analytics snapshot.").foregroundStyle(.secondary)
            }
        }.padding(24) }.onAppear { analytics.setVisible(.traffic, true) }.onDisappear { analytics.setVisible(.traffic, false) }
    }

    private func metric(_ name: String, _ value: Int64) -> some View {
        VStack(alignment: .leading) { Text(name).font(.caption); Text(value.formatted()).font(.title2.monospacedDigit()) }.padding().background(.quaternary, in: RoundedRectangle(cornerRadius: 10))
    }

    private func breakdown(_ title: String, _ rows: [(String, Int64)]) -> some View {
        GroupBox(title) { VStack(alignment: .leading) { ForEach(Array(rows.prefix(10).enumerated()), id: \.offset) { _, row in HStack { Text(row.0.isEmpty ? "other" : row.0); Spacer(); Text(row.1.formatted()).monospacedDigit() } } }.padding(.vertical, 4) }
    }
}

public struct RequestsView: View {
    @ObservedObject var analytics: AnalyticsViewModel
    @State private var filter: StatsRequestFilter = .all
    public var body: some View {
        VStack(alignment: .leading, spacing: 14) {
            PageTitle(title: "Requests", subtitle: "Bounded recent requests; attempts never join across snapshots or helper launches.")
            Picker("Filter", selection: $filter) { ForEach(StatsRequestFilter.allCases, id: \.self) { Text($0.title).tag($0) } }.pickerStyle(.segmented).frame(width: 360)
            ScrollView {
                LazyVStack(alignment: .leading, spacing: 8) {
                    ForEach(Array(analytics.requests.filter { filter.includes($0.request) }.enumerated()), id: \.offset) { _, row in
                        HStack {
                            Text(row.request.status.formatted()).frame(width: 44)
                            Text(row.request.endpoint).frame(width: 100, alignment: .leading)
                            Text(row.request.model).frame(width: 120, alignment: .leading)
                            Text([row.request.routeID, row.request.finalTarget].filter { !$0.isEmpty }.joined(separator: " → ")).frame(maxWidth: .infinity, alignment: .leading)
                            Text("\(row.request.durationMilliseconds) ms").frame(width: 75)
                            Text(row.request.totalTokens.formatted()).frame(width: 60)
                            Text(row.completenessLabel).foregroundStyle(row.isPartial ? .orange : .secondary).frame(width: 65)
                        }
                        .padding(.vertical, 5)
                        .accessibilityElement(children: .combine)
                        Divider()
                    }
                }
            }
        }.padding(24).onAppear { analytics.setVisible(.requests, true) }.onDisappear { analytics.setVisible(.requests, false) }
    }
}

public struct ProvidersView: View {
    @ObservedObject var app: VekilAppState
    @ObservedObject var analytics: AnalyticsViewModel
    public var body: some View {
        Form {
            Section("External Configuration") {
                LabeledContent("Selected", value: app.runtimeState.configuration.selectedExternalPath ?? "None")
                LabeledContent("On-disk revision", value: app.runtimeState.configuration.selectedRevision ?? "—")
                LabeledContent("Active revision", value: app.runtimeState.configuration.activeRevision ?? "—")
                HStack { Button("Choose…") { Task { await app.chooseExternalConfiguration() } }; Button("Reload and Restart") { Task { await app.reloadExternalConfiguration() } }.disabled(app.runtimeState.configuration.mode != .external); Button("Use Default / Managed") { Task { await app.clearExternalConfiguration() } } }
                Text("Swift passes only the path. External bytes remain user-owned and are never rewritten by the app.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Managed Configuration") {
                Text("Managed provider drafts and Validate and Apply are protocol-backed scaffolding. Secrets are staged in Keychain and never written to YAML, preferences, logs, or environment variables.")
                Button("Validate and Apply") {}.disabled(true)
            }
        }.formStyle(.grouped).padding(24).onAppear { analytics.setVisible(.providers, true) }.onDisappear { analytics.setVisible(.providers, false) }
    }
}

public struct SettingsView: View {
    @ObservedObject var app: VekilAppState
    public var body: some View {
        Form {
            Section("Startup") {
                Toggle("Open at Login", isOn: Binding(get: { app.openAtLogin }, set: { value in Task { await app.setOpenAtLogin(value) } }))
                if app.loginItemStatus == .requiresApproval {
                    Text("macOS requires approval before Vekil can open at login.")
                        .foregroundStyle(.orange)
                    Button("Open Login Items Settings") { Task { await app.openLoginItemSettings() } }
                }
                Toggle("Start proxy when the app launches", isOn: Binding(get: { app.startProxyWhenAppLaunches }, set: { app.setStartProxyWhenAppLaunches($0) }))
                Text("These settings are independent and both default off.").font(.caption).foregroundStyle(.secondary)
            }
            Section("Updates") { Button("Check for Updates…") { Task { await app.checkForUpdates() } }.disabled(!app.updaterAvailable) }
            Section("Build") {
                LabeledContent("App version", value: app.applicationVersion)
                LabeledContent("Bundle build ID", value: app.bundleBuildID ?? "—")
                LabeledContent("Helper build", value: app.helperBuild ?? "—")
                LabeledContent("Helper epoch", value: app.runtimeState.helperEpoch.isEmpty ? "Not connected" : app.runtimeState.helperEpoch)
                LabeledContent("Configuration revision", value: app.runtimeState.configRevision ?? "—")
            }
        }.formStyle(.grouped).padding(24)
    }
}
