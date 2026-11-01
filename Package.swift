// swift-tools-version:5.9
import PackageDescription

let package = Package(
    name: "FoundryLink",
    platforms: [.macOS(.v14)],
    targets: [
        .executableTarget(name: "FoundryLink", path: "Sources/FoundryLink")
    ]
)
