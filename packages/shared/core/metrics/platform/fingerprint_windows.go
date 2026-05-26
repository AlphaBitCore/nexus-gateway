//go:build windows

package platform

import (
	"os/exec"
	"strings"
)

// hardwareUUID reads HKLM\SOFTWARE\Microsoft\Cryptography\MachineGuid
// via reg.exe. Stable across reboots; regenerated only by sysprep
// /generalize (image deployment tools). Empty on shell-out failure.
func hardwareUUID() string {
	out, err := exec.Command("reg", "query",
		`HKLM\SOFTWARE\Microsoft\Cryptography`,
		"/v", "MachineGuid",
	).Output()
	if err != nil {
		return ""
	}
	// reg.exe output ends in "MachineGuid    REG_SZ    <guid>" — take last token.
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "MachineGuid") {
			fields := strings.Fields(line)
			if len(fields) >= 3 {
				return fields[len(fields)-1]
			}
		}
	}
	return ""
}

// hardwareSerial returns the BIOS serial from `wmic bios get serialnumber`.
// Older Windows; on Win11 wmic is deprecated but typically still present.
// Empty on any failure.
func hardwareSerial() string {
	out, err := exec.Command("wmic", "bios", "get", "serialnumber").Output()
	if err != nil {
		return ""
	}
	// wmic output is two lines: header + value.
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line != "" && line != "SerialNumber" {
			return line
		}
	}
	return ""
}

// hardwareModel returns the friendly model from `wmic computersystem get model`.
// Empty on shell-out failure.
func hardwareModel() string {
	out, err := exec.Command("wmic", "computersystem", "get", "model").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line != "" && line != "Model" {
			return line
		}
	}
	return ""
}

// cpuBrandString returns the friendly CPU name from `wmic cpu get name`.
func cpuBrandString() string {
	out, err := exec.Command("wmic", "cpu", "get", "name").Output()
	if err != nil {
		return ""
	}
	lines := strings.Split(string(out), "\n")
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line != "" && line != "Name" {
			return line
		}
	}
	return ""
}
