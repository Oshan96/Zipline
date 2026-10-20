; Zipline's Windows installer (NSIS 3). tools/package.sh builds it as pkg/Zipline-Windows-Setup.exe,
; next to the portable zips, from the two Windows builds it has just made:
;
;   makensis -DVERSION=1.0.3 -DVIVERSION=1.0.3.0 -DROOT=<repository> -DOUT=<setup.exe> tools/installer.nsi
;
; It installs for the current user only, so there is no administrator prompt. The default folder is
; %LOCALAPPDATA%\Programs\Zipline, and the build matches the computer (x64 or Arm64). It adds a Start
; menu shortcut, an optional desktop shortcut and an entry in Settings > Apps that uninstalls it.
; Installing again updates in place: a running Zipline is asked to quit first. Uninstalling keeps
; the settings and download list unless you choose to remove them; downloaded files are never touched.

Unicode true
!ifdef TARGET
  Target ${TARGET} ; (amd64-unicode for trying it under 64-bit-only Wine; releases use the default x86)
!endif
SetCompressor /SOLID lzma
ManifestDPIAware true
ManifestSupportedOS Win10
RequestExecutionLevel user

!include MUI2.nsh
!include LogicLib.nsh
!include x64.nsh
!include WinVer.nsh
!include FileFunc.nsh

!define UNKEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\Zipline"
!define WIN "${ROOT}/pkg/Zipline-Windows-x64"     ; (the portable package: the files besides the exe)
!define ARM "${ROOT}/pkg/Zipline-Windows-arm64"

Name "Zipline"
OutFile "${OUT}"
InstallDir "$LOCALAPPDATA\Programs\Zipline"
InstallDirRegKey HKCU "${UNKEY}" "InstallLocation"
BrandingText "Zipline ${VERSION}"

VIProductVersion "${VIVERSION}"
VIFileVersion "${VIVERSION}"
VIAddVersionKey "ProductName" "Zipline"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "Zipline Setup"
VIAddVersionKey "CompanyName" "Zipline"
VIAddVersionKey "LegalCopyright" "Zipline"

!define MUI_ICON "${ROOT}/icon.ico"
!define MUI_UNICON "${ROOT}/icon.ico"
!define MUI_WELCOMEFINISHPAGE_BITMAP "${ROOT}/pkg/installer-side.bmp"
!define MUI_ABORTWARNING
!define MUI_COMPONENTSPAGE_NODESC
!define MUI_FINISHPAGE_RUN "$INSTDIR\Zipline.exe"
!define MUI_FINISHPAGE_RUN_TEXT "Start Zipline"

!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH
!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES
!insertmacro MUI_LANGUAGE "English"

Function .onInit
  ${IfNot} ${IsNativeAMD64}
  ${AndIfNot} ${IsNativeARM64}
    MessageBox MB_ICONSTOP "Zipline needs 64-bit Windows 10 or 11." /SD IDOK
    Abort
  ${EndIf}
  ${IfNot} ${AtLeastWin10}
    MessageBox MB_ICONSTOP "Zipline needs Windows 10 or 11." /SD IDOK
    Abort
  ${EndIf}
FunctionEnd

; QuitZipline asks a running Zipline to quit (zipline --quit), then waits until its exe can be
; replaced. If it's still running after 10 seconds, it asks you to close it.
!macro QUIT_ZIPLINE un
Function ${un}QuitZipline
  IfFileExists "$INSTDIR\Zipline.exe" 0 done
  ExecWait '"$INSTDIR\Zipline.exe" --quit'
  StrCpy $1 0
  check:
    ClearErrors
    FileOpen $0 "$INSTDIR\Zipline.exe" a
    IfErrors 0 free
    IntOp $1 $1 + 1
    IntCmp $1 50 0 wait 0
    MessageBox MB_RETRYCANCEL|MB_ICONEXCLAMATION "Zipline is still running.$\n$\nClose it (Tasks > Exit Zipline), then click Retry." /SD IDCANCEL IDRETRY again
    Abort
  again:
    StrCpy $1 0
  wait:
    Sleep 200
    Goto check
  free:
    FileClose $0
  done:
FunctionEnd
!macroend
!insertmacro QUIT_ZIPLINE ""
!insertmacro QUIT_ZIPLINE "un."

Section "Zipline" SecApp
  SectionIn RO
  SetOutPath "$INSTDIR"
  Call QuitZipline
  ${If} ${IsNativeARM64}
    File "/oname=Zipline.exe" "${ARM}/Zipline.exe"
  ${Else}
    File "/oname=Zipline.exe" "${WIN}/Zipline.exe"
  ${EndIf}
  File "${WIN}/README.md"
  File "${WIN}/zipline-firefox.xpi"
  RMDir /r "$INSTDIR\extension" ; (what an older version had there goes)
  RMDir /r "$INSTDIR\extension-firefox"
  File /r "${WIN}/extension"
  File /r "${WIN}/extension-firefox"
  WriteUninstaller "$INSTDIR\Uninstall.exe"
  CreateShortcut "$SMPROGRAMS\Zipline.lnk" "$INSTDIR\Zipline.exe"

  ; Settings > Apps
  WriteRegStr HKCU "${UNKEY}" "DisplayName" "Zipline"
  WriteRegStr HKCU "${UNKEY}" "DisplayVersion" "${VERSION}"
  WriteRegStr HKCU "${UNKEY}" "Publisher" "Zipline"
  WriteRegStr HKCU "${UNKEY}" "DisplayIcon" "$INSTDIR\Zipline.exe"
  WriteRegStr HKCU "${UNKEY}" "InstallLocation" "$INSTDIR"
  WriteRegStr HKCU "${UNKEY}" "URLInfoAbout" "https://github.com/Oshan96/Zipline"
  WriteRegStr HKCU "${UNKEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
  WriteRegStr HKCU "${UNKEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
  WriteRegDWORD HKCU "${UNKEY}" "NoModify" 1
  WriteRegDWORD HKCU "${UNKEY}" "NoRepair" 1
  ${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
  WriteRegDWORD HKCU "${UNKEY}" "EstimatedSize" $0
SectionEnd

Section "Desktop shortcut" SecDesktop
  CreateShortcut "$DESKTOP\Zipline.lnk" "$INSTDIR\Zipline.exe"
SectionEnd

Section "Uninstall"
  Call un.QuitZipline
  ExecWait '"$INSTDIR\Zipline.exe" --unregister' ; (magnet links, .torrent files, the browser extension's link)
  Delete "$INSTDIR\Zipline.exe"
  Delete "$INSTDIR\README.md"
  Delete "$INSTDIR\zipline-firefox.xpi"
  RMDir /r "$INSTDIR\extension"
  RMDir /r "$INSTDIR\extension-firefox"
  Delete "$INSTDIR\Uninstall.exe"
  RMDir "$INSTDIR" ; (only if empty: nothing else in a folder you chose is removed)
  Delete "$SMPROGRAMS\Zipline.lnk"
  Delete "$DESKTOP\Zipline.lnk"
  DeleteRegKey HKCU "${UNKEY}"
  MessageBox MB_YESNO|MB_ICONQUESTION|MB_DEFBUTTON2 "Also remove your Zipline settings and download list?$\n$\nFiles you downloaded are kept either way." /SD IDNO IDNO keep
    RMDir /r "$APPDATA\Zipline"
  keep:
SectionEnd
