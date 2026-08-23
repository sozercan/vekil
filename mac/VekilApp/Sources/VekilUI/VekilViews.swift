import Charts
import SwiftUI
import VekilCore

public struct VekilRootView: View {
  @ObservedObject var app: VekilAppState
  @ObservedObject var analytics: AnalyticsViewModel

  public init(app: VekilAppState, analytics: AnalyticsViewModel) {
    self.app = app
    self.analytics = analytics
  }

  public var body: some View {
    if app.isShowingOnboarding {
      VekilOnboardingView(
        app: app,
        analytics: analytics,
        onComplete: { app.completeOnboarding() },
        onDefer: { app.deferOnboarding() }
      )
    } else {
      workspace
    }
  }

  private var workspace: some View {
    NavigationSplitView {
      List(
        selection: Binding(
          get: { app.selectedDestination },
          set: { app.selectDestination($0) }
        )
      ) {
        Section {
          ForEach(VekilDestination.allCases, id: \.self) { destination in
            destinationLabel(destination)
          }
        }
      }
      .listStyle(.sidebar)
      .navigationTitle("Vekil")
      .navigationSplitViewColumnWidth(min: 180, ideal: 210, max: 260)
    } detail: {
      detail
        .navigationTitle(app.selectedDestination.title)
    }
    .toolbar {
      ToolbarItemGroup(placement: .primaryAction) {
        if let baseURL = app.baseURL {
          Menu {
            Button("Copy Base URL") {
              Task { await app.copyBaseURL() }
            }
            Button("Open Dashboard") {
              Task { await app.openDashboard() }
            }
          } label: {
            Label(endpointLabel(baseURL), systemImage: "network")
          }
          .help(baseURL.absoluteString)
        }

        Button {
          Task { await app.performPrimaryAction() }
        } label: {
          Label(app.primaryAction.title, systemImage: primaryActionIcon)
        }
        .disabled(!app.primaryAction.isEnabled)
      }
    }
    .frame(minWidth: 860, minHeight: 580)
  }

  @ViewBuilder
  private var detail: some View {
    switch app.selectedDestination {
    case .overview:
      OverviewView(app: app, analytics: analytics)
    case .activity:
      ActivityView(analytics: analytics)
    case .connection:
      ConnectionView(app: app, analytics: analytics)
    case .clients:
      ClientSetupView(app: app, analytics: analytics)
    case .settings:
      SettingsView(app: app)
    case .about:
      AboutView(app: app)
    }
  }

  private func destinationLabel(_ destination: VekilDestination) -> some View {
    Label(destination.title, systemImage: destination.icon)
      .tag(destination)
  }

  private func endpointLabel(_ url: URL) -> String {
    guard let host = url.host else { return "Local endpoint" }
    if let port = url.port {
      return "\(host):\(port)"
    }
    return host
  }

  private var primaryActionIcon: String {
    switch app.primaryAction.kind {
    case .startProxy:
      return "play.fill"
    case .cancelStarting:
      return "xmark"
    case .stopProxy:
      return "stop.fill"
    case .restartHelper:
      return "arrow.clockwise"
    case .none:
      return "circle"
    }
  }
}

extension VekilDestination {
  fileprivate var title: String {
    switch self {
    case .overview:
      return "Overview"
    case .activity:
      return "Activity"
    case .connection:
      return "Connection"
    case .clients:
      return "Clients"
    case .settings:
      return "Settings"
    case .about:
      return "About"
    }
  }

  fileprivate var icon: String {
    switch self {
    case .overview:
      return "gauge.with.dots.needle.67percent"
    case .activity:
      return "chart.xyaxis.line"
    case .connection:
      return "externaldrive.connected.to.line.below"
    case .clients:
      return "terminal"
    case .settings:
      return "gearshape"
    case .about:
      return "info.circle"
    }
  }
}

private struct PageTitle: View {
  let title: String
  let subtitle: String

  var body: some View {
    VStack(alignment: .leading, spacing: 4) {
      Text(title)
        .font(.largeTitle.weight(.semibold))
      Text(subtitle)
        .foregroundStyle(.secondary)
    }
    .frame(maxWidth: .infinity, alignment: .leading)
  }
}

private struct Notice: View {
  let text: String

  var body: some View {
    Label(text, systemImage: "exclamationmark.triangle.fill")
      .foregroundStyle(.orange)
      .padding(12)
      .frame(maxWidth: .infinity, alignment: .leading)
      .background(.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 10))
      .accessibilityLabel("Warning: \(text)")
  }
}

private struct EmptyState: View {
  let title: String
  let message: String
  let systemImage: String

  var body: some View {
    VStack(spacing: 10) {
      Image(systemName: systemImage)
        .font(.system(size: 28))
        .foregroundStyle(.secondary)
      Text(title)
        .font(.headline)
      Text(message)
        .foregroundStyle(.secondary)
        .multilineTextAlignment(.center)
        .frame(maxWidth: 360)
    }
    .padding(32)
    .frame(maxWidth: .infinity, maxHeight: .infinity)
  }
}

private struct MetricTile: View {
  let title: String
  let value: String
  let systemImage: String
  var detail: String?

  var body: some View {
    VStack(alignment: .leading, spacing: 8) {
      Label(title, systemImage: systemImage)
        .font(.caption)
        .foregroundStyle(.secondary)
      Text(value)
        .font(.title2.weight(.semibold).monospacedDigit())
        .lineLimit(1)
        .minimumScaleFactor(0.8)
      if let detail {
        Text(detail)
          .font(.caption)
          .foregroundStyle(.secondary)
          .lineLimit(1)
      }
    }
    .padding(14)
    .frame(maxWidth: .infinity, minHeight: 92, alignment: .leading)
    .background(.quaternary, in: RoundedRectangle(cornerRadius: 10))
    .accessibilityElement(children: .combine)
  }
}

private enum TrafficChartMetric: String, CaseIterable, Identifiable {
  case requests = "Requests"
  case tokens = "Tokens"

  var id: String { rawValue }
}

private struct TrafficChart: View {
  let points: [StatsSeriesPoint]
  let metric: TrafficChartMetric

  var body: some View {
    Chart {
      ForEach(points, id: \.timestamp) { point in
        switch metric {
        case .requests:
          LineMark(
            x: .value("Time", point.date),
            y: .value("Count", point.requests)
          )
          .foregroundStyle(by: .value("Series", "Requests"))
          .interpolationMethod(.monotone)

          LineMark(
            x: .value("Time", point.date),
            y: .value("Count", point.errors)
          )
          .foregroundStyle(by: .value("Series", "Errors"))
          .interpolationMethod(.monotone)
        case .tokens:
          AreaMark(
            x: .value("Time", point.date),
            y: .value("Count", point.promptTokens)
          )
          .foregroundStyle(by: .value("Series", "Input"))
          .opacity(0.45)

          LineMark(
            x: .value("Time", point.date),
            y: .value("Count", point.completionTokens)
          )
          .foregroundStyle(by: .value("Series", "Output"))
          .interpolationMethod(.monotone)
        }
      }
    }
    .chartLegend(position: .bottom, alignment: .leading, spacing: 12)
    .accessibilityLabel("\(metric.rawValue) over the current proxy run")
    .accessibilityValue("\(points.count) samples")
  }
}

public struct OverviewView: View {
  @ObservedObject var app: VekilAppState
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var diagnosticsExpanded = false

  public var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 18) {
        HStack(alignment: .top, spacing: 16) {
          PageTitle(title: "Overview", subtitle: "Your local gateway at a glance.")
          if app.baseURL != nil {
            Button {
              Task { await app.copyBaseURL() }
            } label: {
              Label("Copy Endpoint", systemImage: "doc.on.doc")
            }
          }
        }

        if let notice = errorNotice {
          Notice(text: notice)
        }
        if let notice = app.environmentTokenSignOutNotice {
          Notice(text: notice)
        }

        serviceCard

        if let capture = analytics.state.snapshotState.capture {
          metricGrid(capture.snapshot)

          if !capture.snapshot.series.isEmpty {
            GroupBox {
              TrafficChart(points: capture.snapshot.series, metric: .requests)
                .frame(height: 170)
                .padding(.top, 6)
            } label: {
              HStack {
                Text("Current-run traffic")
                Spacer()
                Text(analytics.state.snapshotState.presentationLabel)
                  .font(.caption)
                  .foregroundStyle(.secondary)
              }
            }
          }
        }

        setupGrid

        if let code = app.deviceCode {
          GroupBox("GitHub Device Sign In") {
            VStack(alignment: .leading, spacing: 12) {
              Text("Enter this code on GitHub to finish signing in.")
                .foregroundStyle(.secondary)
              HStack {
                Text(code.userCode)
                  .font(.title2.monospaced())
                  .textSelection(.enabled)
                Spacer()
                Button("Copy") {
                  Task { await app.copyDeviceCode() }
                }
                Button("Open GitHub") {
                  Task { await app.openDeviceVerificationPage() }
                }
                .buttonStyle(.borderedProminent)
              }
            }
            .padding(.vertical, 6)
          }
        }

        diagnostics
      }
      .padding(24)
    }
    .onAppear { analytics.setVisible(.overview, true) }
    .onDisappear { analytics.setVisible(.overview, false) }
  }

  private var serviceCard: some View {
    GroupBox {
      HStack(spacing: 14) {
        Image(systemName: statusSystemImage)
          .font(.system(size: 24))
          .foregroundStyle(statusColor)
          .frame(width: 34)

        VStack(alignment: .leading, spacing: 3) {
          Text(app.presentation.title)
            .font(.title3.weight(.semibold))
          if let detail = app.presentation.detail, !detail.isEmpty {
            Text(detail)
              .font(.callout)
              .foregroundStyle(.secondary)
          } else {
            Text(readinessSummary)
              .font(.callout)
              .foregroundStyle(.secondary)
          }
        }

        Spacer()

        if app.runtimeState.operation != nil {
          ProgressView()
            .controlSize(.small)
            .accessibilityLabel(startupStageSummary)
        }

        Button(app.primaryAction.title) {
          Task { await app.performPrimaryAction() }
        }
        .buttonStyle(.borderedProminent)
        .disabled(!app.primaryAction.isEnabled)
      }
      .padding(.vertical, 8)
      .accessibilityElement(children: .combine)
      .accessibilityLabel("Proxy status: \(app.presentation.title)")
    }
  }

  private func metricGrid(_ snapshot: StatsSnapshot) -> some View {
    let totals = snapshot.totals
    return LazyVGrid(
      columns: [GridItem(.adaptive(minimum: 132), spacing: 12)],
      spacing: 12
    ) {
      MetricTile(
        title: "Requests",
        value: totals.requests.formatted(),
        systemImage: "arrow.left.arrow.right",
        detail: "Current run"
      )
      MetricTile(
        title: "Tokens",
        value: totals.totalTokens.formatted(.number.notation(.compactName)),
        systemImage: "text.word.spacing",
        detail: "Input and output"
      )
      MetricTile(
        title: "Errors",
        value: totals.errors.formatted(),
        systemImage: "exclamationmark.circle",
        detail: errorRate(totals)
      )
      MetricTile(
        title: "P95 latency",
        value: durationLabel(totals.latencyP95Milliseconds),
        systemImage: "timer",
        detail: "End to end"
      )
    }
  }

  private var setupGrid: some View {
    LazyVGrid(
      columns: [GridItem(.adaptive(minimum: 280), spacing: 16)],
      spacing: 16
    ) {
      GroupBox("Setup") {
        VStack(spacing: 12) {
          summaryRow(
            title: "Configuration",
            systemImage: "slider.horizontal.3",
            value: configurationSummary(app.runtimeState.configuration)
          )
          Divider()
          summaryRow(
            title: "GitHub account",
            systemImage: "person.crop.circle",
            value: authenticationSummary(app.runtimeState)
          )
        }
        .padding(.vertical, 6)
      }

      GroupBox("Local endpoint") {
        if let baseURL = app.baseURL {
          VStack(alignment: .leading, spacing: 12) {
            Text(baseURL.absoluteString)
              .font(.callout.monospaced())
              .foregroundStyle(.secondary)
              .textSelection(.enabled)
            HStack {
              Button("Copy Base URL") {
                Task { await app.copyBaseURL() }
              }
              Button("Open Dashboard") {
                Task { await app.openDashboard() }
              }
            }
          }
          .padding(.vertical, 6)
          .frame(maxWidth: .infinity, alignment: .leading)
        } else {
          Text("Start the proxy to publish a local endpoint.")
            .foregroundStyle(.secondary)
            .padding(.vertical, 12)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
      }
    }
  }

  private var diagnostics: some View {
    GroupBox {
      DisclosureGroup(isExpanded: $diagnosticsExpanded) {
        Grid(alignment: .leading, horizontalSpacing: 18, verticalSpacing: 8) {
          diagnosticRow("Readiness", readinessSummary)
          diagnosticRow("Startup stage", startupStageSummary)
          diagnosticRow(
            "Runtime generation",
            app.runtimeState.runtimeGeneration.map(String.init) ?? "Not started"
          )
          diagnosticRow("Helper build", app.helperBuild ?? "Unavailable")
          diagnosticRow(
            "Helper epoch",
            app.runtimeState.helperEpoch.isEmpty
              ? "Not connected"
              : app.runtimeState.helperEpoch
          )
          diagnosticRow(
            "Configuration revision",
            app.runtimeState.configRevision ?? "Unavailable"
          )
          diagnosticRow(
            "Selected configuration revision",
            app.runtimeState.configuration.selectedRevision ?? "Unavailable"
          )
          diagnosticRow(
            "Active configuration revision",
            app.runtimeState.configuration.activeRevision ?? "Unavailable"
          )
        }
        .padding(.top, 10)
        .textSelection(.enabled)
      } label: {
        Text("Diagnostics")
          .font(.headline)
      }
    }
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

  private var statusColor: Color {
    switch app.presentation.kind {
    case .ready:
      return .green
    case .failed, .helperUnavailable, .degraded, .authenticationRequired:
      return .orange
    case .initializing, .starting, .stopping:
      return .accentColor
    case .stopped:
      return .secondary
    }
  }

  private var readinessSummary: String {
    switch app.runtimeState.readiness {
    case .unknown:
      return "Readiness has not been checked."
    case .checking:
      return "Checking readiness…"
    case .ready:
      return "Ready for local requests."
    case .notReady:
      return "The proxy is not ready for requests."
    case .stale:
      return "The last readiness result is stale."
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
      Text(title)
        .foregroundStyle(.secondary)
      Text(value)
    }
  }
}

private enum ActivitySection: String, CaseIterable, Identifiable {
  case summary = "Summary"
  case requests = "Requests"

  var id: String { rawValue }
}

public struct ActivityView: View {
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var section: ActivitySection = .summary

  public var body: some View {
    VStack(spacing: 0) {
      HStack {
        Picker("Activity view", selection: $section) {
          ForEach(ActivitySection.allCases) { section in
            Text(section.rawValue).tag(section)
          }
        }
        .pickerStyle(.segmented)
        .frame(width: 260)
        Spacer()
      }
      .padding(.horizontal, 24)
      .padding(.top, 16)

      switch section {
      case .summary:
        TrafficView(analytics: analytics)
      case .requests:
        RequestsView(analytics: analytics)
      }
    }
  }
}

public struct TrafficView: View {
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var chartMetric: TrafficChartMetric = .requests

  public var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 18) {
        HStack(alignment: .top, spacing: 16) {
          PageTitle(
            title: "Traffic",
            subtitle: "Current-run, generation-scoped in-memory analytics."
          )
          refreshButton
        }

        if case .stale(_, let failure) = analytics.state.snapshotState {
          Notice(text: "Analytics are stale: \(failure.message)")
        }

        if let capture = analytics.state.snapshotState.capture {
          snapshotHeader(capture)
          trafficMetrics(capture.snapshot)

          GroupBox {
            VStack(alignment: .leading, spacing: 12) {
              Picker("Metric", selection: $chartMetric) {
                ForEach(TrafficChartMetric.allCases) { metric in
                  Text(metric.rawValue).tag(metric)
                }
              }
              .pickerStyle(.segmented)
              .frame(width: 220)

              if capture.snapshot.series.isEmpty {
                EmptyState(
                  title: "No traffic samples yet",
                  message: "The current proxy run has not recorded chart samples.",
                  systemImage: "chart.xyaxis.line"
                )
                .frame(height: 180)
              } else {
                TrafficChart(
                  points: capture.snapshot.series,
                  metric: chartMetric
                )
                .frame(height: 230)
              }
            }
            .padding(.vertical, 6)
          } label: {
            Text("Activity")
          }

          LazyVGrid(
            columns: [GridItem(.adaptive(minimum: 260), spacing: 16)],
            spacing: 16
          ) {
            BreakdownView(title: "Providers", rows: capture.snapshot.byProvider)
            BreakdownView(title: "Models", rows: capture.snapshot.byModel)
            BreakdownView(title: "Clients", rows: capture.snapshot.byAgent)
            BreakdownView(title: "Routes", rows: capture.snapshot.byRoute)
          }
        } else {
          EmptyState(
            title: "No analytics yet",
            message: "Start the proxy and send a request to populate current-run traffic.",
            systemImage: "chart.xyaxis.line"
          )
          .frame(minHeight: 360)
        }
      }
      .padding(24)
    }
    .onAppear { analytics.setVisible(.traffic, true) }
    .onDisappear { analytics.setVisible(.traffic, false) }
  }

  private var refreshButton: some View {
    Button {
      Task {
        await analytics.store.manualRefresh()
        await analytics.reload()
      }
    } label: {
      if analytics.state.isRefreshingStats {
        ProgressView()
          .controlSize(.small)
      } else {
        Label("Refresh", systemImage: "arrow.clockwise")
      }
    }
    .disabled(analytics.state.serviceState != .running || analytics.state.isRefreshingStats)
  }

  private func snapshotHeader(_ capture: StatsCapture) -> some View {
    HStack {
      Label(
        analytics.state.snapshotState.presentationLabel,
        systemImage: analytics.state.snapshotState.isStale
          ? "exclamationmark.triangle"
          : "checkmark.circle"
      )
      .font(.headline)
      Spacer()
      Text("Updated \(capture.capturedAt, style: .relative)")
        .font(.caption)
        .foregroundStyle(.secondary)
    }
  }

  private func trafficMetrics(_ snapshot: StatsSnapshot) -> some View {
    let totals = snapshot.totals
    return LazyVGrid(
      columns: [GridItem(.adaptive(minimum: 128), spacing: 12)],
      spacing: 12
    ) {
      MetricTile(
        title: "Requests",
        value: totals.requests.formatted(),
        systemImage: "arrow.left.arrow.right"
      )
      MetricTile(
        title: "Errors",
        value: totals.errors.formatted(),
        systemImage: "exclamationmark.circle",
        detail: errorRate(totals)
      )
      MetricTile(
        title: "Tokens",
        value: totals.totalTokens.formatted(.number.notation(.compactName)),
        systemImage: "text.word.spacing"
      )
      MetricTile(
        title: "P50 latency",
        value: durationLabel(totals.latencyP50Milliseconds),
        systemImage: "timer"
      )
      MetricTile(
        title: "P95 latency",
        value: durationLabel(totals.latencyP95Milliseconds),
        systemImage: "timer.square"
      )
      MetricTile(
        title: "Retries",
        value: snapshot.retries.formatted(),
        systemImage: "arrow.clockwise"
      )
      MetricTile(
        title: "Failovers",
        value: snapshot.targetSwitches.formatted(),
        systemImage: "arrow.triangle.branch"
      )
    }
  }
}

private struct BreakdownView: View {
  let title: String
  let rows: [StatsBreakdown]

  var body: some View {
    GroupBox(title) {
      if rows.isEmpty {
        Text("No data")
          .foregroundStyle(.secondary)
          .padding(.vertical, 18)
          .frame(maxWidth: .infinity, alignment: .center)
      } else {
        VStack(alignment: .leading, spacing: 10) {
          ForEach(Array(rows.prefix(8).enumerated()), id: \.offset) { _, row in
            VStack(alignment: .leading, spacing: 4) {
              HStack {
                Text(row.label.isEmpty ? "Other" : row.label)
                  .lineLimit(1)
                Spacer()
                Text(row.requests.formatted())
                  .monospacedDigit()
                  .foregroundStyle(.secondary)
              }
              ProgressView(
                value: Double(row.requests),
                total: Double(max(maxRequests, 1))
              )
              .accessibilityLabel(row.label.isEmpty ? "Other" : row.label)
              .accessibilityValue("\(row.requests) requests")
            }
          }
        }
        .padding(.vertical, 6)
      }
    }
  }

  private var maxRequests: Int64 {
    rows.map(\.requests).max() ?? 0
  }
}

private struct RequestRow: Identifiable {
  struct ID: Hashable {
    let scope: StatsSnapshotScope
    let sourceIndex: Int
  }

  let projected: StatsProjectedRequest

  var id: ID {
    ID(scope: projected.scope, sourceIndex: projected.sourceIndex)
  }

  var request: StatsRecentRequest { projected.request }
}

public struct RequestsView: View {
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var filter: StatsRequestFilter = .all
  @State private var searchText = ""
  @State private var selection: RequestRow.ID?

  public var body: some View {
    VStack(alignment: .leading, spacing: 14) {
      PageTitle(
        title: "Requests",
        subtitle: "Recent requests and route attempts from the current snapshot."
      )

      HStack(spacing: 12) {
        Picker("Filter", selection: $filter) {
          ForEach(StatsRequestFilter.allCases, id: \.self) { filter in
            Text(filter.title).tag(filter)
          }
        }
        .pickerStyle(.segmented)
        .frame(width: 300)

        TextField("Search model, endpoint, provider, or route", text: $searchText)
          .textFieldStyle(.roundedBorder)
          .frame(maxWidth: 360)

        Spacer()

        Text("\(rows.count) requests")
          .font(.caption)
          .foregroundStyle(.secondary)
      }

      VSplitView {
        Table(rows, selection: $selection) {
          TableColumn("Time") { row in
            Text(row.request.date, style: .time)
              .monospacedDigit()
          }
          TableColumn("Status") { row in
            StatusCodeLabel(status: row.request.status)
          }
          TableColumn("Endpoint") { row in
            Text(displayValue(row.request.endpoint))
              .lineLimit(1)
          }
          TableColumn("Model") { row in
            Text(displayValue(row.request.model))
              .lineLimit(1)
          }
          TableColumn("Route") { row in
            Text(routeSummary(row.request))
              .lineLimit(1)
          }
          TableColumn("Duration") { row in
            Text(durationLabel(row.request.durationMilliseconds))
              .monospacedDigit()
          }
          TableColumn("Tokens") { row in
            Text(row.request.totalTokens.formatted())
              .monospacedDigit()
          }
          TableColumn("Trace") { row in
            Text(row.projected.completenessLabel)
              .foregroundStyle(row.projected.isPartial ? .orange : .secondary)
          }
        }
        .frame(minHeight: selectedRow == nil ? 360 : 270)
        .overlay {
          if rows.isEmpty {
            EmptyState(
              title: emptyTitle,
              message: emptyMessage,
              systemImage: "list.bullet.rectangle"
            )
          }
        }

        if let selectedRow {
          RequestDetailView(row: selectedRow) {
            selection = nil
          }
          .frame(minHeight: 190, idealHeight: 230)
        }
      }
    }
    .padding(24)
    .onAppear { analytics.setVisible(.requests, true) }
    .onDisappear { analytics.setVisible(.requests, false) }
    .onChange(of: rows.map(\.id)) { ids in
      if let selection, !ids.contains(selection) {
        self.selection = nil
      }
    }
  }

  private var rows: [RequestRow] {
    analytics.requests
      .filter { filter.includes($0.request) }
      .filter { row in
        let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !query.isEmpty else { return true }
        let request = row.request
        return [
          request.model,
          request.endpoint,
          request.provider,
          request.routeID,
          request.finalTarget,
          request.agent,
        ].contains { $0.localizedCaseInsensitiveContains(query) }
      }
      .map(RequestRow.init(projected:))
  }

  private var selectedRow: RequestRow? {
    guard let selection else { return nil }
    return rows.first { $0.id == selection }
  }

  private var emptyTitle: String {
    analytics.requests.isEmpty ? "No requests yet" : "No matching requests"
  }

  private var emptyMessage: String {
    analytics.requests.isEmpty
      ? "Send a request through the running proxy to populate this table."
      : "Change the filter or search text to see more requests."
  }
}

private struct StatusCodeLabel: View {
  let status: Int

  var body: some View {
    Text(status == 0 ? "—" : status.formatted())
      .monospacedDigit()
      .foregroundStyle(color)
      .accessibilityLabel(status == 0 ? "No status" : "HTTP status \(status)")
  }

  private var color: Color {
    switch status {
    case 200..<400:
      return .green
    case 400...:
      return .red
    default:
      return .secondary
    }
  }
}

private struct RequestDetailView: View {
  let row: RequestRow
  let close: () -> Void

  var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 14) {
        HStack {
          VStack(alignment: .leading, spacing: 3) {
            Text(displayValue(row.request.model))
              .font(.headline)
            Text(row.request.date.formatted(date: .abbreviated, time: .standard))
              .font(.caption)
              .foregroundStyle(.secondary)
          }
          Spacer()
          Button(action: close) {
            Label("Close Details", systemImage: "xmark")
              .labelStyle(.iconOnly)
          }
          .buttonStyle(.plain)
          .help("Close request details")
        }

        Grid(alignment: .leading, horizontalSpacing: 22, verticalSpacing: 8) {
          detailRow("Status", row.request.status == 0 ? "—" : row.request.status.formatted())
          detailRow("Endpoint", displayValue(row.request.endpoint))
          detailRow("Provider", displayValue(row.request.provider))
          detailRow("Client", displayValue(row.request.agent))
          detailRow("Route", routeSummary(row.request))
          detailRow("Duration", durationLabel(row.request.durationMilliseconds))
          detailRow("Tokens", row.request.totalTokens.formatted())
          detailRow("Trace", row.projected.completenessLabel)
        }
        .textSelection(.enabled)

        if row.projected.isPartial {
          VStack(alignment: .leading, spacing: 4) {
            Text("Trace limitations")
              .font(.subheadline.weight(.semibold))
            ForEach(Array(row.projected.attemptCoverage.reasons.enumerated()), id: \.offset) {
              _, reason in
              Label(reason.description, systemImage: "exclamationmark.triangle")
                .font(.caption)
                .foregroundStyle(.orange)
            }
          }
        }

        VStack(alignment: .leading, spacing: 7) {
          Text("Route attempts")
            .font(.subheadline.weight(.semibold))
          if row.projected.attempts.isEmpty {
            Text("No attempt details are available for this request.")
              .font(.caption)
              .foregroundStyle(.secondary)
          } else {
            ForEach(Array(row.projected.attempts.enumerated()), id: \.offset) { _, attempt in
              HStack(spacing: 10) {
                Text("#\(attempt.sequence)")
                  .monospacedDigit()
                  .foregroundStyle(.secondary)
                Text(attempt.targetID.isEmpty ? "Redacted target" : attempt.targetID)
                Spacer()
                Text(attempt.outcome.isEmpty ? attempt.status : attempt.outcome)
                  .foregroundStyle(.secondary)
                if let latency = attempt.timeToFirstTokenMilliseconds {
                  Text("TTFT \(durationLabel(latency))")
                    .monospacedDigit()
                    .foregroundStyle(.secondary)
                }
              }
              .font(.caption)
            }
          }
        }
      }
      .padding(14)
      .frame(maxWidth: .infinity, alignment: .leading)
    }
  }

  private func detailRow(_ title: String, _ value: String) -> some View {
    GridRow {
      Text(title)
        .foregroundStyle(.secondary)
      Text(value)
    }
  }
}

private struct ModelRow: Identifiable {
  let model: RuntimeModel
  var id: String { model.id }
}

private enum ConnectionSection: String, CaseIterable, Identifiable {
  case providers = "Providers"
  case models = "Models"

  var id: String { rawValue }
}

public struct ConnectionView: View {
  @ObservedObject var app: VekilAppState
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var section: ConnectionSection = .providers

  public var body: some View {
    VStack(spacing: 0) {
      HStack {
        Picker("Connection view", selection: $section) {
          ForEach(ConnectionSection.allCases) { section in
            Text(section.rawValue).tag(section)
          }
        }
        .pickerStyle(.segmented)
        .frame(width: 260)
        Spacer()
        Button("Add or Change Providers…") {
          app.showOnboarding()
        }
      }
      .padding(.horizontal, 24)
      .padding(.top, 16)

      switch section {
      case .providers:
        ProvidersView(app: app, analytics: analytics)
      case .models:
        ModelsView(analytics: analytics)
      }
    }
  }
}

public struct ModelsView: View {
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var searchText = ""
  @State private var endpointFilter = ""
  @State private var selection: String?

  public var body: some View {
    VStack(alignment: .leading, spacing: 14) {
      HStack(alignment: .top, spacing: 16) {
        PageTitle(
          title: "Models",
          subtitle: "Native upstream endpoints reported by the running proxy."
        )
        refreshButton
      }

      if case .failed(_, let failure) = analytics.state.models {
        Notice(text: "The model catalog could not be refreshed: \(failure.message)")
      }

      HStack(spacing: 12) {
        TextField("Search model or owner", text: $searchText)
          .textFieldStyle(.roundedBorder)
          .frame(maxWidth: 340)

        Picker("Endpoint", selection: $endpointFilter) {
          Text("All endpoints").tag("")
          ForEach(availableEndpoints, id: \.self) { endpoint in
            Text(endpoint).tag(endpoint)
          }
        }
        .frame(width: 230)

        Spacer()

        Text("\(rows.count) models")
          .font(.caption)
          .foregroundStyle(.secondary)
      }

      VSplitView {
        Table(rows, selection: $selection) {
          TableColumn("Model") { row in
            VStack(alignment: .leading, spacing: 2) {
              Text(row.model.name.isEmpty ? row.model.id : row.model.name)
              if !row.model.name.isEmpty, row.model.name != row.model.id {
                Text(row.model.id)
                  .font(.caption)
                  .foregroundStyle(.secondary)
              }
            }
          }
          TableColumn("Owner") { row in
            Text(displayValue(row.model.ownedBy))
          }
          TableColumn("Native endpoints") { row in
            Text(endpointSummary(row.model))
              .lineLimit(2)
          }
          TableColumn("Context") { row in
            Text(contextWindow(row.model))
              .monospacedDigit()
          }
          TableColumn("Capabilities") { row in
            Text(capabilitySummary(row.model))
              .lineLimit(2)
          }
        }
        .frame(minHeight: selectedModel == nil ? 360 : 280)
        .overlay {
          if rows.isEmpty {
            EmptyState(
              title: modelEmptyTitle,
              message: modelEmptyMessage,
              systemImage: "square.stack.3d.up"
            )
          }
        }

        if let selectedModel {
          ModelDetailView(model: selectedModel) {
            selection = nil
          }
          .frame(minHeight: 160, idealHeight: 200)
        }
      }
    }
    .padding(24)
    .onAppear { analytics.setVisible(.providers, true) }
    .onDisappear { analytics.setVisible(.providers, false) }
    .onChange(of: rows.map(\.id)) { ids in
      if let selection, !ids.contains(selection) {
        self.selection = nil
      }
    }
  }

  private var refreshButton: some View {
    Button {
      Task {
        _ = await analytics.store.refreshModels(trigger: .manual)
        await analytics.reload()
      }
    } label: {
      if analytics.state.isRefreshingModels {
        ProgressView()
          .controlSize(.small)
      } else {
        Label("Refresh", systemImage: "arrow.clockwise")
      }
    }
    .disabled(analytics.state.serviceState != .running || analytics.state.isRefreshingModels)
  }

  private var catalog: [RuntimeModel] {
    analytics.state.models.capture?.catalog.data ?? []
  }

  private var rows: [ModelRow] {
    let query = searchText.trimmingCharacters(in: .whitespacesAndNewlines)
    return
      catalog
      .filter { !$0.id.isEmpty }
      .filter { model in
        endpointFilter.isEmpty || model.supportedEndpoints.contains(endpointFilter)
      }
      .filter { model in
        query.isEmpty
          || model.id.localizedCaseInsensitiveContains(query)
          || model.name.localizedCaseInsensitiveContains(query)
          || model.ownedBy.localizedCaseInsensitiveContains(query)
      }
      .sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
      .map(ModelRow.init(model:))
  }

  private var availableEndpoints: [String] {
    Array(Set(catalog.flatMap(\.supportedEndpoints))).sorted()
  }

  private var selectedModel: RuntimeModel? {
    guard let selection else { return nil }
    return rows.first { $0.id == selection }?.model
  }

  private var modelEmptyTitle: String {
    catalog.isEmpty ? "No model catalog" : "No matching models"
  }

  private var modelEmptyMessage: String {
    catalog.isEmpty
      ? "Start the proxy to load its current public model catalog."
      : "Change the search or endpoint filter to see more models."
  }
}

private struct ModelDetailView: View {
  let model: RuntimeModel
  let close: () -> Void

  var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 12) {
        HStack(alignment: .top) {
          VStack(alignment: .leading, spacing: 3) {
            Text(model.name.isEmpty ? model.id : model.name)
              .font(.headline)
            Text(model.id)
              .font(.caption.monospaced())
              .foregroundStyle(.secondary)
              .textSelection(.enabled)
          }
          Spacer()
          Button(action: close) {
            Label("Close Details", systemImage: "xmark")
              .labelStyle(.iconOnly)
          }
          .buttonStyle(.plain)
          .help("Close model details")
        }

        Grid(alignment: .leading, horizontalSpacing: 22, verticalSpacing: 8) {
          detailRow("Owner", displayValue(model.ownedBy))
          detailRow("Native endpoints", endpointSummary(model))
          detailRow("Context window", contextWindow(model))
          detailRow("Capabilities", capabilitySummary(model))
          detailRow(
            "Model picker",
            model.modelPickerEnabled == false ? "Hidden" : "Available"
          )
          if !model.modelPickerCategory.isEmpty {
            detailRow("Category", model.modelPickerCategory)
          }
        }
        .textSelection(.enabled)

        Text(
          "Native endpoints describe verified upstream routes. Vekil may still serve compatibility protocols through its translation layer."
        )
        .font(.caption)
        .foregroundStyle(.secondary)
      }
      .padding(14)
      .frame(maxWidth: .infinity, alignment: .leading)
    }
  }

  private func detailRow(_ title: String, _ value: String) -> some View {
    GridRow {
      Text(title)
        .foregroundStyle(.secondary)
      Text(value)
    }
  }
}

private enum ClientKind: String, CaseIterable, Identifiable {
  case claude = "Claude Code"
  case codex = "Codex CLI"
  case copilot = "GitHub Copilot CLI"

  var id: String { rawValue }

  var commandName: String {
    switch self {
    case .claude:
      return "claude"
    case .codex:
      return "codex"
    case .copilot:
      return "copilot"
    }
  }
}

public struct ClientSetupView: View {
  @ObservedObject var app: VekilAppState
  @ObservedObject var analytics: AnalyticsViewModel
  @State private var client: ClientKind = .claude
  @State private var selectedModelID = ""

  public var body: some View {
    ScrollView {
      VStack(alignment: .leading, spacing: 18) {
        PageTitle(
          title: "Client Setup",
          subtitle: "Copy safe commands for clients that connect to this Vekil instance."
        )

        if let baseURL = app.baseURL {
          GroupBox("Local endpoint") {
            HStack(spacing: 12) {
              Label(baseURL.absoluteString, systemImage: "network")
                .font(.callout.monospaced())
                .textSelection(.enabled)
              Spacer()
              Button("Copy Base URL") {
                Task { await app.copyBaseURL() }
              }
            }
            .padding(.vertical, 6)
          }

          Picker("Client", selection: $client) {
            ForEach(ClientKind.allCases) { client in
              Text(client.rawValue).tag(client)
            }
          }
          .pickerStyle(.segmented)

          GroupBox("Model") {
            if modelIDs.isEmpty {
              Text(
                "The running proxy has not published a model catalog yet. Replace <model-id> in the commands below after choosing a model from /v1/models."
              )
              .foregroundStyle(.secondary)
              .padding(.vertical, 6)
            } else {
              Picker("Public model ID", selection: $selectedModelID) {
                ForEach(modelIDs, id: \.self) { modelID in
                  Text(modelID).tag(modelID)
                }
              }
              .labelsHidden()
              .frame(maxWidth: 420, alignment: .leading)

              if let model = selectedModel {
                Text("Native endpoints: \(endpointSummary(model))")
                  .font(.caption)
                  .foregroundStyle(.secondary)
              }
            }
          }

          CommandGroup(
            title: "Launch with Vekil",
            description:
              "Vekil supervises an ephemeral proxy and removes the temporary client overlay when the session ends.",
            command: launchCommand,
            copyAction: { Task { await app.copyText(launchCommand) } }
          )

          CommandGroup(
            title: "Connect to this running proxy",
            description:
              "Use these temporary environment variables when you want to keep the current app-managed proxy running.",
            command: manualCommand(baseURL),
            copyAction: { Task { await app.copyText(manualCommand(baseURL)) } }
          )

          Label(
            "The macOS app does not rewrite your client configuration or dotfiles.",
            systemImage: "lock.shield"
          )
          .font(.caption)
          .foregroundStyle(.secondary)
        } else {
          VStack(spacing: 16) {
            EmptyState(
              title: "Start the proxy first",
              message:
                "Client commands use the active local endpoint, which is assigned when the proxy starts.",
              systemImage: "terminal"
            )
            Button(app.primaryAction.title) {
              Task { await app.performPrimaryAction() }
            }
            .buttonStyle(.borderedProminent)
            .disabled(!app.primaryAction.isEnabled)
          }
          .frame(maxWidth: .infinity, minHeight: 360)
        }
      }
      .padding(24)
    }
    .onAppear {
      analytics.setVisible(.providers, true)
      chooseDefaultModelIfNeeded()
    }
    .onDisappear { analytics.setVisible(.providers, false) }
    .onChange(of: modelIDs) { _ in
      chooseDefaultModelIfNeeded()
    }
  }

  private var models: [RuntimeModel] {
    (analytics.state.models.capture?.catalog.data ?? [])
      .filter { !$0.id.isEmpty && $0.modelPickerEnabled != false }
      .sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
  }

  private var modelIDs: [String] {
    models.map(\.id)
  }

  private var selectedModel: RuntimeModel? {
    models.first { $0.id == selectedModelID }
  }

  private var commandModelID: String {
    ShellArgument.quote(selectedModelID.isEmpty ? "<model-id>" : selectedModelID)
  }

  private var launchCommand: String {
    "vekil launch \(client.commandName) --model \(commandModelID)"
  }

  private func manualCommand(_ baseURL: URL) -> String {
    let base = baseURL.absoluteString.trimmingCharacters(in: CharacterSet(charactersIn: "/"))
    switch client {
    case .claude:
      return """
        env ANTHROPIC_BASE_URL=\(base) \\
          ANTHROPIC_API_KEY=dummy \\
          claude --model \(commandModelID)
        """
    case .codex:
      return """
        env OPENAI_API_KEY=dummy \\
          OPENAI_BASE_URL=\(base)/v1 \\
          codex -m \(commandModelID)
        """
    case .copilot:
      return """
        env COPILOT_PROVIDER_BASE_URL=\(base)/v1 \\
          COPILOT_PROVIDER_TYPE=openai \\
          COPILOT_PROVIDER_WIRE_API=responses \\
          COPILOT_MODEL=\(commandModelID) \\
          COPILOT_OFFLINE=true copilot
        """
    }
  }

  private func chooseDefaultModelIfNeeded() {
    guard !modelIDs.contains(selectedModelID) else { return }
    selectedModelID = modelIDs.first ?? ""
  }
}

private struct CommandGroup: View {
  let title: String
  let description: String
  let command: String
  let copyAction: () -> Void

  var body: some View {
    GroupBox(title) {
      VStack(alignment: .leading, spacing: 10) {
        HStack(alignment: .firstTextBaseline, spacing: 12) {
          Text(description)
            .font(.caption)
            .foregroundStyle(.secondary)
          Spacer()
          Button("Copy", action: copyAction)
            .controlSize(.small)
        }
        ScrollView(.horizontal) {
          Text(command)
            .font(.callout.monospaced())
            .textSelection(.enabled)
            .padding(12)
            .frame(maxWidth: .infinity, alignment: .leading)
        }
        .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))
      }
      .padding(.vertical, 6)
    }
  }
}

public struct ProvidersView: View {
  @ObservedObject var app: VekilAppState
  @ObservedObject var analytics: AnalyticsViewModel

  public var body: some View {
    Form {
      Section("Supported providers") {
        LazyVGrid(
          columns: [GridItem(.adaptive(minimum: 210), spacing: 12)],
          alignment: .leading,
          spacing: 12
        ) {
          ForEach(VekilOnboardingProvider.providerCases) { provider in
            HStack(spacing: 10) {
              Image(systemName: provider.systemImage)
                .foregroundStyle(Color.accentColor)
                .frame(width: 22)
              VStack(alignment: .leading, spacing: 2) {
                Text(provider.title)
                  .fontWeight(.medium)
                Text(provider.shortRequirement)
                  .font(.caption)
                  .foregroundStyle(.secondary)
                  .lineLimit(2)
              }
            }
            .padding(10)
            .frame(maxWidth: .infinity, alignment: .leading)
            .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))
          }
        }

        Text(
          "One JSON or YAML configuration can connect several providers. Public model IDs remain global, and Vekil rejects collisions during startup."
        )
        .font(.caption)
        .foregroundStyle(.secondary)
      }

      Section("Configuration") {
        LabeledContent("Mode", value: configurationMode)
        LabeledContent("Selected", value: selectedConfiguration)
        LabeledContent(
          "On-disk revision",
          value: app.runtimeState.configuration.selectedRevision ?? "—"
        )
        LabeledContent(
          "Active revision",
          value: app.runtimeState.configuration.activeRevision ?? "—"
        )

        HStack {
          Button("Choose External Configuration…") {
            Task { await app.chooseExternalConfiguration() }
          }
          Button("Reload and Restart") {
            Task { await app.reloadExternalConfiguration() }
          }
          .disabled(app.runtimeState.configuration.mode != .external)
          Button("Use Default / Managed") {
            Task { await app.clearExternalConfiguration() }
          }
        }

        Text(
          "Swift passes only the selected path. External JSON or YAML remains user-owned and is never rewritten by the app."
        )
        .font(.caption)
        .foregroundStyle(.secondary)
      }

      Section("GitHub Account") {
        LabeledContent("Status", value: authenticationSummary(app.runtimeState))

        if let notice = app.environmentTokenSignOutNotice {
          Text(notice)
            .foregroundStyle(.orange)
        }

        authenticationActions

        if let code = app.deviceCode {
          LabeledContent("Device code") {
            HStack {
              Text(code.userCode)
                .font(.body.monospaced())
                .textSelection(.enabled)
              Button("Copy") {
                Task { await app.copyDeviceCode() }
              }
              Button("Open GitHub") {
                Task { await app.openDeviceVerificationPage() }
              }
            }
          }
        }
      }

      Section("Advanced Provider Configuration") {
        Text(
          "Routes, failover, policy profiles, custom headers and paths, trust metadata, and tool optimizers remain External Configuration concerns."
        )
        .foregroundStyle(.secondary)
        Text(
          "Native API-key entry remains unavailable until signed cross-version Keychain continuity is release-qualified."
        )
          .font(.caption)
          .foregroundStyle(.secondary)
      }
    }
    .formStyle(.grouped)
    .padding(24)
    .onAppear { analytics.setVisible(.providers, true) }
    .onDisappear { analytics.setVisible(.providers, false) }
  }

  @ViewBuilder
  private var authenticationActions: some View {
    if !app.runtimeState.configuration.requiresGitHubAuthentication {
      Text("This configuration does not require GitHub authentication.")
        .foregroundStyle(.secondary)
    } else {
      switch app.runtimeState.authentication.state {
      case .signedIn:
        if app.runtimeState.authentication.source != .environment {
          Button("Sign Out") {
            Task { await app.signOut() }
          }
          .disabled(app.isSubmittingCommand)
        }
      case .signingIn:
        HStack {
          ProgressView()
            .controlSize(.small)
          Text("Waiting for GitHub sign in…")
            .foregroundStyle(.secondary)
        }
      default:
        HStack {
          Button("Sign In with GitHub…") {
            Task { await app.signInWithGitHub() }
          }
          .buttonStyle(.borderedProminent)
          Button("Use GitHub CLI Account") {
            Task { await app.signInWithGitHubCLI() }
          }
        }
        .disabled(app.isSubmittingCommand)
      }
    }
  }

  private var configurationMode: String {
    switch app.runtimeState.configuration.mode {
    case .external:
      return "External Configuration"
    case .managed:
      return "Managed"
    case .legacy:
      return "Copilot default"
    default:
      return humanized(app.runtimeState.configuration.mode.rawValue)
    }
  }

  private var selectedConfiguration: String {
    app.runtimeState.configuration.selectedExternalPath
      ?? app.runtimeState.configuration.displayName
  }
}

public struct SettingsView: View {
  @ObservedObject var app: VekilAppState

  public var body: some View {
    Form {
      Section("Startup") {
        Toggle(
          "Open at Login",
          isOn: Binding(
            get: { app.openAtLogin },
            set: { value in Task { await app.setOpenAtLogin(value) } }
          )
        )
        if app.loginItemStatus == .requiresApproval {
          Text("macOS requires approval before Vekil can open at login.")
            .foregroundStyle(.orange)
          Button("Open Login Items Settings") {
            Task { await app.openLoginItemSettings() }
          }
        }
        Toggle(
          "Start proxy when the app launches",
          isOn: Binding(
            get: { app.startProxyWhenAppLaunches },
            set: { app.setStartProxyWhenAppLaunches($0) }
          )
        )
        Text("These settings are independent and both default off.")
          .font(.caption)
          .foregroundStyle(.secondary)
      }
      Section("Setup Assistant") {
        Text(
          "Run the guided provider, model, verification, and client setup again. The current proxy changes only after you confirm a configuration action."
        )
        .foregroundStyle(.secondary)
        Button("Run Setup Assistant…") {
          app.showOnboarding()
        }
      }
    }
    .formStyle(.grouped)
    .padding(24)
  }
}

public struct AboutView: View {
  @ObservedObject var app: VekilAppState

  public var body: some View {
    Form {
      Section {
        HStack(spacing: 16) {
          Image(systemName: "bolt.horizontal.circle.fill")
            .font(.system(size: 44))
            .foregroundStyle(Color.accentColor)
          VStack(alignment: .leading, spacing: 3) {
            Text("Vekil")
              .font(.title2.weight(.semibold))
            Text("A local multi-provider AI gateway for macOS.")
              .foregroundStyle(.secondary)
          }
        }
        .padding(.vertical, 8)
      }

      Section("Version") {
        LabeledContent("App version", value: app.applicationVersion)
        LabeledContent("Bundle build ID", value: app.bundleBuildID ?? "—")
        LabeledContent("Helper build", value: app.helperBuild ?? "—")
        LabeledContent(
          "Helper epoch",
          value: app.runtimeState.helperEpoch.isEmpty
            ? "Not connected"
            : app.runtimeState.helperEpoch
        )
        LabeledContent(
          "Configuration revision",
          value: app.runtimeState.configRevision ?? "—"
        )
      }

      Section("Updates") {
        Button("Check for Updates…") {
          Task { await app.checkForUpdates() }
        }
        .disabled(!app.updaterAvailable)
      }
    }
    .formStyle(.grouped)
    .padding(24)
  }
}

private func configurationSummary(_ configuration: AppRuntimeConfigurationState) -> String {
  let name = configuration.displayName
  switch configuration.drift {
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
    return "\(name) — \(humanized(configuration.drift.rawValue).lowercased())"
  }
}

private func authenticationSummary(_ state: AppRuntimeStateSnapshot) -> String {
  guard state.configuration.requiresGitHubAuthentication else {
    return "Not required for this configuration"
  }

  switch state.authentication.state {
  case .notRequired:
    return "Not required for this configuration"
  case .signedOut:
    return "Not signed in"
  case .signingIn:
    return "Signing in…"
  case .failed:
    return "Sign-in failed"
  case .signedIn:
    switch state.authentication.source {
    case .environment:
      return "Using an environment token"
    case .vekil:
      return "Signed in with Vekil"
    case .githubCLI:
      return "Using the GitHub CLI account"
    case .none:
      return "Signed in"
    default:
      return "Signed in via \(humanized(state.authentication.source.rawValue).lowercased())"
    }
  default:
    return humanized(state.authentication.state.rawValue)
  }
}

private func endpointSummary(_ model: RuntimeModel) -> String {
  model.supportedEndpoints.isEmpty
    ? "Not reported"
    : model.supportedEndpoints.joined(separator: ", ")
}

private func contextWindow(_ model: RuntimeModel) -> String {
  let values = [
    model.contextWindow,
    model.maximumContextWindow,
    model.capabilities?.limits.maximumContextWindowTokens,
    model.capabilities?.limits.contextWindow,
    model.capabilities?.limits.contextWindowTokens,
  ]
  .compactMap { $0 }
  .filter { $0 > 0 }

  guard let value = values.max() else { return "—" }
  return value.formatted(.number.notation(.compactName))
}

private func capabilitySummary(_ model: RuntimeModel) -> String {
  guard let supports = model.capabilities?.supports else { return "Not reported" }
  var capabilities: [String] = []
  if supports.toolCalls { capabilities.append("Tools") }
  if supports.parallelToolCalls { capabilities.append("Parallel tools") }
  if supports.vision { capabilities.append("Vision") }
  if !supports.reasoningEffort.isEmpty { capabilities.append("Reasoning") }
  return capabilities.isEmpty ? "Text" : capabilities.joined(separator: ", ")
}

private func routeSummary(_ request: StatsRecentRequest) -> String {
  let route = request.routeID.isEmpty ? request.provider : request.routeID
  if !route.isEmpty, !request.finalTarget.isEmpty, route != request.finalTarget {
    return "\(route) → \(request.finalTarget)"
  }
  if !route.isEmpty { return route }
  if !request.finalTarget.isEmpty { return request.finalTarget }
  return "—"
}

private func durationLabel(_ milliseconds: Int64) -> String {
  if milliseconds >= 1_000 {
    return String(format: "%.1f s", Double(milliseconds) / 1_000)
  }
  return "\(milliseconds.formatted()) ms"
}

private func errorRate(_ totals: StatsTotals) -> String {
  guard totals.requests > 0 else { return "0% of requests" }
  let rate = Double(totals.errors) / Double(totals.requests)
  return "\(rate.formatted(.percent.precision(.fractionLength(1)))) of requests"
}

private func displayValue(_ value: String) -> String {
  value.isEmpty ? "—" : value
}

private func humanized(_ value: String) -> String {
  value
    .replacingOccurrences(of: "_", with: " ")
    .replacingOccurrences(of: "-", with: " ")
    .capitalized
}
