// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "VekilApp",
    defaultLocalization: "en",
    platforms: [.macOS(.v13)],
    products: [
        .library(name: "VekilCore", targets: ["VekilCore"]),
        .library(name: "VekilUI", targets: ["VekilUI"]),
        .library(name: "VekilUpdater", targets: ["VekilUpdater"]),
        .executable(name: "Vekil", targets: ["Vekil"]),
    ],
    dependencies: [
        .package(url: "https://github.com/sparkle-project/Sparkle", exact: "2.9.4"),
    ],
    targets: [
        .target(name: "VekilCore"),
        .target(name: "VekilUI", dependencies: ["VekilCore"]),
        .target(name: "VekilUpdater", dependencies: [
            "VekilCore",
            .product(name: "Sparkle", package: "Sparkle"),
        ]),
        .executableTarget(name: "Vekil", dependencies: ["VekilCore", "VekilUI", "VekilUpdater"]),
        .testTarget(name: "VekilCoreTests", dependencies: ["VekilCore"], resources: [.process("Fixtures")]),
        .testTarget(name: "VekilUITests", dependencies: ["VekilCore", "VekilUI"]),
    ]
)
