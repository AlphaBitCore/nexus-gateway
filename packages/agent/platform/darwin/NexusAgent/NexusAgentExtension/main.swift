import NetworkExtension

// Entry point for the NexusAgent System Extension.
// The NE framework instantiates NexusProxyProvider via the EXPrincipalClass
// key in Info.plist; we just tell the framework to take over the process.
autoreleasepool {
    NEProvider.startSystemExtensionMode()
}
dispatchMain()
