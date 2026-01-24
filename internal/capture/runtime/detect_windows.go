//go:build windows

package runtime

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// VS_FIXEDFILEINFO contains version information for a file.
type VS_FIXEDFILEINFO struct {
	Signature        uint32
	StrucVersion     uint32
	FileVersionMS    uint32
	FileVersionLS    uint32
	ProductVersionMS uint32
	ProductVersionLS uint32
	FileFlagsMask    uint32
	FileFlags        uint32
	FileOS           uint32
	FileType         uint32
	FileSubtype      uint32
	FileDateMS       uint32
	FileDateLS       uint32
}

// Windows API bindings for version information retrieval
var (
	modVersion = windows.NewLazySystemDLL("version.dll")

	procGetFileVersionInfoSizeW = modVersion.NewProc("GetFileVersionInfoSizeW")
	procGetFileVersionInfoW     = modVersion.NewProc("GetFileVersionInfoW")
	procVerQueryValueW          = modVersion.NewProc("VerQueryValueW")
)

// Windows API bindings for EnumProcessModulesEx
var (
	modPsapi                 = windows.NewLazySystemDLL("psapi.dll")
	procEnumProcessModulesEx = modPsapi.NewProc("EnumProcessModulesEx")
)

// javaModules maps DLL names to their descriptions for Java/JVM detection.
var javaModules = map[string]string{
	"jvm.dll":    "HotSpot/Standard JVM",
	"libjvm.dll": "JVM (alternate naming)",
}

// dotNetFrameworkModules maps DLL names to descriptions for .NET Framework detection.
var dotNetFrameworkModules = map[string]string{
	"clr.dll":      ".NET Framework 4.x CLR",
	"mscorwks.dll": ".NET Framework 2.0-3.5 CLR",
	"mscoree.dll":  ".NET Framework Loader",
}

// dotNetCoreModules maps DLL names to descriptions for .NET Core/5+/6+/7+/8+ detection.
var dotNetCoreModules = map[string]string{
	"coreclr.dll":    ".NET Core/5+ Runtime",
	"hostfxr.dll":    ".NET Core/5+ Host Resolver",
	"hostpolicy.dll": ".NET Core/5+ Host Policy",
}

// DetectRuntime analyzes a process to determine if it's running on Java or .NET.
func DetectRuntime(pid int) (*RuntimeInfo, error) {
	// Open the process with required access rights
	handle, err := windows.OpenProcess(
		windows.PROCESS_QUERY_INFORMATION|windows.PROCESS_VM_READ,
		false,
		uint32(pid),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to open process %d: %w", pid, err)
	}
	defer windows.CloseHandle(handle)

	// Get list of all loaded modules (DLLs) in the process
	modules, err := enumerateModules(handle)
	if err != nil {
		return nil, fmt.Errorf("failed to enumerate modules: %w", err)
	}

	info := &RuntimeInfo{
		Runtime: RuntimeUnknown,
	}

	// Track which runtime DLL we found for version extraction
	var dotNetRuntimeDLL string
	var javaRuntimeDLL string
	var isDotNetCore bool

	// Check each module for runtime indicators
	for _, modulePath := range modules {
		moduleName := strings.ToLower(filepath.Base(modulePath))

		// Check for Java runtime DLLs
		if desc, found := javaModules[moduleName]; found {
			info.Runtime = RuntimeJava
			if info.Details == "" {
				info.Details = desc
			} else {
				info.Details += ", " + desc
			}
			// Prefer jvm.dll for version extraction
			if moduleName == "jvm.dll" || javaRuntimeDLL == "" {
				javaRuntimeDLL = modulePath
			}
		}

		// Check for .NET Framework runtime DLLs
		if desc, found := dotNetFrameworkModules[moduleName]; found {
			info.Runtime = RuntimeDotNet
			if info.Details == "" {
				info.Details = desc
			} else if !strings.Contains(info.Details, desc) {
				info.Details += ", " + desc
			}
			// Use clr.dll for version extraction (primary runtime DLL)
			if moduleName == "clr.dll" {
				dotNetRuntimeDLL = modulePath
				isDotNetCore = false
			}
		}

		// Check for .NET Core/5+ runtime DLLs
		if desc, found := dotNetCoreModules[moduleName]; found {
			info.Runtime = RuntimeDotNet
			if info.Details == "" {
				info.Details = desc
			} else if !strings.Contains(info.Details, desc) {
				info.Details += ", " + desc
			}
			// Use coreclr.dll for version extraction (primary runtime DLL)
			if moduleName == "coreclr.dll" {
				dotNetRuntimeDLL = modulePath
				isDotNetCore = true
			}
		}
	}

	// Extract version information from the runtime DLLs
	if info.Runtime == RuntimeDotNet && dotNetRuntimeDLL != "" {
		info.Version = getFileVersion(dotNetRuntimeDLL)
		if info.Version == "" {
			// Fallback to path extraction
			info.Version = extractVersionFromPath(dotNetRuntimeDLL, isDotNetCore)
		}
	}

	if info.Runtime == RuntimeJava && javaRuntimeDLL != "" {
		info.Version = getFileVersion(javaRuntimeDLL)
		if info.Version == "" {
			// Fallback to path extraction for Java
			info.Version = extractVersionFromPath(javaRuntimeDLL, false)
		}
	}

	return info, nil
}

// getFileVersion retrieves the file version from a DLL using the Windows Version API.
func getFileVersion(filePath string) string {
	// Convert Go string to null-terminated UTF-16 pointer for Windows API
	pathPtr, err := syscall.UTF16PtrFromString(filePath)
	if err != nil {
		return ""
	}

	// Step 1: Get the size of the version info block
	var handle uint32
	size, _, _ := procGetFileVersionInfoSizeW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		uintptr(unsafe.Pointer(&handle)),
	)
	if size == 0 {
		return ""
	}

	// Step 2: Allocate buffer and retrieve version info
	buffer := make([]byte, size)
	ret, _, _ := procGetFileVersionInfoW.Call(
		uintptr(unsafe.Pointer(pathPtr)),
		0,
		size,
		uintptr(unsafe.Pointer(&buffer[0])),
	)
	if ret == 0 {
		return ""
	}

	// Step 3: Query for the root VS_FIXEDFILEINFO structure
	var fixedInfo *VS_FIXEDFILEINFO
	var fixedInfoLen uint32

	rootPath, _ := syscall.UTF16PtrFromString(`\`)

	ret, _, _ = procVerQueryValueW.Call(
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(unsafe.Pointer(rootPath)),
		uintptr(unsafe.Pointer(&fixedInfo)),
		uintptr(unsafe.Pointer(&fixedInfoLen)),
	)
	if ret == 0 || fixedInfo == nil {
		return ""
	}

	// Verify the signature
	if fixedInfo.Signature != 0xFEEF04BD {
		return ""
	}

	// Extract version components from the DWORD pairs
	major := (fixedInfo.FileVersionMS >> 16) & 0xFFFF
	minor := fixedInfo.FileVersionMS & 0xFFFF
	build := (fixedInfo.FileVersionLS >> 16) & 0xFFFF
	revision := fixedInfo.FileVersionLS & 0xFFFF

	// Format version string - omit revision if it's 0
	if revision == 0 {
		return fmt.Sprintf("%d.%d.%d", major, minor, build)
	}
	return fmt.Sprintf("%d.%d.%d.%d", major, minor, build, revision)
}

// extractVersionFromPath attempts to extract the runtime version from the DLL path.
func extractVersionFromPath(path string, isDotNetCore bool) string {
	// Normalize path separators
	normalizedPath := strings.ReplaceAll(path, "\\", "/")

	if isDotNetCore {
		// .NET Core/5+ pattern: look for version directory in path
		patterns := []string{
			`Microsoft\.NETCore\.App/([\d]+\.[\d]+\.[\d]+)/`,
			`host/fxr/([\d]+\.[\d]+\.[\d]+)/`,
			`/([\d]+\.[\d]+\.[\d]+)/coreclr\.dll`,
			`/([\d]+\.[\d]+\.[\d]+)/hostfxr\.dll`,
		}

		for _, pattern := range patterns {
			re := regexp.MustCompile(pattern)
			if matches := re.FindStringSubmatch(normalizedPath); len(matches) > 1 {
				return matches[1]
			}
		}
	} else {
		// .NET Framework pattern: look for version in Framework path
		frameworkPattern := regexp.MustCompile(`Framework(?:64)?/v([\d]+\.[\d]+\.[\d]+)/`)
		if matches := frameworkPattern.FindStringSubmatch(normalizedPath); len(matches) > 1 {
			return matches[1]
		}

		// Java pattern: look for version in path
		javaPatterns := []string{
			`jdk-([\d]+\.[\d]+\.[\d]+)`,
			`jdk([\d]+\.[\d]+\.[\d]+)`,
			`jre([\d]+\.[\d]+\.[\d]+)`,
			`jdk-([\d]+)(?:\.[\d]+)?`,
			`/([\d]+\.[\d]+\.[\d]+)[/-]`,
		}

		for _, pattern := range javaPatterns {
			re := regexp.MustCompile(pattern)
			if matches := re.FindStringSubmatch(normalizedPath); len(matches) > 1 {
				return matches[1]
			}
		}
	}

	return ""
}

// enumerateModules retrieves the list of all loaded modules (DLLs) in a process.
func enumerateModules(handle windows.Handle) ([]string, error) {
	var modules [1024]windows.Handle
	var needed uint32

	// Try EnumProcessModulesEx first with LIST_MODULES_ALL (0x03)
	err := enumProcessModulesEx(handle, &modules[0], uint32(len(modules))*uint32(unsafe.Sizeof(modules[0])), &needed, 0x03)
	if err != nil {
		// Fallback to regular EnumProcessModules if extended version fails
		err = windows.EnumProcessModules(handle, &modules[0], uint32(len(modules))*uint32(unsafe.Sizeof(modules[0])), &needed)
		if err != nil {
			return nil, err
		}
	}

	// Calculate how many modules were returned
	count := needed / uint32(unsafe.Sizeof(modules[0]))
	result := make([]string, 0, count)

	// Get the full path for each module
	for i := uint32(0); i < count; i++ {
		var name [windows.MAX_PATH]uint16
		err := windows.GetModuleFileNameEx(handle, modules[i], &name[0], windows.MAX_PATH)
		if err == nil {
			result = append(result, windows.UTF16ToString(name[:]))
		}
	}

	return result, nil
}

// enumProcessModulesEx is a wrapper for the Windows EnumProcessModulesEx function.
func enumProcessModulesEx(process windows.Handle, module *windows.Handle, cb uint32, needed *uint32, filterFlag uint32) error {
	ret, _, err := procEnumProcessModulesEx.Call(
		uintptr(process),
		uintptr(unsafe.Pointer(module)),
		uintptr(cb),
		uintptr(unsafe.Pointer(needed)),
		uintptr(filterFlag),
	)
	if ret == 0 {
		return err
	}
	return nil
}
