import SwiftUI

@main
struct FoundryLinkApp: App {
    @StateObject private var store = Store()

    var body: some Scene {
        WindowGroup("Foundry Link") {
            RootView().environmentObject(store)
        }
    }
}
