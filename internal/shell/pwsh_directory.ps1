& {
    if ($ExecutionContext.SessionState.LanguageMode -ne 'FullLanguage') { return }
    $reportDirectory = [System.EventHandler[System.Management.Automation.LocationChangedEventArgs]] {
        if ($PWD.Provider.Name -eq 'FileSystem') {
            $esc = [char]27
            [Console]::Write("$esc]9;9;" + '"' + $PWD.ProviderPath + '"' + "$esc\")
        }
    }
    $invocation = $ExecutionContext.SessionState.InvokeCommand
    $invocation.LocationChangedAction = [Delegate]::Combine($reportDirectory, $invocation.LocationChangedAction)
    $reportDirectory.Invoke($null, $null)
}
