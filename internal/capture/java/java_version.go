package java

import (
	"bufio"
	"bytes"
	"errors"
	"strconv"
	"strings"

	"shell/internal/utils"
)

// JavaVersion represents the New Version supporting the
// JEP 223: New Version-String Scheme specification
// https://openjdk.org/jeps/223
type JavaVersion struct {
	Major    uint
	Minor    uint
	Security uint
}

func ParseJavaVersionString(version string) JavaVersion {
	// Replace _ with . so that
	// 1.8.0_342 becomes 1.8.0.342
	// This is to treat 1.8.0_342 as 8.0.342
	version = strings.ReplaceAll(version, "_", ".")

	versionNumbers := strings.Split(version, ".")
	javaVersion := JavaVersion{}

	// Skip first element if its value is 1
	// This is to treat 1.8.0_342 as 8.0.342
	if len(versionNumbers) > 0 {
		if versionNumbers[0] == "1" {
			versionNumbers = versionNumbers[1:]
		}
	}

	if len(versionNumbers) > 0 {
		parsedUint, _ := strconv.ParseUint(versionNumbers[0], 10, 0)
		javaVersion.Major = uint(parsedUint)
	}

	if len(versionNumbers) > 1 {
		parsedUint, _ := strconv.ParseUint(versionNumbers[1], 10, 0)
		javaVersion.Minor = uint(parsedUint)
	}

	if len(versionNumbers) > 2 {
		parsedUint, _ := strconv.ParseUint(versionNumbers[2], 10, 0)
		javaVersion.Security = uint(parsedUint)
	}

	return javaVersion
}

func GetLocalJavaVersion() (JavaVersion, error) {
	combinedOutput, err := utils.CommandCombinedOutput(utils.JavaVersionCommand)
	if err != nil {
		return JavaVersion{}, errors.New("error while getting java version: " + err.Error())
	}

	var textLine string

	scanner := bufio.NewScanner(strings.NewReader(string(combinedOutput)))
	for scanner.Scan() {
		if bytes.Contains(scanner.Bytes(), []byte("java.version =")) {
			// java.version = x.y.z
			textLine = scanner.Text()
		}
	}

	// The index of '=' in "java.version = x.y.z"
	symbolIndex := strings.IndexRune(textLine, '=')

	// +2 here is to account for the = character itself and the space afterward.
	if symbolIndex < 0 || symbolIndex+2 > len(textLine) {
		return JavaVersion{}, errors.New("java version not found")
	}

	textLine = textLine[symbolIndex+2:]
	javaVersion := ParseJavaVersionString(textLine)

	return javaVersion, nil
}
