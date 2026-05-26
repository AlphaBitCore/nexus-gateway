//go:build windows

package paths

import "os"

// Windows paths follow the Windows Application Data conventions:
//
//	%ProgramData%\NexusAgent\           system-wide state + config
//	%ProgramData%\NexusAgent\Logs\      log files
//	\\.\pipe\com.nexus-gateway.agent    named pipe IPC
//
// The Windows service is registered with SCM (sc.exe create / New-Service);
// there is no on-disk unit file analogous to systemd, so DaemonUnitPath
// points at the install dir of the service executable as a stand-in.
const winAppDir = "NexusAgent"

func defaultPaths() Paths {
	programData := os.Getenv("ProgramData")
	if programData == "" {
		programData = `C:\ProgramData`
	}
	stateDir := programData + `\` + winAppDir
	flagsDir := stateDir + `\Flags`
	return Paths{
		StateDir:         stateDir,
		ConfigDir:        stateDir,
		ConfigFile:       stateDir + `\agent.yaml`,
		LogDir:           stateDir + `\Logs`,
		SocketPath:       `\\.\pipe\nexus-agent-status`,
		FlagsDir:         flagsDir,
		UserQuitFlagPath: flagsDir + `\user-quit`,
		DaemonUnitPath:   `C:\Program Files\NexusAgent\nexus-agent.exe`,
	}
}
