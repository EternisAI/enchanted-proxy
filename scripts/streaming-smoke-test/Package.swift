// swift-tools-version: 5.10
import PackageDescription

let package = Package(
    name: "StreamingSmokeTest",
    platforms: [.macOS(.v13)],
    dependencies: [
        .package(url: "https://github.com/MacPaw/OpenAI", from: "0.4.7"),
    ],
    targets: [
        .executableTarget(
            name: "StreamingSmokeTest",
            dependencies: [
                .product(name: "OpenAI", package: "OpenAI"),
            ]
        ),
    ]
)
