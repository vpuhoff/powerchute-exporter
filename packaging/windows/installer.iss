#ifndef AppVersion
  #define AppVersion "0.0.0-dev"
#endif
#ifndef NumericVersion
  #define NumericVersion "0.0.0.0"
#endif
#define AppName "PowerChute Prometheus Exporter"

[Setup]
AppId={{36AA1700-83C9-44EF-9D19-076DE16CC96F}
AppName={#AppName}
AppVersion={#AppVersion}
VersionInfoVersion={#NumericVersion}
DefaultDirName={autopf}\PowerChute Exporter
DisableProgramGroupPage=yes
PrivilegesRequired=admin
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
MinVersion=10.0
OutputDir=..\..\dist
OutputBaseFilename=powerchute-exporter_{#AppVersion}_windows_amd64_setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
CloseApplications=no
UninstallDisplayIcon={app}\powerchute-exporter.exe
SetupLogging=yes

[Dirs]
Name: "{commonappdata}\PowerChute Exporter"; Flags: uninsneveruninstall

[Files]
Source: "..\..\dist\windows-amd64\powerchute-exporter.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\..\configs\config.example.yaml"; DestDir: "{commonappdata}\PowerChute Exporter"; DestName: "config.yaml"; Flags: onlyifdoesntexist uninsneveruninstall
Source: "..\..\README.md"; DestDir: "{app}"; Flags: ignoreversion

[Icons]
Name: "{group}\Configuration"; Filename: "{commonappdata}\PowerChute Exporter\config.yaml"

[Code]
var
  RestartService: Boolean;

function ExporterPath: String;
begin
  Result := ExpandConstant('{app}\powerchute-exporter.exe');
end;

function ServiceCommand(const Action: String; var ExitCode: Integer): Boolean;
begin
  Result := Exec(ExporterPath, '-service ' + Action + ' -config "' +
    ExpandConstant('{commonappdata}\PowerChute Exporter\config.yaml') + '"',
    '', SW_HIDE, ewWaitUntilTerminated, ExitCode);
end;

function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  Code: Integer;
begin
  Result := '';
  RestartService := False;
  if FileExists(ExporterPath) then begin
    if not ServiceCommand('status', Code) then begin
      Result := 'Cannot query the existing exporter service.';
      Exit;
    end;
    if (Code <> 0) and (Code <> 3) then begin
      Result := 'Cannot determine the existing exporter service state.';
      Exit;
    end;
    RestartService := Code = 0;
    if not ServiceCommand('stop', Code) or (Code <> 0) then
      Result := 'Cannot stop the existing exporter service. Stop it and retry.';
  end;
end;

procedure CurStepChanged(CurStep: TSetupStep);
var
  Code: Integer;
  Params: String;
begin
  if CurStep = ssPostInstall then begin
    { Limit this application's data directory to SYSTEM, administrators and LocalService. }
    Params := '"' + ExpandConstant('{commonappdata}\PowerChute Exporter') +
      '" /inheritance:r /grant:r "*S-1-5-18:(OI)(CI)F"' +
      ' "*S-1-5-32-544:(OI)(CI)F" "*S-1-5-19:(OI)(CI)RX"';
    if not Exec(ExpandConstant('{sys}\icacls.exe'), Params, '', SW_HIDE,
      ewWaitUntilTerminated, Code) or (Code <> 0) then
      RaiseException('Cannot set exporter configuration permissions.');
    if not ServiceCommand('install', Code) or (Code <> 0) then
      RaiseException('Cannot register the exporter service.');
    if RestartService then begin
      if not ServiceCommand('start', Code) or (Code <> 0) then
        RaiseException('Cannot restart the exporter service. Check the Application event log.');
    end else if not WizardSilent then
      MsgBox('The Windows service is installed with automatic startup.' + #13#10 +
        'Configure ' + ExpandConstant('{commonappdata}\PowerChute Exporter\config.yaml') +
        ' and start PowerChuteExporter in Services.', mbInformation, MB_OK);
  end;
end;

function InitializeUninstall: Boolean;
var
  Code: Integer;
begin
  Result := ServiceCommand('uninstall', Code) and (Code = 0);
  if not Result and not UninstallSilent then
    MsgBox('Cannot stop or remove the exporter service. Uninstallation was canceled.', mbError, MB_OK);
end;
