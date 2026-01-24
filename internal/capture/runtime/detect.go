package runtime

// RuntimeType represents the detected runtime environment
type RuntimeType string

const (
	RuntimeUnknown RuntimeType = "unknown"
	RuntimeJava    RuntimeType = "java"
	RuntimeDotNet  RuntimeType = "dotnet"
)

// RuntimeInfo holds detection results for a process
type RuntimeInfo struct {
	Runtime       RuntimeType
	Version       string   // e.g., "8.0.1" for .NET or "17.0.2" for Java
	Details       string   // e.g., ".NET Core/5+ Runtime" or "HotSpot/Standard JVM"
	LoadedModules []string // Optional: for debugging
}
