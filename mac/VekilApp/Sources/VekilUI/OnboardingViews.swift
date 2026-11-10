import SwiftUI
import VekilCore

public enum VekilOnboardingProvider: String, CaseIterable, Identifiable, Sendable {
  case githubCopilot
  case openAICodex
  case azureOpenAI
  case openAICompatible
  case anthropicCompatible
  case importedConfiguration

  public var id: String { rawValue }

  static let providerCases: [Self] = [
    .githubCopilot,
    .openAICodex,
    .azureOpenAI,
    .openAICompatible,
    .anthropicCompatible,
  ]

  var requiresConfigurationFile: Bool { self != .githubCopilot }

  var title: String {
    switch self {
    case .githubCopilot: return "GitHub Copilot"
    case .openAICodex: return "OpenAI Codex"
    case .azureOpenAI: return "Azure OpenAI"
    case .openAICompatible: return "OpenAI-compatible"
    case .anthropicCompatible: return "Anthropic-compatible"
    case .importedConfiguration: return "Import Configuration"
    }
  }

  var subtitle: String {
    switch self {
    case .githubCopilot:
      return "Zero-config setup with a GitHub account that has Copilot access."
    case .openAICodex:
      return "Use your existing Codex CLI ChatGPT authentication and Responses-native models."
    case .azureOpenAI:
      return "Connect deployment-backed models with an API key or Microsoft Entra authentication."
    case .openAICompatible:
      return "Connect OpenAI, Ollama, vLLM, or another compatible hosted or local endpoint."
    case .anthropicCompatible:
      return "Connect an Anthropic Messages-compatible hosted or local endpoint."
    case .importedConfiguration:
      return "Use one JSON or YAML file for several providers, routes, and failover."
    }
  }

  var shortRequirement: String {
    switch self {
    case .githubCopilot: return "GitHub sign-in"
    case .openAICodex: return "Codex CLI auth file"
    case .azureOpenAI: return "Deployment + auth"
    case .openAICompatible: return "Base URL + auth"
    case .anthropicCompatible: return "Base URL + auth"
    case .importedConfiguration: return "JSON or YAML"
    }
  }

  var configurationRequirement: String {
    switch self {
    case .githubCopilot:
      return "No provider file is required. Vekil uses its built-in Copilot routing."
    case .openAICodex:
      return "Choose a configuration containing a provider with type openai-codex. Vekil uses the existing Codex CLI ChatGPT auth file; no API key is copied into the app."
    case .azureOpenAI:
      return "Choose a configuration containing type azure-openai, its Azure endpoint and deployments, and either API-key or Entra authentication settings."
    case .openAICompatible:
      return "Choose a configuration containing type openai-compatible, a base URL, authentication settings, and dynamic or static model discovery."
    case .anthropicCompatible:
      return "Choose a configuration containing type anthropic-compatible, a base URL, authentication settings, and the public models it exposes."
    case .importedConfiguration:
      return "Choose any valid Vekil JSON or YAML provider configuration. A single file can contain several providers and advanced routing."
    }
  }

  var systemImage: String {
    switch self {
    case .githubCopilot: return "bolt.horizontal.circle"
    case .openAICodex: return "chevron.left.forwardslash.chevron.right"
    case .azureOpenAI: return "cloud"
    case .openAICompatible: return "point.3.connected.trianglepath.dotted"
    case .anthropicCompatible: return "text.bubble"
    case .importedConfiguration: return "doc.badge.gearshape"
    }
  }

  var badge: String {
    switch self {
    case .githubCopilot: return "Built in"
    case .importedConfiguration: return "Multi-provider"
    default: return "Configuration"
    }
  }
}

public enum VekilOnboardingStage: String, CaseIterable, Identifiable, Sendable {
  case providers
  case models
  case verification
  case client
  case ready

  public var id: String { rawValue }
}

func onboardingConfigurationIsReady(
  provider: VekilOnboardingProvider,
  configuration: AppRuntimeConfigurationState
) -> Bool {
  guard configuration.drift == .none else { return false }
  if let activeRevision = configuration.activeRevision,
    activeRevision != configuration.selectedRevision
  {
    return false
  }
  if provider == .githubCopilot {
    return configuration.mode != .external
  }
  return configuration.mode == .external
    && configuration.selectedExternalPath != nil
}

/// First-run setup for the native configuration paths that can ship safely.
/// External provider files remain user-owned and are validated by the runtime.
public struct VekilOnboardingView: View {
  @ObservedObject private var app: VekilAppState
  @ObservedObject private var analytics: AnalyticsViewModel

  private let onComplete: () -> Void
  private let onDefer: () -> Void

  @State private var stage: VekilOnboardingStage
  @State private var provider: VekilOnboardingProvider
  @State private var providerWasChosen = false
  @State private var providerActionInFlight = false
  @State private var authenticationActionInFlight = false
  @State private var refreshInFlight = false
  @State private var client: OnboardingClient = .claudeCode
  @State private var selectedModelID = ""

  public init(
    app: VekilAppState,
    analytics: AnalyticsViewModel,
    initialStage: VekilOnboardingStage = .providers,
    onComplete: @escaping () -> Void,
    onDefer: @escaping () -> Void
  ) {
    self.app = app
    self.analytics = analytics
    self.onComplete = onComplete
    self.onDefer = onDefer
    _stage = State(initialValue: initialStage)
    _provider = State(
      initialValue: app.runtimeState.configuration.mode == .external
        ? .importedConfiguration : .githubCopilot
    )
  }

  public var body: some View {
    VStack(spacing: 0) {
      progressHeader
      Divider()

      ScrollView {
        VStack(alignment: .leading, spacing: 20) {
          if let error = app.lastError {
            OnboardingNotice(
              title: "Vekil Needs Attention",
              message: error.userMessage,
              systemImage: "exclamationmark.triangle.fill"
            ) {
              app.dismissError()
            }
          }

          if app.initializationState == .failed {
            initializationRecovery
          }

          stageContent
        }
        .frame(maxWidth: 780, alignment: .leading)
        .padding(.horizontal, 34)
        .padding(.vertical, 28)
        .frame(maxWidth: .infinity)
      }

      Divider()
      footer
    }
    .frame(minWidth: 800, minHeight: 600)
    .onAppear {
      synchronizeProviderIfNeeded()
      chooseDefaultModelIfNeeded()
      analytics.setVisible(.providers, true)
    }
    .onDisappear {
      analytics.setVisible(.providers, false)
    }
    .onChange(of: app.runtimeState.configuration.mode) { _ in
      synchronizeProviderIfNeeded()
    }
    .onChange(of: modelIDs) { _ in
      chooseDefaultModelIfNeeded()
    }
  }

  private var initializationRecovery: some View {
    GroupBox {
      HStack(spacing: 12) {
        VStack(alignment: .leading, spacing: 4) {
          Text("The runtime did not initialize.")
            .font(.headline)
          Text("Retry after resolving the reported issue. Setup will continue here.")
            .font(.callout)
            .foregroundStyle(.secondary)
        }
        Spacer()
        Button("Retry Initialization") {
          Task { _ = await app.initialize() }
        }
        .buttonStyle(.borderedProminent)
      }
      .padding(.vertical, 4)
    } label: {
      Label("Runtime Recovery", systemImage: "arrow.clockwise.circle")
    }
  }

  private var progressHeader: some View {
    VStack(alignment: .leading, spacing: 9) {
      HStack(alignment: .firstTextBaseline) {
        Text(stageTitle)
          .font(.headline)
        Spacer()
        Text("Step \(activeStageNumber) of \(VekilOnboardingStage.allCases.count)")
          .font(.caption)
          .foregroundStyle(.secondary)
      }

      ProgressView(
        value: Double(activeStageNumber),
        total: Double(VekilOnboardingStage.allCases.count)
      )
      .accessibilityLabel("Setup progress")
      .accessibilityValue(
        "Step \(activeStageNumber) of \(VekilOnboardingStage.allCases.count), \(stageTitle)"
      )
    }
    .padding(.horizontal, 24)
    .padding(.vertical, 16)
  }

  @ViewBuilder
  private var stageContent: some View {
    switch stage {
    case .providers:
      providersStage
    case .models:
      modelsStage
    case .verification:
      verificationStage
    case .client:
      clientStage
    case .ready:
      readyStage
    }
  }

  private var providersStage: some View {
    VStack(alignment: .leading, spacing: 20) {
      OnboardingPageHeading(
        title: "Connect Your Providers",
        subtitle:
          "Choose where Vekil gets models. You can start with one provider or use a configuration that connects several."
      )

      LazyVGrid(
        columns: [
          GridItem(.flexible(), spacing: 12),
          GridItem(.flexible(), spacing: 12),
          GridItem(.flexible(), spacing: 12),
        ],
        spacing: 12
      ) {
        ForEach(VekilOnboardingProvider.allCases) { candidate in
          providerCard(candidate)
        }
      }

      if provider == .githubCopilot {
        authenticationPanel
        githubCopilotPanel
      } else {
        externalProviderPanel
        if requiresAuthenticationForFlow {
          authenticationPanel
        }
      }
    }
  }

  private func providerCard(_ candidate: VekilOnboardingProvider) -> some View {
    let selected = provider == candidate
    return Button {
      providerWasChosen = true
      provider = candidate
    } label: {
      VStack(alignment: .leading, spacing: 10) {
        HStack(alignment: .top) {
          Image(systemName: candidate.systemImage)
            .font(.title2)
            .foregroundStyle(selected ? Color.accentColor : Color.secondary)
            .frame(width: 28)
          Spacer()
          Text(candidate.badge)
            .font(.caption2.weight(.semibold))
            .foregroundStyle(.secondary)
            .padding(.horizontal, 7)
            .padding(.vertical, 3)
            .background(.quaternary, in: Capsule())
        }

        Text(candidate.title)
          .font(.headline)
          .foregroundStyle(.primary)

        Text(candidate.subtitle)
          .font(.caption)
          .foregroundStyle(.secondary)
          .fixedSize(horizontal: false, vertical: true)

        HStack {
          Text(candidate.shortRequirement)
            .font(.caption2)
            .foregroundStyle(.secondary)
          Spacer()
          Image(systemName: selected ? "checkmark.circle.fill" : "circle")
            .foregroundStyle(selected ? Color.accentColor : Color.secondary)
        }
      }
      .padding(14)
      .frame(maxWidth: .infinity, minHeight: 154, alignment: .topLeading)
      .background(
        selected ? Color.accentColor.opacity(0.08) : Color.secondary.opacity(0.045),
        in: RoundedRectangle(cornerRadius: 11)
      )
      .overlay {
        RoundedRectangle(cornerRadius: 11)
          .stroke(
            selected ? Color.accentColor.opacity(0.75) : Color.secondary.opacity(0.18),
            lineWidth: selected ? 2 : 1
          )
      }
      .contentShape(Rectangle())
    }
    .buttonStyle(.plain)
    .accessibilityLabel(candidate.title)
    .accessibilityHint(candidate.subtitle)
    .accessibilityValue(selected ? "Selected" : "Not selected")
  }

  private var githubCopilotPanel: some View {
    GroupBox {
      VStack(alignment: .leading, spacing: 10) {
        OnboardingStatusRow(
          label: "Configuration",
          value: app.runtimeState.configuration.mode == .external
            ? "Switch to Copilot default" : "Copilot default",
          state: configurationIsReady ? .success : configurationOperationIsBusy ? .working : .pending
        )

        Text(provider.configurationRequirement)
          .font(.caption)
          .foregroundStyle(.secondary)

        if app.runtimeState.configuration.mode == .external {
          Label(
            "Continuing returns to Vekil's preferred app configuration. The selected external file is not changed or deleted.",
            systemImage: "info.circle"
          )
          .font(.caption)
          .foregroundStyle(.secondary)
        }
      }
      .padding(.vertical, 4)
    } label: {
      Label("Quick Setup", systemImage: "bolt.horizontal")
    }
  }

  private var externalProviderPanel: some View {
    GroupBox {
      VStack(alignment: .leading, spacing: 12) {
        Text(provider.configurationRequirement)
          .font(.callout)
          .fixedSize(horizontal: false, vertical: true)

        OnboardingStatusRow(
          label: "Configuration file",
          value: selectedConfigurationName,
          state: configurationIsReady
            ? .success : configurationOperationIsBusy ? .working : .pending
        )

        if let path = app.runtimeState.configuration.selectedExternalPath {
          Text(path)
            .font(.caption.monospaced())
            .foregroundStyle(.secondary)
            .lineLimit(2)
            .truncationMode(.middle)
            .textSelection(.enabled)
        }

        HStack(spacing: 10) {
          Button(externalConfigurationIsSelected ? "Choose a Different File…" : "Choose Configuration…") {
            chooseExternalConfiguration()
          }
          .disabled(providerActionInFlight || app.isSubmittingCommand)

          if configurationOperationIsBusy {
            ProgressView()
              .controlSize(.small)
            Text("Validating configuration…")
              .font(.caption)
              .foregroundStyle(.secondary)
          }
        }

        Label(
          "The runtime validates the selected path. Vekil never imports, normalizes, or rewrites the file.",
          systemImage: "lock.shield"
        )
        .font(.caption)
        .foregroundStyle(.secondary)

        if provider != .openAICodex {
          Text(
            "Native API-key entry remains unavailable until signed cross-version Keychain continuity is release-qualified. Credentials stay in your existing configuration or environment."
          )
          .font(.caption)
          .foregroundStyle(.secondary)
        }
      }
      .padding(.vertical, 4)
    } label: {
      Label(provider.title, systemImage: provider.systemImage)
    }
  }

  private var authenticationPanel: some View {
    GroupBox {
      VStack(alignment: .leading, spacing: 14) {
        OnboardingStatusRow(
          label: "GitHub account",
          value: authenticationStatusLabel,
          state: authenticationReady ? .success : authenticationIsBusy ? .working : .pending
        )

        if authenticationReady {
          LabeledContent("Credential source", value: authenticationSourceLabel)
        } else {
          HStack(spacing: 10) {
            Button("Sign In with GitHub") {
              authenticateWithGitHub()
            }
            .buttonStyle(.borderedProminent)

            Button("Use GitHub CLI Account") {
              authenticateWithGitHubCLI()
            }

            if authenticationIsBusy {
              ProgressView()
                .controlSize(.small)
                .accessibilityLabel("Signing in")
            }
          }
          .disabled(authenticationIsBusy || app.isSubmittingCommand)

          Text(
            "Using the GitHub CLI account is explicit opt-in. Vekil reads it for Copilot access and keeps the token in memory only."
          )
          .font(.caption)
          .foregroundStyle(.secondary)
        }

        if let notice = app.environmentTokenSignOutNotice {
          Label(notice, systemImage: "info.circle")
            .font(.caption)
            .foregroundStyle(.secondary)
        }

        if let code = app.deviceCode {
          Divider()
          deviceCodePanel(code)
        }
      }
      .padding(.vertical, 4)
    } label: {
      Label("GitHub Authentication", systemImage: "person.crop.circle")
    }
  }

  private func deviceCodePanel(_ code: AppRuntimeDeviceCode) -> some View {
    VStack(alignment: .leading, spacing: 10) {
      Text(code.userCode)
        .font(.system(.title2, design: .monospaced).weight(.semibold))
        .textSelection(.enabled)
        .accessibilityLabel("GitHub device code \(code.userCode)")

      HStack {
        Button("Copy Code") {
          Task { await app.copyDeviceCode() }
        }
        Button("Open GitHub") {
          Task { await app.openDeviceVerificationPage() }
        }
        Spacer()
        Text("Expires \(code.expiresAt, style: .relative)")
          .font(.caption)
          .foregroundStyle(.secondary)
      }

      Text("Enter this code on GitHub, then return to Vekil. Setup updates when sign-in completes.")
        .font(.caption)
        .foregroundStyle(.secondary)
    }
  }

  private var modelsStage: some View {
    VStack(alignment: .leading, spacing: 20) {
      OnboardingPageHeading(
        title: "Discover Your Models",
        subtitle:
          "Vekil validates provider access and builds one public catalog across every configured provider."
      )

      Label(
        "Public model IDs are global. Startup fails on collisions instead of silently choosing one provider.",
        systemImage: "square.stack.3d.up"
      )
      .font(.callout)
      .foregroundStyle(.secondary)

      GroupBox {
        VStack(spacing: 0) {
          OnboardingStatusRow(
            label: "Providers",
            value: configurationStatusLabel,
            state: configurationIsReady
              ? .success : configurationOperationIsBusy ? .working : .warning
          )
          if requiresAuthenticationForFlow {
            Divider()
            OnboardingStatusRow(
              label: "Authentication",
              value: authenticationStatusLabel,
              state: authenticationReady ? .success : authenticationIsBusy ? .working : .warning
            )
          }
          Divider()
          OnboardingStatusRow(
            label: "Provider verification",
            value: providerVerificationLabel,
            state: verificationRowState
          )
          Divider()
          OnboardingStatusRow(
            label: "Model catalog",
            value: modelCatalogStatusLabel,
            state: modelCatalogRowState
          )
        }
        .padding(.vertical, 2)
      } label: {
        Label("Discovery", systemImage: "magnifyingglass")
      }

      if let operationDetail {
        HStack(spacing: 10) {
          ProgressView()
            .controlSize(.small)
          Text(operationDetail)
            .font(.callout)
          Spacer()
        }
        .accessibilityElement(children: .combine)
      }

      if !models.isEmpty {
        modelCatalogPreview
      } else if proxyIsRunning && verificationIsReady {
        GroupBox {
          VStack(alignment: .leading, spacing: 10) {
            Text("The proxy is ready while the model catalog refreshes.")
              .foregroundStyle(.secondary)
            Button("Refresh Catalog") {
              refreshModelCatalog()
            }
            .disabled(refreshInFlight || analytics.state.isRefreshingModels)
          }
          .padding(.vertical, 6)
        } label: {
          Label("Models", systemImage: "square.stack.3d.up")
        }
      }

      if case .failed(_, let failure) = analytics.state.models {
        OnboardingNotice(
          title: "Model Catalog Could Not Refresh",
          message: failure.message,
          systemImage: "exclamationmark.triangle.fill"
        ) {
          refreshModelCatalog()
        }
      }
    }
  }

  private var modelCatalogPreview: some View {
    GroupBox {
      VStack(spacing: 0) {
        ForEach(Array(models.prefix(7).enumerated()), id: \.element.id) { index, model in
          if index > 0 { Divider() }
          HStack(alignment: .firstTextBaseline, spacing: 12) {
            VStack(alignment: .leading, spacing: 2) {
              Text(model.name.isEmpty ? model.id : model.name)
                .fontWeight(.medium)
              if !model.name.isEmpty, model.name != model.id {
                Text(model.id)
                  .font(.caption.monospaced())
                  .foregroundStyle(.secondary)
              }
            }
            Spacer()
            VStack(alignment: .trailing, spacing: 2) {
              Text(model.ownedBy.isEmpty ? "Provider" : model.ownedBy)
                .font(.caption)
              Text(modelEndpointSummary(model))
                .font(.caption2)
                .foregroundStyle(.secondary)
                .lineLimit(1)
            }
          }
          .padding(.vertical, 9)
        }

        if models.count > 7 {
          Divider()
          Text("\(models.count - 7) more models are available in Connection → Models.")
            .font(.caption)
            .foregroundStyle(.secondary)
            .frame(maxWidth: .infinity, alignment: .leading)
            .padding(.vertical, 9)
        }
      }
    } label: {
      HStack {
        Label("Available Models", systemImage: "square.stack.3d.up")
        Spacer()
        Button("Refresh") {
          refreshModelCatalog()
        }
        .controlSize(.small)
        .disabled(refreshInFlight || analytics.state.isRefreshingModels)
      }
    }
  }

  private var verificationStage: some View {
    VStack(alignment: .leading, spacing: 20) {
      OnboardingPageHeading(
        title: "Verify the Connection",
        subtitle:
          "These checks come from the running proxy—not a simulated setup screen. Continue when the local endpoint is ready."
      )

      GroupBox {
        VStack(spacing: 0) {
          OnboardingStatusRow(
            label: "Configuration",
            value: configurationStatusLabel,
            state: configurationIsReady
              ? .success : configurationOperationIsBusy ? .working : .warning
          )
          Divider()
          OnboardingStatusRow(
            label: "Authentication",
            value: authenticationStatusLabel,
            state: authenticationReady ? .success : authenticationIsBusy ? .working : .warning
          )
          Divider()
          OnboardingStatusRow(
            label: "Proxy",
            value: app.presentation.title,
            state: verificationRowState
          )
          Divider()
          OnboardingStatusRow(
            label: "Endpoint",
            value: app.baseURL?.absoluteString ?? "Available after startup",
            state: app.baseURL == nil ? .pending : .success
          )
          Divider()
          OnboardingStatusRow(
            label: "Models",
            value: modelCatalogStatusLabel,
            state: modelCatalogRowState
          )
        }
        .padding(.vertical, 2)
      } label: {
        Label("Connection Test", systemImage: "checkmark.shield")
      }

      if let operationDetail {
        HStack(spacing: 10) {
          ProgressView()
            .controlSize(.small)
          Text(operationDetail)
            .font(.callout)
          Spacer()
        }
      }

      verificationActions
    }
  }

  @ViewBuilder
  private var verificationActions: some View {
    if requiresAuthenticationForFlow && !authenticationReady {
      authenticationPanel
    } else if proxyIsStarting {
      Button("Cancel Starting") {
        Task { await app.cancelStarting() }
      }
      .disabled(app.isSubmittingCommand || app.runtimeState.operation == nil)
    } else if proxyIsStopping {
      HStack(spacing: 10) {
        ProgressView()
          .controlSize(.small)
        Text("Waiting for the proxy to stop…")
          .foregroundStyle(.secondary)
      }
    } else if !proxyIsRunning {
      Button("Start Proxy") {
        Task { await app.startProxy() }
      }
      .buttonStyle(.borderedProminent)
      .disabled(!canStartProxy)
    } else if !verificationIsReady {
      Button("Refresh Status") {
        refreshRuntimeState()
      }
      .disabled(refreshInFlight)
    } else {
      Label("Vekil is ready for local clients.", systemImage: "checkmark.circle.fill")
        .foregroundStyle(.green)
    }
  }

  private var clientStage: some View {
    VStack(alignment: .leading, spacing: 20) {
      OnboardingPageHeading(
        title: "Connect a Client",
        subtitle:
          "Choose a client and copy a temporary setup command. Vekil does not rewrite your dotfiles or client configuration."
      )

      GroupBox {
        HStack(spacing: 12) {
          Text(baseURLString)
            .font(.body.monospaced())
            .textSelection(.enabled)
          Spacer()
          Button("Copy Base URL") {
            Task { await app.copyBaseURL() }
          }
          .disabled(app.baseURL == nil)
        }
        .padding(.vertical, 6)
      } label: {
        Label("Local Endpoint", systemImage: "network")
      }

      Picker("Client", selection: $client) {
        ForEach(OnboardingClient.allCases) { candidate in
          Text(candidate.title).tag(candidate)
        }
      }
      .pickerStyle(.segmented)
      .frame(maxWidth: 620)

      if !modelIDs.isEmpty {
        Picker("Model", selection: $selectedModelID) {
          ForEach(modelIDs, id: \.self) { modelID in
            Text(modelID).tag(modelID)
          }
        }
        .frame(maxWidth: 460, alignment: .leading)
      }

      GroupBox {
        VStack(alignment: .leading, spacing: 10) {
          Text(client.summary)
            .font(.callout)

          ScrollView(.horizontal) {
            Text(clientInstructions)
              .font(.callout.monospaced())
              .textSelection(.enabled)
              .padding(12)
              .frame(maxWidth: .infinity, alignment: .leading)
          }
          .background(.quaternary, in: RoundedRectangle(cornerRadius: 8))

          Button(client.copyButtonTitle) {
            Task { await app.copyText(clientInstructions) }
          }

          Text(client.footnote)
            .font(.caption)
            .foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
      } label: {
        Label(client.longTitle, systemImage: client.systemImage)
      }
    }
  }

  private var readyStage: some View {
    VStack(alignment: .leading, spacing: 20) {
      OnboardingPageHeading(
        title: "Vekil Is Ready",
        subtitle:
          "Setup is complete. Detailed provider, model, client, and activity controls remain available in the main window."
      )

      GroupBox {
        VStack(spacing: 0) {
          OnboardingStatusRow(label: "Provider setup", value: providerSummary, state: .success)
          Divider()
          OnboardingStatusRow(
            label: "Models",
            value: models.isEmpty ? "Catalog ready" : "\(models.count) available",
            state: .success
          )
          Divider()
          OnboardingStatusRow(
            label: "Proxy",
            value: app.presentation.title,
            state: verificationRowState
          )
          Divider()
          OnboardingStatusRow(
            label: "Endpoint",
            value: app.baseURL?.absoluteString ?? "Unavailable",
            state: app.baseURL == nil ? .warning : .success
          )
        }
        .padding(.vertical, 2)
      } label: {
        Label("Setup Summary", systemImage: "checkmark.circle")
      }

      GroupBox {
        VStack(alignment: .leading, spacing: 10) {
          Toggle(
            "Open Vekil at login",
            isOn: Binding(
              get: { app.openAtLogin },
              set: { value in Task { await app.setOpenAtLogin(value) } }
            )
          )
          Toggle(
            "Start proxy when Vekil launches",
            isOn: Binding(
              get: { app.startProxyWhenAppLaunches },
              set: { app.setStartProxyWhenAppLaunches($0) }
            )
          )
          Text("These choices are independent and can be changed later in Settings.")
            .font(.caption)
            .foregroundStyle(.secondary)
        }
        .padding(.vertical, 4)
      } label: {
        Label("Launch Options", systemImage: "power")
      }
    }
  }

  private var footer: some View {
    HStack(spacing: 12) {
      Button("Skip Setup", action: onDefer)

      Spacer()

      if stage != .providers {
        Button("Back", action: goBack)
      }

      Button(primaryButtonTitle, action: performPrimaryStageAction)
        .buttonStyle(.borderedProminent)
        .keyboardShortcut(.defaultAction)
        .disabled(primaryButtonDisabled)
    }
    .padding(.horizontal, 24)
    .padding(.vertical, 14)
  }

  private var activeStageNumber: Int {
    (VekilOnboardingStage.allCases.firstIndex(of: stage) ?? 0) + 1
  }

  private var stageTitle: String {
    switch stage {
    case .providers: return "Providers"
    case .models: return "Models"
    case .verification: return "Verify"
    case .client: return "Client"
    case .ready: return "Ready"
    }
  }

  private var primaryButtonTitle: String {
    switch stage {
    case .providers:
      if provider.requiresConfigurationFile && !externalConfigurationIsSelected {
        return "Choose Configuration…"
      }
      if provider == .githubCopilot && app.runtimeState.configuration.mode == .external {
        return "Use GitHub Copilot"
      }
      return "Continue"
    case .models:
      if proxyIsStarting { return "Discovering Models…" }
      if !proxyIsRunning { return "Start & Discover Models" }
      if !verificationIsReady { return "Refresh Status" }
      return "Continue"
    case .verification:
      return "Continue"
    case .client:
      return "Continue"
    case .ready:
      return "Finish Setup"
    }
  }

  private var primaryButtonDisabled: Bool {
    switch stage {
    case .providers:
      if providerActionInFlight || configurationOperationIsBusy
        || app.initializationState != .initialized
      {
        return true
      }
      if provider.requiresConfigurationFile && !externalConfigurationIsSelected {
        return false
      }
      return !configurationIsReady || !authenticationReady || authenticationIsBusy
    case .models:
      if proxyIsStarting || proxyIsStopping || app.isSubmittingCommand { return true }
      if !proxyIsRunning { return !canStartProxy }
      return false
    case .verification:
      return !verificationIsReady
    case .client, .ready:
      return false
    }
  }

  private func performPrimaryStageAction() {
    switch stage {
    case .providers:
      continueFromProviders()
    case .models:
      if !proxyIsRunning {
        Task { await app.startProxy() }
      } else if verificationIsReady {
        stage = .verification
      } else {
        refreshRuntimeState()
      }
    case .verification:
      if verificationIsReady {
        stage = .client
      }
    case .client:
      stage = .ready
    case .ready:
      onComplete()
    }
  }

  private func continueFromProviders() {
    if provider == .githubCopilot {
      guard app.runtimeState.configuration.mode == .external else {
        stage = .models
        return
      }
      providerActionInFlight = true
      Task {
        let accepted = await app.clearExternalConfiguration()
        providerActionInFlight = false
        if accepted { stage = .models }
      }
      return
    }

    guard externalConfigurationIsSelected else {
      chooseExternalConfiguration()
      return
    }
    stage = .models
  }

  private func chooseExternalConfiguration() {
    providerActionInFlight = true
    Task {
      _ = await app.chooseExternalConfiguration()
      providerActionInFlight = false
    }
  }

  private func authenticateWithGitHub() {
    authenticationActionInFlight = true
    Task {
      _ = await app.signInWithGitHub()
      authenticationActionInFlight = false
    }
  }

  private func authenticateWithGitHubCLI() {
    authenticationActionInFlight = true
    Task {
      _ = await app.signInWithGitHubCLI()
      authenticationActionInFlight = false
    }
  }

  private func refreshRuntimeState() {
    refreshInFlight = true
    Task {
      _ = await app.refreshRuntimeState()
      refreshInFlight = false
    }
  }

  private func refreshModelCatalog() {
    refreshInFlight = true
    Task {
      await analytics.store.manualRefresh()
      await analytics.reload()
      refreshInFlight = false
    }
  }

  private func goBack() {
    switch stage {
    case .providers:
      break
    case .models:
      stage = .providers
    case .verification:
      stage = .models
    case .client:
      stage = .verification
    case .ready:
      stage = .client
    }
  }

  private func synchronizeProviderIfNeeded() {
    guard stage == .providers, !providerWasChosen else { return }
    provider = app.runtimeState.configuration.mode == .external
      ? .importedConfiguration : .githubCopilot
  }

  private func chooseDefaultModelIfNeeded() {
    guard !modelIDs.contains(selectedModelID) else { return }
    selectedModelID = modelIDs.first ?? ""
  }

  private var requiresAuthenticationForFlow: Bool {
    if provider == .githubCopilot { return true }
    guard externalConfigurationIsSelected else { return false }
    return app.runtimeState.configuration.requiresGitHubAuthentication
  }

  private var authenticationReady: Bool {
    !requiresAuthenticationForFlow || app.runtimeState.authentication.state == .signedIn
  }

  private var authenticationIsBusy: Bool {
    authenticationActionInFlight
      || app.runtimeState.authentication.state == .signingIn
      || app.runtimeState.operation?.kind == .authDevice
      || app.runtimeState.operation?.kind == .authGitHubCLI
  }

  private var authenticationStatusLabel: String {
    if !requiresAuthenticationForFlow { return "Not required" }
    let state = app.runtimeState.authentication.state
    if state == .signedIn { return "Signed in" }
    if state == .signingIn { return "Signing in…" }
    if state == .failed { return "Sign-in failed" }
    return "Not signed in"
  }

  private var authenticationSourceLabel: String {
    let source = app.runtimeState.authentication.source
    if source == .environment { return "Environment token" }
    if source == .vekil { return "Vekil-managed sign in" }
    if source == .githubCLI { return "GitHub CLI" }
    return "GitHub"
  }

  private var externalConfigurationIsSelected: Bool {
    app.runtimeState.configuration.mode == .external
      && app.runtimeState.configuration.selectedExternalPath != nil
  }

  private var configurationIsReady: Bool {
    onboardingConfigurationIsReady(
      provider: provider,
      configuration: app.runtimeState.configuration
    )
  }

  private var configurationOperationIsBusy: Bool {
    let kind = app.runtimeState.operation?.kind
    return kind == .selectExternalConfig
      || kind == .reloadExternalConfig
      || kind == .clearExternalConfig
      || kind == .useManagedConfig
      || kind == .ensureManagedConfig
  }

  private var selectedConfigurationName: String {
    guard let path = app.runtimeState.configuration.selectedExternalPath else {
      return "None selected"
    }
    return URL(fileURLWithPath: path).lastPathComponent
  }

  private var configurationStatusLabel: String {
    if configurationOperationIsBusy { return "Applying selection…" }
    if !configurationIsReady { return "Needs attention" }
    return provider == .githubCopilot
      ? "GitHub Copilot"
      : app.runtimeState.configuration.displayName
  }

  private var proxyIsRunning: Bool {
    app.runtimeState.service == .running
  }

  private var proxyIsStarting: Bool {
    app.runtimeState.service == .starting
      || app.runtimeState.operation?.kind == .start
  }

  private var proxyIsStopping: Bool {
    app.runtimeState.service == .stopping
      || app.runtimeState.operation?.kind == .stop
      || app.runtimeState.operation?.kind == .restart
  }

  private var verificationIsReady: Bool {
    proxyIsRunning
      && app.runtimeState.readiness == .ready
      && configurationIsReady
      && authenticationReady
      && !configurationOperationIsBusy
      && app.runtimeState.operation == nil
      && !app.isSubmittingCommand
  }

  private var canStartProxy: Bool {
    app.initializationState == .initialized
      && app.runtimeState.helper == .connected
      && app.runtimeState.operation == nil
      && !app.isSubmittingCommand
      && configurationIsReady
      && authenticationReady
  }

  private var verificationRowState: OnboardingRowState {
    if verificationIsReady { return .success }
    if proxyIsStarting || proxyIsStopping || app.runtimeState.readiness == .checking {
      return .working
    }
    if app.runtimeState.service == .failed || app.runtimeState.readiness == .notReady {
      return .warning
    }
    return .pending
  }

  private var providerVerificationLabel: String {
    if verificationIsReady { return "Connected" }
    if proxyIsStarting || app.runtimeState.readiness == .checking { return "Checking…" }
    if app.runtimeState.service == .failed || app.runtimeState.readiness == .notReady {
      return "Connection failed"
    }
    return "Start Vekil to verify"
  }

  private var operationDetail: String? {
    if configurationOperationIsBusy { return "Applying configuration selection" }
    guard let phase = app.runtimeState.operation?.phase else {
      if proxyIsStarting { return "Starting proxy" }
      return nil
    }
    if phase == .loadingConfiguration { return "Loading configuration" }
    if phase == .constructingServer { return "Preparing server" }
    if phase == .listenerStartup { return "Starting local listener" }
    if phase == .startupAuthentication { return "Checking authentication" }
    if phase == .dynamicProviderModelValidation { return "Validating provider models" }
    if phase == .policyRoutingPreflight { return "Checking routing policy" }
    if phase == .readinessCheck { return "Checking readiness" }
    if phase == .cleanup { return "Cleaning up" }
    return phase.rawValue.replacingOccurrences(of: "_", with: " ").capitalized
  }

  private var models: [RuntimeModel] {
    (analytics.state.models.capture?.catalog.data ?? [])
      .filter { !$0.id.isEmpty && $0.modelPickerEnabled != false }
      .sorted { $0.id.localizedStandardCompare($1.id) == .orderedAscending }
  }

  private var modelIDs: [String] {
    models.map(\.id)
  }

  private var modelCatalogStatusLabel: String {
    if analytics.state.isRefreshingModels { return "Refreshing…" }
    if !models.isEmpty { return "\(models.count) available" }
    if case .failed = analytics.state.models { return "Refresh failed" }
    return proxyIsRunning ? "Waiting for catalog" : "Available after startup"
  }

  private var modelCatalogRowState: OnboardingRowState {
    if !models.isEmpty { return .success }
    if analytics.state.isRefreshingModels || proxyIsStarting { return .working }
    if case .failed = analytics.state.models { return .warning }
    return .pending
  }

  private var providerSummary: String {
    guard provider.requiresConfigurationFile else { return provider.title }
    return "\(provider.title) · \(selectedConfigurationName)"
  }

  private var baseURLString: String {
    let value = app.baseURL?.absoluteString ?? "http://127.0.0.1:1337"
    return value.hasSuffix("/") ? String(value.dropLast()) : value
  }

  private var commandModelID: String {
    ShellArgument.quote(selectedModelID.isEmpty ? "<model-id>" : selectedModelID)
  }

  private var clientInstructions: String {
    switch client {
    case .claudeCode:
      return "ANTHROPIC_BASE_URL=\(ShellArgument.quote(baseURLString)) ANTHROPIC_API_KEY=dummy claude --model \(commandModelID)"
    case .codex:
      return "OPENAI_BASE_URL=\(ShellArgument.quote(baseURLString + "/v1")) OPENAI_API_KEY=dummy codex -m \(commandModelID)"
    case .copilot:
      return "COPILOT_PROVIDER_BASE_URL=\(ShellArgument.quote(baseURLString + "/v1")) COPILOT_PROVIDER_TYPE=openai COPILOT_PROVIDER_WIRE_API=responses COPILOT_MODEL=\(commandModelID) COPILOT_OFFLINE=true copilot"
    case .openAICompatible:
      return "OPENAI_BASE_URL=\(ShellArgument.quote(baseURLString + "/v1")) OPENAI_API_KEY=dummy openai-compatible-client --model \(commandModelID)"
    }
  }

  private func modelEndpointSummary(_ model: RuntimeModel) -> String {
    model.supportedEndpoints.isEmpty
      ? "Endpoints not reported"
      : model.supportedEndpoints.joined(separator: ", ")
  }
}

private enum OnboardingClient: String, CaseIterable, Identifiable {
  case claudeCode
  case codex
  case copilot
  case openAICompatible

  var id: String { rawValue }

  var title: String {
    switch self {
    case .claudeCode: return "Claude"
    case .codex: return "Codex"
    case .copilot: return "Copilot"
    case .openAICompatible: return "OpenAI"
    }
  }

  var longTitle: String {
    switch self {
    case .claudeCode: return "Claude Code"
    case .codex: return "Codex CLI"
    case .copilot: return "GitHub Copilot CLI"
    case .openAICompatible: return "OpenAI-compatible Client"
    }
  }

  var summary: String {
    switch self {
    case .claudeCode:
      return "Point Claude Code's Anthropic-compatible endpoint at the running Vekil app."
    case .codex:
      return "Point Codex CLI's OpenAI-compatible endpoint at the running Vekil app."
    case .copilot:
      return "Use Copilot CLI's custom-provider Responses wire API against Vekil's local endpoint."
    case .openAICompatible:
      return "Point an OpenAI-compatible client at Vekil's local /v1 root and choose a public model ID."
    }
  }

  var footnote: String {
    switch self {
    case .claudeCode, .codex:
      return "The dummy client key is local placeholder data; provider credentials remain owned by Vekil."
    case .copilot:
      return "Select a direct Responses-capable model from Vekil's advertised catalog."
    case .openAICompatible:
      return "Replace openai-compatible-client with your client executable. The dummy key stays local; provider credentials remain owned by Vekil."
    }
  }

  var systemImage: String {
    switch self {
    case .claudeCode: return "terminal"
    case .codex: return "chevron.left.forwardslash.chevron.right"
    case .copilot: return "bolt.horizontal.circle"
    case .openAICompatible: return "curlybraces"
    }
  }

  var copyButtonTitle: String { "Copy Command" }
}

private enum OnboardingRowState {
  case success
  case working
  case pending
  case warning

  var systemImage: String {
    switch self {
    case .success: return "checkmark.circle.fill"
    case .working: return "clock.arrow.circlepath"
    case .pending: return "circle"
    case .warning: return "exclamationmark.triangle.fill"
    }
  }

  var color: Color {
    switch self {
    case .success: return .green
    case .working: return .accentColor
    case .pending: return .secondary
    case .warning: return .orange
    }
  }

  var accessibilityValue: String {
    switch self {
    case .success: return "Ready"
    case .working: return "In progress"
    case .pending: return "Pending"
    case .warning: return "Needs attention"
    }
  }
}

private struct OnboardingPageHeading: View {
  let title: String
  let subtitle: String

  var body: some View {
    VStack(alignment: .leading, spacing: 6) {
      Text(title)
        .font(.largeTitle.bold())
      Text(subtitle)
        .font(.title3)
        .foregroundStyle(.secondary)
        .fixedSize(horizontal: false, vertical: true)
    }
    .accessibilityElement(children: .combine)
  }
}

private struct OnboardingStatusRow: View {
  let label: String
  let value: String
  let state: OnboardingRowState

  var body: some View {
    HStack(alignment: .firstTextBaseline, spacing: 12) {
      Image(systemName: state.systemImage)
        .foregroundStyle(state.color)
        .frame(width: 18)
      Text(label)
        .foregroundStyle(.secondary)
      Spacer(minLength: 20)
      Text(value)
        .multilineTextAlignment(.trailing)
        .textSelection(.enabled)
    }
    .padding(.vertical, 10)
    .accessibilityElement(children: .ignore)
    .accessibilityLabel(label)
    .accessibilityValue("\(value), \(state.accessibilityValue)")
  }
}

private struct OnboardingNotice: View {
  let title: String
  let message: String
  let systemImage: String
  let onDismiss: () -> Void

  var body: some View {
    HStack(alignment: .top, spacing: 12) {
      Image(systemName: systemImage)
        .foregroundStyle(.orange)
      VStack(alignment: .leading, spacing: 3) {
        Text(title)
          .font(.headline)
        Text(message)
          .font(.callout)
          .fixedSize(horizontal: false, vertical: true)
      }
      Spacer(minLength: 12)
      Button("Dismiss", action: onDismiss)
    }
    .padding(14)
    .background(Color.orange.opacity(0.1), in: RoundedRectangle(cornerRadius: 10))
    .accessibilityElement(children: .contain)
  }
}
