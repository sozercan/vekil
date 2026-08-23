import SwiftUI
import VekilCore

public struct VekilMenuBarPopoverView: View {
  @ObservedObject private var app: VekilAppState
  @ObservedObject private var analytics: AnalyticsViewModel

  private let openMainWindow: () -> Void
  private let openSettings: () -> Void
  private let quit: () -> Void

  public init(
    app: VekilAppState,
    analytics: AnalyticsViewModel,
    openMainWindow: @escaping () -> Void,
    openSettings: @escaping () -> Void,
    quit: @escaping () -> Void
  ) {
    self.app = app
    self.analytics = analytics
    self.openMainWindow = openMainWindow
    self.openSettings = openSettings
    self.quit = quit
  }

  public var body: some View {
    VStack(spacing: 0) {
      header
      if let warning = app.lastError?.userMessage ?? app.environmentTokenSignOutNotice {
        Divider()
        Label(warning, systemImage: "exclamationmark.triangle.fill")
          .font(.caption)
          .foregroundStyle(.orange)
          .frame(maxWidth: .infinity, alignment: .leading)
          .padding(12)
      }
      Divider()
      endpoint
      Divider()
      activitySummary
      Divider()
      actions
    }
    .frame(width: 340)
    .background(.regularMaterial)
  }

  private var header: some View {
    HStack(spacing: 12) {
      Image(systemName: statusSymbol)
        .font(.title2)
        .foregroundStyle(statusColor)
        .accessibilityHidden(true)
      VStack(alignment: .leading, spacing: 2) {
        Text(app.presentation.title).fontWeight(.semibold)
        Text(app.runtimeState.configuration.displayName)
          .font(.caption)
          .foregroundStyle(.secondary)
          .lineLimit(1)
      }
      Spacer()
      Button(app.primaryAction.title) {
        Task { await app.performPrimaryAction() }
      }
      .disabled(!app.primaryAction.isEnabled)
      .controlSize(.small)
    }
    .padding(16)
  }

  @ViewBuilder
  private var endpoint: some View {
    if let baseURL = app.baseURL {
      HStack(spacing: 8) {
        Image(systemName: "link").foregroundStyle(.secondary)
        Text(baseURL.absoluteString)
          .font(.system(.caption, design: .monospaced))
          .lineLimit(1)
          .truncationMode(.middle)
        Spacer()
        Button("Copy Base URL") { Task { await app.copyBaseURL() } }
          .buttonStyle(.borderless)
      }
      .padding(12)
    } else {
      Label("Start the proxy to expose a local endpoint", systemImage: "link.badge.plus")
        .font(.caption)
        .foregroundStyle(.secondary)
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(12)
    }
  }

  private var activitySummary: some View {
    HStack(spacing: 10) {
      Image(systemName: "waveform.path.ecg")
        .foregroundStyle(.secondary)
      VStack(alignment: .leading, spacing: 2) {
        Text("Current run")
          .font(.caption)
          .foregroundStyle(.secondary)
        Text(activitySummaryText)
          .font(.callout)
          .monospacedDigit()
      }
      Spacer()
    }
    .padding(12)
    .accessibilityElement(children: .combine)
  }

  private var actions: some View {
    VStack(spacing: 2) {
      popoverAction(
        "Open Vekil", symbol: "macwindow", key: "o", shortcut: "⌘O", action: openMainWindow)
      popoverAction(
        "Settings…", symbol: "gearshape", key: ",", shortcut: "⌘,", action: openSettings)
      popoverAction(
        "Quit Vekil", symbol: "power", key: "q", shortcut: "⌘Q", action: quit)
    }
    .padding(8)
  }

  private func popoverAction(
    _ title: String,
    symbol: String,
    key: KeyEquivalent,
    shortcut: String,
    action: @escaping () -> Void
  ) -> some View {
    Button(action: action) {
      HStack(spacing: 9) {
        Image(systemName: symbol).frame(width: 18)
        Text(title)
        Spacer()
        Text(shortcut).foregroundStyle(.tertiary)
      }
      .contentShape(Rectangle())
      .padding(.horizontal, 8)
      .padding(.vertical, 6)
    }
    .buttonStyle(.plain)
    .keyboardShortcut(key, modifiers: .command)
    .accessibilityLabel(title)
  }

  private var snapshot: StatsSnapshot? {
    analytics.state.snapshotState.activeCapture?.snapshot
  }

  private var activitySummaryText: String {
    guard let totals = snapshot?.totals else {
      return "No activity yet"
    }
    return "\(compact(totals.requests)) requests · \(compact(totals.errors)) errors · \(compact(totals.totalTokens)) tokens"
  }

  private var statusSymbol: String {
    switch app.presentation.kind {
    case .ready: "bolt.horizontal.circle.fill"
    case .starting, .initializing: "progress.indicator"
    case .failed, .helperUnavailable, .authenticationRequired: "exclamationmark.triangle.fill"
    default: "bolt.horizontal.circle"
    }
  }

  private var statusColor: Color {
    switch app.presentation.kind {
    case .ready: .green
    case .failed, .helperUnavailable, .authenticationRequired: .orange
    default: .secondary
    }
  }

  private func compact(_ value: Int64) -> String {
    value.formatted(.number.notation(.compactName))
  }
}
