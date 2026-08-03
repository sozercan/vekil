import Foundation
import Security

public struct ProviderSecretRole: RawRepresentable, Codable, Hashable, Sendable {
  public let rawValue: String

  public init(rawValue: String) {
    self.rawValue = rawValue
  }

  public static let apiKey = Self(rawValue: "api_key")
  public static let bearerToken = Self(rawValue: "bearer_token")
}

public struct ProviderSecretIdentity: Codable, Hashable, Sendable {
  public var providerID: UUID
  public var role: ProviderSecretRole

  public init(providerID: UUID, role: ProviderSecretRole) {
    self.providerID = providerID
    self.role = role
  }
}

public struct ProviderSecretReference: Codable, Hashable, Sendable {
  public var providerID: UUID
  public var role: ProviderSecretRole
  public var generation: UInt64

  public init(providerID: UUID, role: ProviderSecretRole, generation: UInt64) {
    self.providerID = providerID
    self.role = role
    self.generation = generation
  }

  public init(identity: ProviderSecretIdentity, generation: UInt64) {
    self.init(providerID: identity.providerID, role: identity.role, generation: generation)
  }

  public var identity: ProviderSecretIdentity {
    ProviderSecretIdentity(providerID: providerID, role: role)
  }

  public var accountName: String {
    KeychainSecretAccount.name(for: self)
  }
}

/// Stable, versioned Keychain account names. Provider display IDs and array
/// positions are intentionally absent: identity derives only from the durable
/// provider UUID, semantic secret role, and monotonic generation.
public enum KeychainSecretAccount {
  public static let formatPrefix = "vekil-provider-v1"

  public static func name(for reference: ProviderSecretReference) -> String {
    let provider = reference.providerID.uuidString.lowercased()
    let role = base64URLEncode(Data(reference.role.rawValue.utf8))
    return "\(formatPrefix):\(provider):\(reference.generation):\(role)"
  }

  public static func reference(from accountName: String) -> ProviderSecretReference? {
    let components = accountName.split(separator: ":", omittingEmptySubsequences: false)
    guard
      components.count == 4,
      components[0] == Substring(formatPrefix),
      let providerID = UUID(uuidString: String(components[1])),
      let generation = UInt64(components[2]),
      let roleData = base64URLDecode(String(components[3])),
      let role = String(data: roleData, encoding: .utf8)
    else {
      return nil
    }

    return ProviderSecretReference(
      providerID: providerID,
      role: ProviderSecretRole(rawValue: role),
      generation: generation
    )
  }

  private static func base64URLEncode(_ data: Data) -> String {
    data.base64EncodedString()
      .replacingOccurrences(of: "+", with: "-")
      .replacingOccurrences(of: "/", with: "_")
      .replacingOccurrences(of: "=", with: "")
  }

  private static func base64URLDecode(_ value: String) -> Data? {
    var base64 =
      value
      .replacingOccurrences(of: "-", with: "+")
      .replacingOccurrences(of: "_", with: "/")
    let remainder = base64.utf8.count % 4
    if remainder != 0 {
      base64.append(String(repeating: "=", count: 4 - remainder))
    }
    return Data(base64Encoded: base64)
  }
}

public protocol KeychainSecretStore: Sendable {
  func secret(for reference: ProviderSecretReference) async throws -> Data?
  func setSecret(_ secret: Data, for reference: ProviderSecretReference) async throws
  func removeSecret(for reference: ProviderSecretReference) async throws
  func allReferences() async throws -> Set<ProviderSecretReference>
}

public enum KeychainSecretStoreOperation: String, Equatable, Sendable {
  case read
  case write
  case update
  case delete
  case list
}

public enum KeychainSecretStoreError: Error, Equatable, LocalizedError, Sendable {
  case unexpectedStatus(operation: KeychainSecretStoreOperation, status: OSStatus)
  case unexpectedResult

  public var errorDescription: String? {
    switch self {
    case .unexpectedStatus(let operation, let status):
      return "Keychain \(operation.rawValue) failed (status \(status))."
    case .unexpectedResult:
      return "Keychain returned an unexpected result."
    }
  }
}

/// Generic-password Keychain storage for managed provider secrets.
///
/// The service identifier is deliberately stable across app versions and must
/// remain aligned with the bundle's production signing continuity tests.
public actor SecurityKeychainSecretStore: KeychainSecretStore {
  public static let serviceIdentifier = "com.vekil.menubar.providers"

  public nonisolated let service: String

  public init(service: String = SecurityKeychainSecretStore.serviceIdentifier) {
    self.service = service
  }

  public func secret(for reference: ProviderSecretReference) async throws -> Data? {
    var query = matchQuery(for: reference)
    query[kSecReturnData] = kCFBooleanTrue
    query[kSecMatchLimit] = kSecMatchLimitOne

    var result: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &result)
    switch status {
    case errSecSuccess:
      guard let data = result as? Data else {
        throw KeychainSecretStoreError.unexpectedResult
      }
      return data
    case errSecItemNotFound:
      return nil
    default:
      throw KeychainSecretStoreError.unexpectedStatus(operation: .read, status: status)
    }
  }

  public func setSecret(_ secret: Data, for reference: ProviderSecretReference) async throws {
    var add = matchQuery(for: reference)
    add[kSecValueData] = secret
    add[kSecAttrAccessible] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
    add[kSecAttrLabel] = "Vekil provider secret"

    let status = SecItemAdd(add as CFDictionary, nil)
    switch status {
    case errSecSuccess:
      return
    case errSecDuplicateItem:
      let update: [CFString: Any] = [kSecValueData: secret]
      let updateStatus = SecItemUpdate(
        matchQuery(for: reference) as CFDictionary, update as CFDictionary)
      guard updateStatus == errSecSuccess else {
        throw KeychainSecretStoreError.unexpectedStatus(operation: .update, status: updateStatus)
      }
    default:
      throw KeychainSecretStoreError.unexpectedStatus(operation: .write, status: status)
    }
  }

  public func removeSecret(for reference: ProviderSecretReference) async throws {
    let status = SecItemDelete(matchQuery(for: reference) as CFDictionary)
    guard status == errSecSuccess || status == errSecItemNotFound else {
      throw KeychainSecretStoreError.unexpectedStatus(operation: .delete, status: status)
    }
  }

  public func allReferences() async throws -> Set<ProviderSecretReference> {
    let query: [CFString: Any] = [
      kSecClass: kSecClassGenericPassword,
      kSecAttrService: service,
      kSecReturnAttributes: kCFBooleanTrue as Any,
      kSecMatchLimit: kSecMatchLimitAll,
    ]

    var result: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &result)
    switch status {
    case errSecItemNotFound:
      return []
    case errSecSuccess:
      break
    default:
      throw KeychainSecretStoreError.unexpectedStatus(operation: .list, status: status)
    }

    let dictionaries: [[String: Any]]
    if let many = result as? [[String: Any]] {
      dictionaries = many
    } else if let one = result as? [String: Any] {
      dictionaries = [one]
    } else {
      throw KeychainSecretStoreError.unexpectedResult
    }

    var references = Set<ProviderSecretReference>()
    for attributes in dictionaries {
      guard
        let account = attributes[kSecAttrAccount as String] as? String,
        let reference = KeychainSecretAccount.reference(from: account)
      else {
        continue
      }
      references.insert(reference)
    }
    return references
  }

  private func matchQuery(for reference: ProviderSecretReference) -> [CFString: Any] {
    [
      kSecClass: kSecClassGenericPassword,
      kSecAttrService: service,
      kSecAttrAccount: reference.accountName,
    ]
  }
}

public actor InMemoryKeychainSecretStore: KeychainSecretStore {
  private var secrets: [ProviderSecretReference: Data]

  public init(secrets: [ProviderSecretReference: Data] = [:]) {
    self.secrets = secrets
  }

  public func secret(for reference: ProviderSecretReference) async throws -> Data? {
    secrets[reference]
  }

  public func setSecret(_ secret: Data, for reference: ProviderSecretReference) async throws {
    secrets[reference] = secret
  }

  public func removeSecret(for reference: ProviderSecretReference) async throws {
    secrets.removeValue(forKey: reference)
  }

  public func allReferences() async throws -> Set<ProviderSecretReference> {
    Set(secrets.keys)
  }

  public func snapshot() -> [ProviderSecretReference: Data] {
    secrets
  }
}
