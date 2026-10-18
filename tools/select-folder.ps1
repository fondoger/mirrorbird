param([Parameter(Mandatory=$true)][string]$Folder)
$ErrorActionPreference = 'Stop'
if (-not [System.IO.Directory]::Exists($Folder)) { throw 'Folder must already exist.' }
Add-Type -AssemblyName Microsoft.VisualBasic
Add-Type -AssemblyName System.Windows.Forms
$desktop = Get-Process -Name ChatGPT | Where-Object { $_.MainWindowHandle -ne 0 } | Select-Object -First 1
if ($null -eq $desktop) { throw 'Desktop is not running.' }
[Microsoft.VisualBasic.Interaction]::AppActivate($desktop.Id)
[System.Windows.Forms.SendKeys]::SendWait('%d')
Start-Sleep -Milliseconds 200
[System.Windows.Forms.SendKeys]::SendWait($Folder)
[System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
Start-Sleep -Milliseconds 300
[System.Windows.Forms.SendKeys]::SendWait('{ENTER}')
