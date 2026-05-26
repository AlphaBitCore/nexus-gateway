@echo off
REM nexus-wfp build.bat — drive msbuild for both x64 and ARM64.
REM
REM Authoritative design: docs/developers/architecture/agent-windows-wfp-driver.md
REM SDD: docs/developers/specs/e59-s4-cross-arch-build.md
REM
REM Prerequisites:
REM   - Visual Studio 2022 Build Tools (or full IDE)
REM   - Windows Driver Kit (WDK) 11 24H2 or later
REM   - Both x64 and ARM64 build tools components selected at WDK install time
REM
REM Usage:
REM   build.bat                  -- builds both platforms
REM   build.bat x64              -- amd64 only
REM   build.bat ARM64            -- arm64 only
REM
REM Outputs:
REM   bin\x64\Release\nexus-wfp.sys
REM   bin\ARM64\Release\nexus-wfp.sys
REM Both are unsigned (signing happens in sign-driver.ps1, E59-S5).

setlocal enabledelayedexpansion

set MSBUILD="C:\Program Files\Microsoft Visual Studio\2022\BuildTools\MSBuild\Current\Bin\MSBuild.exe"
if not exist %MSBUILD% set MSBUILD="C:\Program Files\Microsoft Visual Studio\2022\Community\MSBuild\Current\Bin\MSBuild.exe"
if not exist %MSBUILD% set MSBUILD="C:\Program Files\Microsoft Visual Studio\2022\Professional\MSBuild\Current\Bin\MSBuild.exe"
if not exist %MSBUILD% set MSBUILD="C:\Program Files\Microsoft Visual Studio\2022\Enterprise\MSBuild\Current\Bin\MSBuild.exe"
if not exist %MSBUILD% (
    echo MSBuild for Visual Studio 2022 not found in standard paths.
    echo Install Visual Studio 2022 Build Tools and the WDK, then re-run.
    exit /b 1
)

set ARCHS=
if "%~1"=="" (
    set ARCHS=x64 ARM64
) else (
    set ARCHS=%~1
)

for %%A in (%ARCHS%) do (
    echo === Building nexus-wfp for %%A ===
    %MSBUILD% nexus-wfp.sln /p:Configuration=Release /p:Platform=%%A /m
    if errorlevel 1 (
        echo Build failed for %%A.
        exit /b 1
    )
)

echo.
echo === Build outputs ===
if exist bin\x64\Release\nexus-wfp.sys (
    for %%F in (bin\x64\Release\nexus-wfp.sys) do echo   %%F  %%~zF bytes
)
if exist bin\ARM64\Release\nexus-wfp.sys (
    for %%F in (bin\ARM64\Release\nexus-wfp.sys) do echo   %%F  %%~zF bytes
)
echo.
echo Next: run sign-driver.ps1 (E59-S5) to sign and submit to Microsoft.
exit /b 0
