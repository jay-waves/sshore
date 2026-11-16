package main

// Scripts only adapt shell APIs. They contain no command/option catalog and never
// evaluate dynamic candidates; the one-time registration text is static.
var completionScripts = map[string]string{
	"bash": `# Load with: source <(sshore completion bash)
_sshore_complete() {
    local LC_ALL=C line mode prefix cur value description trim=''
    local -a rows
    mapfile -t rows < <(command sshore __complete bash "${COMP_LINE:0:COMP_POINT}" 2>/dev/null)
    COMPREPLY=()
    compopt -o filenames 2>/dev/null || :
    ((${#rows[@]})) || return 0
    IFS=$'\t' read -r mode prefix cur <<< "${rows[0]}"
    prefix=${prefix:1}; cur=${cur:1}
    for line in "${rows[@]:1}"; do
        IFS=$'\t' read -r value description <<< "$line"
        COMPREPLY+=("$value")
    done
    if [[ $mode == files || $mode == dirs ]]; then
        local action=-f
        [[ $mode == dirs ]] && action=-d
        while IFS= read -r value; do
            COMPREPLY+=("$prefix$value")
        done < <(compgen "$action" -- "${cur#"$prefix"}")
        compopt -o filenames 2>/dev/null || :
    fi
    if [[ $COMP_WORDBREAKS == *'='* && $cur == *=* ]]; then trim=${cur%=*}=; fi
    if [[ $COMP_WORDBREAKS == *':'* && $cur == *:* ]]; then trim=${cur%:*}:; fi
    if [[ -n $trim ]]; then
        for value in "${!COMPREPLY[@]}"; do COMPREPLY[value]=${COMPREPLY[value]#"$trim"}; done
    fi
    if ((${#COMPREPLY[@]} == 1)) && [[ ${COMPREPLY[0]} == *: || ${COMPREPLY[0]} == */ ]]; then
        compopt -o nospace 2>/dev/null || :
    fi
}
complete -F _sshore_complete sshore
`,
	"zsh": `# Load after compinit: source <(sshore completion zsh)
_sshore_complete() {
    local -a before rows header values descriptions
    local line value
    (( CURRENT > 2 )) && before=("${words[@]:1:$((CURRENT-2))}")
    rows=("${(@f)$(command sshore __complete words ":${words[CURRENT]}" "${before[@]}" 2>/dev/null)}")
    (( ${#rows} )) || return
    header=("${(@ps:\t:)rows[1]}")
    for line in "${rows[@]:1}"; do
        values+=("${line%%$'\t'*}")
        descriptions+=("${line%%$'\t'*} -- ${line#*$'\t'}")
    done
    if (( ${#values} )); then
        if (( ${#values} == 1 )) && [[ $values[1] == *: ]]; then
            compadd -S '' -d descriptions -- "${values[@]}"
        else
            compadd -d descriptions -- "${values[@]}"
        fi
    fi
    if [[ $header[1] == files || $header[1] == dirs ]]; then
        [[ $header[2] != : ]] && compset -P "${header[2]#:}"
        if [[ $header[1] == dirs ]]; then _files -/; else _files; fi
    fi
}
compdef _sshore_complete sshore
`,
	"fish": `# Load with: sshore completion fish | source
function __sshore_complete
    set -l before (commandline -opc)
    set -l current (commandline -ct)
    set -q current[1]; or set current ''
    set -l rows (command sshore __complete words ":$current" $before[2..-1] 2>/dev/null)
    test (count $rows) -gt 0; or return
    set -l header (string split \t -- $rows[1])
    for row in $rows[2..-1]
        printf '%s\n' "$row"
    end
    set -l prefix (string sub -s 2 -- $header[2])
    set -l path (string sub -s (math (string length -- "$prefix") + 1) -- "$current")
    if test "$header[1]" = files
        for item in (__fish_complete_path "$path")
            printf '%s%s\n' "$prefix" "$item"
        end
    else if test "$header[1]" = dirs
        for item in (__fish_complete_directories "$path")
            printf '%s%s\n' "$prefix" "$item"
        end
    end
end
complete -c sshore -f -a '(__sshore_complete)'
`,
	"powershell": `# Load with: sshore completion powershell | Out-String | Invoke-Expression
Register-ArgumentCompleter -Native -CommandName sshore -ScriptBlock {
    param($wordToComplete, $commandAst, $cursorPosition)
    $before = @()
    foreach ($element in ($commandAst.CommandElements | Select-Object -Skip 1)) {
        if ($element.Extent.EndOffset -ge $cursorPosition) { break }
        if ($element -is [System.Management.Automation.Language.StringConstantExpressionAst]) {
            $before += $element.Value
        } elseif ($element -is [System.Management.Automation.Language.CommandParameterAst]) {
            $before += $element.Extent.Text
        } else { return }
    }
    $current = $wordToComplete.Trim([char[]]@("'", [char]34))
    $rows = @(sshore __complete words (':' + $current) @before 2>$null)
    if ($rows.Count -eq 0) { return }
    $header = $rows[0] -split "` + "`t" + `", 3
    foreach ($row in ($rows | Select-Object -Skip 1)) {
        $fields = $row -split "` + "`t" + `", 2
        $value = $fields[0]
        $quoted = "'" + $value.Replace("'", "''") + "'"
        [System.Management.Automation.CompletionResult]::new($quoted, $value, 'ParameterValue', $fields[1])
    }
    if ($header[0] -eq 'files' -or $header[0] -eq 'dirs') {
        $prefix = $header[1].Substring(1)
        $path = $current.Substring($prefix.Length)
        [System.Management.Automation.CompletionCompleters]::CompleteFilename($path) |
            Where-Object { $header[0] -eq 'files' -or $_.ResultType -eq 'ProviderContainer' } |
            ForEach-Object {
                if ($prefix.Length -eq 0) { $_ } else {
                    [System.Management.Automation.CompletionResult]::new(($prefix + $_.CompletionText), $_.ListItemText, $_.ResultType, $_.ToolTip)
                }
            }
    }
}
`,
}
