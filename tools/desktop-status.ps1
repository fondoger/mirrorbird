param([switch]$Close)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes
$processes = @(Get-Process -Name ChatGPT -ErrorAction SilentlyContinue)
$windows = @($processes | Where-Object { $_.MainWindowHandle -ne 0 })
foreach ($process in $windows) {
    $root = [System.Windows.Automation.AutomationElement]::FromHandle($process.MainWindowHandle)
    $buttons = $root.FindAll([System.Windows.Automation.TreeScope]::Descendants, [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::ControlTypeProperty, [System.Windows.Automation.ControlType]::Button))
    $names = @()
    foreach ($button in $buttons) {
        $name = $button.Current.Name
        if ($name -match 'Stop|Cancel|Send|New|Settings') { $names += $name }
    }
    [pscustomobject]@{ ProcessId = $process.Id; WindowVisible = $true; RelevantControls = $names } | ConvertTo-Json -Compress
}
if ($Close) {
    foreach ($process in $windows) { [void]$process.CloseMainWindow() }
    Start-Sleep -Seconds 2
    $remaining = @(Get-Process -Name ChatGPT -ErrorAction SilentlyContinue)
    if ($remaining.Count -gt 0) { Write-Output 'Desktop is still running; no processes were force-killed.' }
}
