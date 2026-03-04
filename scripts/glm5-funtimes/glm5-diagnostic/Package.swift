// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "GLM5Diagnostic",
    platforms: [.macOS(.v13)],
    dependencies: [],
    targets: [
        .executableTarget(
            name: "GLM5Diagnostic",
            dependencies: []
        ),
    ]
)
