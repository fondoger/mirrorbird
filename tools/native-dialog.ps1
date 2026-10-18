param([string]$Folder)
$ErrorActionPreference = 'Stop'
Add-Type -AssemblyName UIAutomationClient
Add-Type -AssemblyName UIAutomationTypes
$root = [System.Windows.Automation.AutomationElement]::RootElement
$windows = $root.FindAll([System.Windows.Automation.TreeScope]::Children, [System.Windows.Automation.Condition]::TrueCondition)
foreach ($window in $windows) {
    $pidValue = $window.Current.ProcessId
    $proc = Get-Process -Id $pidValue -ErrorAction SilentlyContinue
    if ($null -eq $proc -or $proc.ProcessName -ne 'ChatGPT') { continue }
    $edits = $window.FindAll([System.Windows.Automation.TreeScope]::Descendants, [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::ControlTypeProperty, [System.Windows.Automation.ControlType]::Edit))
    $buttons = $window.FindAll([System.Windows.Automation.TreeScope]::Descendants, [System.Windows.Automation.PropertyCondition]::new([System.Windows.Automation.AutomationElement]::ControlTypeProperty, [System.Windows.Automation.ControlType]::Button))
    $editInfo = @()
    foreach ($edit in $edits) {
        $editInfo += [pscustomobject]@{ Name = $edit.Current.Name; Id = $edit.Current.AutomationId }
        if ($Folder -and $edit.Current.AutomationId -eq '1152') {
            $pattern = $edit.GetCurrentPattern([System.Windows.Automation.ValuePattern]::Pattern)
            $pattern.SetValue($Folder)
        }
    }
    $buttonInfo = @()
    foreach ($button in $buttons) {
        $buttonInfo += [pscustomobject]@{ Name = $button.Current.Name; Id = $button.Current.AutomationId }
    }
    [pscustomobject]@{ Window = $window.Current.Name; ProcessId = $pidValue; Edits = $editInfo; Buttons = $buttonInfo } | ConvertTo-Json -Depth 4 -Compress
    if ($Folder) {
        foreach ($button in $buttons) {
            if ($button.Current.AutomationId -eq '1' -and $button.Current.IsEnabled) {
                $button.GetCurrentPattern([System.Windows.Automation.InvokePattern]::Pattern).Invoke()
                break
            }
        }
    }
}
