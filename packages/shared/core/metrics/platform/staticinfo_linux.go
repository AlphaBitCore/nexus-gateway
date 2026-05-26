//go:build linux

package platform

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// osVersion reads /etc/os-release and returns PRETTY_NAME, falling back to
// NAME, falling back to empty.
func osVersion() string {
	f, err := os.Open("/etc/os-release")
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	var name, pretty string
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case strings.HasPrefix(line, "PRETTY_NAME="):
			pretty = unquote(strings.TrimPrefix(line, "PRETTY_NAME="))
		case strings.HasPrefix(line, "NAME="):
			name = unquote(strings.TrimPrefix(line, "NAME="))
		}
	}
	if pretty != "" {
		return pretty
	}
	return name
}

// kernelVersion reads /proc/version and returns the third token, which is the
// kernel release string per the standard "Linux version <release> (...)" format.
func kernelVersion() string {
	data, err := os.ReadFile("/proc/version")
	if err != nil {
		return ""
	}
	fields := strings.Fields(string(data))
	if len(fields) < 3 {
		return ""
	}
	return fields[2]
}

// totalRAMBytes reads /proc/meminfo MemTotal (in KB) and converts to bytes.
func totalRAMBytes() uint64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}
