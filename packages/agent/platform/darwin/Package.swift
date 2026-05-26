// swift-tools-version: 5.9
import PackageDescription

let package = Package(
    name: "NexusAgentUI",
    defaultLocalization: "en",
    platforms: [.macOS(.v13)],
    targets: [
        // System Extension: NETransparentProxyProvider loaded by the NE framework.
        // IPCProtocol.swift lives here (used only by the extension).
        .executableTarget(
            name: "NexusAgentExtension",
            path: "NexusAgent/NexusAgentExtension",
            exclude: ["Info.plist", "NexusAgentExtension.entitlements"]
        ),

        // Menu bar host app: installs the extension and shows agent status.
        .executableTarget(
            name: "NexusAgentUI",
            path: "NexusAgentUI/Sources",
            resources: [
                .process("Resources"),
            ]
        ),
    ]
)
