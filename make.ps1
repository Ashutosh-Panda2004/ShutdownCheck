<#
.SYNOPSIS
    Windows equivalent of the Makefile. The Makefile stays canonical for CI and
    Unix contributors; this exists so the same targets work in PowerShell
    without installing make.

.EXAMPLE
    .\make.ps1 ci
#>
[CmdletBinding()]
param(
    [ValidateSet('help', 'build', 'test', 'race', 'cover', 'fmt', 'vet', 'lint', 'tidy', 'vuln', 'fuzz', 'docs', 'release-check', 'snapshot', 'ci', 'clean')]
    [string]$Target = 'help'
)

$ErrorActionPreference = 'Stop'
Set-Location -Path $PSScriptRoot

function Invoke-Step {
    param([string]$Name, [scriptblock]$Body)
    Write-Host "==> $Name" -ForegroundColor Cyan
    & $Body
    if ($LASTEXITCODE -ne 0) { throw "$Name failed with exit code $LASTEXITCODE" }
}

function Get-BuildMetadata {
    # Must work outside a git checkout too, e.g. a source tarball.
    $version = 'dev'
    $commit = 'none'
    try {
        $v = & git describe --tags --always --dirty 2>$null
        if ($LASTEXITCODE -eq 0 -and $v) { $version = "$v".Trim() }
        $c = & git rev-parse --short HEAD 2>$null
        if ($LASTEXITCODE -eq 0 -and $c) { $commit = "$c".Trim() }
    }
    catch {
        # No git, or not a repository; the defaults above are correct.
    }
    $global:LASTEXITCODE = 0

    [pscustomobject]@{
        Version = $version
        Commit  = $commit
        Date    = (Get-Date).ToUniversalTime().ToString('yyyy-MM-ddTHH:mm:ssZ')
    }
}

function Invoke-Build {
    $m = Get-BuildMetadata
    $ldflags = "-s -w -X main.version=$($m.Version) -X main.commit=$($m.Commit) -X main.date=$($m.Date)"
    Invoke-Step 'build' { go build -trimpath -ldflags $ldflags -o bin/shutdowncheck.exe ./cmd/shutdowncheck }
}

function Test-Formatting {
    Write-Host '==> fmt' -ForegroundColor Cyan
    $unformatted = (gofmt -l .) | Where-Object { $_ -and ($_ -notlike 'test\conformance*') }
    if ($unformatted) {
        Write-Host "not gofmt'd:" -ForegroundColor Red
        $unformatted | ForEach-Object { Write-Host "  $_" }
        throw 'formatting check failed; run: gofmt -w .'
    }
}

function Invoke-Lint {
    Test-Formatting
    Invoke-Step 'vet' { go vet ./... }
    if (Get-Command golangci-lint -ErrorAction SilentlyContinue) {
        Invoke-Step 'golangci-lint' { golangci-lint run }
    }
    else {
        Write-Host 'golangci-lint not installed, skipping (see CONTRIBUTING.md)' -ForegroundColor Yellow
    }
}

function Invoke-VulnerabilityCheck {
    if (Get-Command govulncheck -ErrorAction SilentlyContinue) {
        Invoke-Step 'govulncheck' { govulncheck ./... }
    }
    else {
        Write-Host 'govulncheck not installed: go install golang.org/x/vuln/cmd/govulncheck@v1.7.0' -ForegroundColor Yellow
    }
}

# The race detector needs cgo and a C toolchain, which a bare Windows install
# usually lacks. CI covers race on Linux and macOS, so skip rather than fail.
function Invoke-RaceTests {
    if (-not (Get-Command gcc -ErrorAction SilentlyContinue)) {
        Write-Host 'race detector needs cgo and a C compiler (gcc); skipping locally. CI runs it on Linux and macOS.' -ForegroundColor Yellow
        return
    }
    Invoke-Step 'race' { go test -race ./... }
}

function Invoke-Coverage {
    # Quoting matters: PowerShell 5.1 splits bare -flag=value.ext arguments.
    Invoke-Step 'cover' { go test '-covermode=atomic' '-coverprofile=coverage.out' ./... }
    go tool cover '-func=coverage.out' | Select-Object -Last 1
}

function Invoke-Goreleaser([string[]] $Arguments) {
    if (-not (Get-Command goreleaser -ErrorAction SilentlyContinue)) {
        Write-Host 'goreleaser not installed: https://goreleaser.com/install' -ForegroundColor Yellow
        Write-Host 'CI validates the release config on every push, so this is optional locally.' -ForegroundColor Yellow
        return
    }
    Invoke-Step "goreleaser $($Arguments -join ' ')" { goreleaser @Arguments }
}

switch ($Target) {
    'help' {
        Write-Host 'Targets: build test race cover fmt vet lint tidy vuln fuzz docs release-check snapshot ci clean'
        Write-Host 'Usage:   .\make.ps1 ci'
    }
    'build' { Invoke-Build }
    'test' { Invoke-Step 'test' { go test ./... } }
    'race' { Invoke-RaceTests }
    'cover' { Invoke-Coverage }
    'fmt' { Test-Formatting }
    'vet' { Invoke-Step 'vet' { go vet ./... } }
    'lint' { Invoke-Lint }
    'tidy' {
        Invoke-Step 'tidy' { go mod tidy }
        git diff --quiet -- go.mod go.sum
        if ($LASTEXITCODE -ne 0) { throw "go.mod/go.sum are not tidy; commit the result of 'go mod tidy'" }
    }
    'vuln' { Invoke-VulnerabilityCheck }
    'fuzz' {
        # Committed crashers under testdata/fuzz already run as part of `test`;
        # this explores for new ones.
        Invoke-Step 'fuzz timeline' { go test ./internal/timeline '-run=XXX' '-fuzz=FuzzReadNDJSON' '-fuzztime=30s' }
        Invoke-Step 'fuzz config' { go test ./internal/config '-run=XXX' '-fuzz=FuzzParse' '-fuzztime=30s' }
    }
    'ci' {
        Invoke-Lint
        Invoke-Step 'test' { go test ./... }
        Invoke-RaceTests
        Invoke-Coverage
        Write-Host 'CI checks passed' -ForegroundColor Green
    }
    'docs' {
        Invoke-Step 'docs' { go test ./internal/remediate -update }
        Write-Host 'docs/signatures regenerated; commit any changes' -ForegroundColor Green
    }
    'release-check' { Invoke-Goreleaser @('check') }
    'snapshot' { Invoke-Goreleaser @('build', '--snapshot', '--clean') }
    'clean' {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue bin, dist, coverage.out
        Write-Host 'cleaned'
    }
}
