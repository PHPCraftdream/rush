$scriptArgs = @($args)
$SelfTest = $scriptArgs -contains "-SelfTest"
if ($SelfTest) {
    $scriptArgs = @($scriptArgs | Where-Object { $_ -ne "-SelfTest" })
}
if (-not $SelfTest) {
    if ($scriptArgs.Count -lt 2) {
        throw "Usage: run_capped.ps1 <memory> <timeout-seconds> <command> [args...]"
    }
    $MemoryLimit = [string] $scriptArgs[0]
    $TimeoutSeconds = [int] $scriptArgs[1]
    $Command = @($scriptArgs[2..($scriptArgs.Count - 1)])
}

Set-StrictMode -Version Latest
$ErrorActionPreference = "Stop"

Add-Type @'
using System;
using System.Runtime.InteropServices;

public static class RushJobObject {
    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_BASIC_LIMIT_INFORMATION {
        public long PerProcessUserTimeLimit;
        public long PerJobUserTimeLimit;
        public uint LimitFlags;
        public UIntPtr MinimumWorkingSetSize;
        public UIntPtr MaximumWorkingSetSize;
        public uint ActiveProcessLimit;
        public UIntPtr Affinity;
        public uint PriorityClass;
        public uint SchedulingClass;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct IO_COUNTERS {
        public ulong ReadOperationCount;
        public ulong WriteOperationCount;
        public ulong OtherOperationCount;
        public ulong ReadTransferCount;
        public ulong WriteTransferCount;
        public ulong OtherTransferCount;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct JOBOBJECT_EXTENDED_LIMIT_INFORMATION {
        public JOBOBJECT_BASIC_LIMIT_INFORMATION BasicLimitInformation;
        public IO_COUNTERS IoInfo;
        public UIntPtr ProcessMemoryLimit;
        public UIntPtr JobMemoryLimit;
        public UIntPtr PeakProcessMemoryUsed;
        public UIntPtr PeakJobMemoryUsed;
    }

    private const uint JobObjectExtendedLimitInformation = 9;
    private const uint JobObjectLimitJobMemory = 0x200;
    private const uint JobObjectLimitKillOnJobClose = 0x2000;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr attributes, string name);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(
        IntPtr job, uint infoClass, IntPtr info, uint length);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);

    public static IntPtr Create(ulong memoryBytes) {
        IntPtr job = CreateJobObject(IntPtr.Zero, null);
        if (job == IntPtr.Zero) throw new System.ComponentModel.Win32Exception();
        var limits = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
        limits.BasicLimitInformation.LimitFlags =
            JobObjectLimitJobMemory | JobObjectLimitKillOnJobClose;
        limits.JobMemoryLimit = new UIntPtr(memoryBytes);
        int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
        IntPtr buffer = Marshal.AllocHGlobal(size);
        try {
            Marshal.StructureToPtr(limits, buffer, false);
            if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation,
                    buffer, (uint)size)) throw new System.ComponentModel.Win32Exception();
        } finally {
            Marshal.FreeHGlobal(buffer);
        }
        return job;
    }

    public static void Assign(IntPtr job, IntPtr process) {
        if (!AssignProcessToJobObject(job, process))
            throw new System.ComponentModel.Win32Exception();
    }

    public static void Close(IntPtr job) {
        if (job != IntPtr.Zero) CloseHandle(job);
    }
}
'@

function Convert-MemoryLimit([string] $value) {
    if ($value -notmatch '^([0-9]+)([kKmMgG])$') {
        throw "Memory limit must be an integer followed by k, m, or g: $value"
    }
    $number = [UInt64] $Matches[1]
    switch ($Matches[2].ToLowerInvariant()) {
        "k" { return $number * 1KB }
        "m" { return $number * 1MB }
        "g" { return $number * 1GB }
    }
}

function Quote-WindowsArgument([string] $value) {
    if ($value -notmatch '[\s"]') { return $value }
    $escaped = $value -replace '(\\*)"', '$1$1\"'
    $escaped = $escaped -replace '(\\+)$', '$1$1'
    return '"' + $escaped + '"'
}

if ($SelfTest) {
    $self = $MyInvocation.MyCommand.Path
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $self 256m 1 powershell.exe -NoProfile -Command "Start-Sleep -Seconds 5"
    if ($LASTEXITCODE -ne 124) {
        throw "timeout self-test expected exit 124, got $LASTEXITCODE"
    }
    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $self 256m 30 powershell.exe -NoProfile -Command "exit 7"
    if ($LASTEXITCODE -ne 7) {
        throw "exit-code self-test expected exit 7, got $LASTEXITCODE"
    }
    Write-Output "run_capped.ps1 self-test passed"
    exit 0
}

if ($TimeoutSeconds -le 0) { throw "Timeout must be positive" }
if ($null -eq $Command -or $Command.Count -eq 0) { throw "A command is required" }
$memoryBytes = Convert-MemoryLimit $MemoryLimit

$psi = New-Object System.Diagnostics.ProcessStartInfo
$psi.FileName = $Command[0]
$psi.UseShellExecute = $false
$psi.CreateNoWindow = $true
$psi.RedirectStandardOutput = $true
$psi.RedirectStandardError = $true
if ($Command.Count -gt 1) {
    $psi.Arguments = (($Command[1..($Command.Count - 1)] | ForEach-Object {
        Quote-WindowsArgument $_
    }) -join ' ')
}

$process = New-Object System.Diagnostics.Process
$process.StartInfo = $psi
$job = [IntPtr]::Zero
try {
    if (-not $process.Start()) { throw "Could not start $($Command[0])" }
    $stdout = $process.StandardOutput.ReadToEndAsync()
    $stderr = $process.StandardError.ReadToEndAsync()
    $job = [RushJobObject]::Create($memoryBytes)
    [RushJobObject]::Assign($job, $process.Handle)

    $completed = $process.WaitForExit($TimeoutSeconds * 1000)
    if (-not $completed) {
        & taskkill.exe /PID $process.Id /T /F *> $null
        if (-not $process.WaitForExit(5000)) { $process.Kill() }
        [void] $process.WaitForExit(5000)
        $exitCode = 124
    } else {
        $exitCode = $process.ExitCode
    }

    [Console]::Write($stdout.Result)
    [Console]::Error.Write($stderr.Result)
    exit $exitCode
} finally {
    if ($job -ne [IntPtr]::Zero) { [RushJobObject]::Close($job) }
    $process.Dispose()
}
