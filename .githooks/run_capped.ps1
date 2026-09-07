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
using System.ComponentModel;
using System.IO;
using System.Runtime.InteropServices;
using System.Text;
using Microsoft.Win32.SafeHandles;

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

    [StructLayout(LayoutKind.Sequential)]
    private struct SECURITY_ATTRIBUTES {
        public int Length;
        public IntPtr SecurityDescriptor;
        public int InheritHandle;
    }

    [StructLayout(LayoutKind.Sequential, CharSet = CharSet.Unicode)]
    private struct STARTUPINFO {
        public int Size;
        public string Reserved;
        public string Desktop;
        public string Title;
        public int X;
        public int Y;
        public int XSize;
        public int YSize;
        public int XCountChars;
        public int YCountChars;
        public int FillAttribute;
        public int Flags;
        public short ShowWindow;
        public short Reserved2;
        public IntPtr Reserved2Data;
        public IntPtr StandardInput;
        public IntPtr StandardOutput;
        public IntPtr StandardError;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct PROCESS_INFORMATION {
        public IntPtr Process;
        public IntPtr Thread;
        public int ProcessId;
        public int ThreadId;
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct STARTUPINFOEX {
        public STARTUPINFO StartupInfo;
        public IntPtr AttributeList;
    }

    private const uint JobObjectExtendedLimitInformation = 9;
    private const uint JobObjectLimitJobMemory = 0x200;
    private const uint JobObjectLimitKillOnJobClose = 0x2000;
    private const uint CreateSuspendedFlag = 0x00000004;
    private const uint CreateNoWindow = 0x08000000;
    private const uint ExtendedStartupinfoPresent = 0x00080000;
    private const int StartfUseStdHandles = 0x00000100;
    private const uint HandleFlagInherit = 0x00000001;
    private const int StdInputHandle = -10;
    private const uint DuplicateSameAccess = 0x00000002;
    private const uint GenericRead = 0x80000000;
    private const uint FileShareRead = 0x00000001;
    private const uint FileShareWrite = 0x00000002;
    private const uint OpenExisting = 3;
    private const uint ErrorInsufficientBuffer = 122;
    private const uint ProcThreadAttributeHandleList = 0x00020002;
    private const uint WaitObject0 = 0;
    private const uint WaitTimeout = 0x00000102;
    private const uint WaitFailed = 0xffffffff;

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateJobObject(IntPtr attributes, string name);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetInformationJobObject(
        IntPtr job, uint infoClass, IntPtr info, uint length);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool AssignProcessToJobObject(IntPtr job, IntPtr process);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CreatePipe(
        out IntPtr readPipe, out IntPtr writePipe,
        ref SECURITY_ATTRIBUTES attributes, uint size);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool SetHandleInformation(
        IntPtr handle, uint mask, uint flags);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern bool CreateProcess(
        string applicationName, StringBuilder commandLine,
        IntPtr processAttributes, IntPtr threadAttributes, bool inheritHandles,
        uint creationFlags, IntPtr environment, string currentDirectory,
        ref STARTUPINFOEX startupInfo, out PROCESS_INFORMATION processInfo);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool InitializeProcThreadAttributeList(
        IntPtr attributeList, int attributeCount, uint flags, ref IntPtr size);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool UpdateProcThreadAttribute(
        IntPtr attributeList, uint flags, uint attribute, IntPtr value,
        IntPtr size, IntPtr previousValue, IntPtr returnSize);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern void DeleteProcThreadAttributeList(IntPtr attributeList);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint ResumeThread(IntPtr thread);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern uint WaitForSingleObject(IntPtr handle, uint timeout);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateProcess(IntPtr process, uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool TerminateJobObject(IntPtr job, uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool GetExitCodeProcess(IntPtr process, out uint exitCode);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr GetStdHandle(int standardHandle);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern IntPtr GetCurrentProcess();

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool DuplicateHandle(
        IntPtr sourceProcess, IntPtr sourceHandle, IntPtr targetProcess,
        out IntPtr targetHandle, uint desiredAccess, bool inheritHandle,
        uint options);

    [DllImport("kernel32.dll", CharSet = CharSet.Unicode, SetLastError = true)]
    private static extern IntPtr CreateFile(
        string name, uint desiredAccess, uint shareMode,
        ref SECURITY_ATTRIBUTES securityAttributes, uint creationDisposition,
        uint flags, IntPtr template);

    [DllImport("kernel32.dll", SetLastError = true)]
    private static extern bool CloseHandle(IntPtr handle);

    private static void CloseIfOpen(ref IntPtr handle) {
        if (handle != IntPtr.Zero) {
            CloseHandle(handle);
            handle = IntPtr.Zero;
        }
    }

    private static Win32Exception LastError() {
        return new Win32Exception(Marshal.GetLastWin32Error());
    }

    private static bool IsInvalidHandle(IntPtr handle) {
        return handle == IntPtr.Zero || handle == new IntPtr(-1);
    }

    private static IntPtr DuplicateOrOpenStandardInput() {
        IntPtr standardInput = GetStdHandle(StdInputHandle);
        IntPtr input;
        if (!IsInvalidHandle(standardInput)) {
            if (!DuplicateHandle(GetCurrentProcess(), standardInput,
                    GetCurrentProcess(), out input, 0, true, DuplicateSameAccess))
                throw LastError();
            return input;
        }

        var attributes = new SECURITY_ATTRIBUTES {
            Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
            InheritHandle = 1
        };
        input = CreateFile("NUL", GenericRead, FileShareRead | FileShareWrite,
            ref attributes, OpenExisting, 0, IntPtr.Zero);
        if (IsInvalidHandle(input)) throw LastError();
        return input;
    }

    public static IntPtr Create(ulong memoryBytes) {
        IntPtr job = CreateJobObject(IntPtr.Zero, null);
        if (job == IntPtr.Zero) throw LastError();
        try {
            var limits = new JOBOBJECT_EXTENDED_LIMIT_INFORMATION();
            limits.BasicLimitInformation.LimitFlags =
                JobObjectLimitJobMemory | JobObjectLimitKillOnJobClose;
            limits.JobMemoryLimit = new UIntPtr(memoryBytes);
            int size = Marshal.SizeOf(typeof(JOBOBJECT_EXTENDED_LIMIT_INFORMATION));
            IntPtr buffer = Marshal.AllocHGlobal(size);
            try {
                Marshal.StructureToPtr(limits, buffer, false);
                if (!SetInformationJobObject(job, JobObjectExtendedLimitInformation,
                        buffer, (uint)size)) throw LastError();
            } finally {
                Marshal.FreeHGlobal(buffer);
            }
            return job;
        } catch {
            CloseIfOpen(ref job);
            throw;
        }
    }

    public static IntPtr CreateInheritableSentinel() {
        var attributes = new SECURITY_ATTRIBUTES {
            Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
            InheritHandle = 1
        };
        IntPtr handle = CreateFile("NUL", GenericRead,
            FileShareRead | FileShareWrite, ref attributes, OpenExisting, 0,
            IntPtr.Zero);
        if (IsInvalidHandle(handle)) throw LastError();
        return handle;
    }

    public static void CreateSuspended(
        string commandLine,
        out IntPtr process,
        out IntPtr thread,
        out IntPtr stdoutRead,
        out IntPtr stderrRead) {
        process = IntPtr.Zero;
        thread = IntPtr.Zero;
        stdoutRead = IntPtr.Zero;
        stderrRead = IntPtr.Zero;
        IntPtr stdoutWrite = IntPtr.Zero;
        IntPtr stderrWrite = IntPtr.Zero;
        IntPtr stdin = IntPtr.Zero;
        IntPtr attributeList = IntPtr.Zero;
        IntPtr attributeHandles = IntPtr.Zero;
        bool attributeListInitialized = false;
        try {
            var attributes = new SECURITY_ATTRIBUTES {
                Length = Marshal.SizeOf(typeof(SECURITY_ATTRIBUTES)),
                InheritHandle = 1
            };
            if (!CreatePipe(out stdoutRead, out stdoutWrite, ref attributes, 0))
                throw LastError();
            if (!SetHandleInformation(stdoutRead, HandleFlagInherit, 0))
                throw LastError();
            if (!CreatePipe(out stderrRead, out stderrWrite, ref attributes, 0))
                throw LastError();
            if (!SetHandleInformation(stderrRead, HandleFlagInherit, 0))
                throw LastError();
            stdin = DuplicateOrOpenStandardInput();

            int handleCount = 3;
            IntPtr attributeListSize = IntPtr.Zero;
            if (InitializeProcThreadAttributeList(IntPtr.Zero, 1, 0,
                    ref attributeListSize) ||
                    Marshal.GetLastWin32Error() != ErrorInsufficientBuffer ||
                    attributeListSize == IntPtr.Zero)
                throw LastError();
            attributeList = Marshal.AllocHGlobal(attributeListSize.ToInt32());
            if (!InitializeProcThreadAttributeList(attributeList, 1, 0,
                    ref attributeListSize))
                throw LastError();
            attributeListInitialized = true;
            attributeHandles = Marshal.AllocHGlobal(IntPtr.Size * handleCount);
            Marshal.WriteIntPtr(attributeHandles, 0, stdin);
            Marshal.WriteIntPtr(attributeHandles, IntPtr.Size, stdoutWrite);
            Marshal.WriteIntPtr(attributeHandles, IntPtr.Size * 2, stderrWrite);
            if (!UpdateProcThreadAttribute(attributeList, 0,
                    ProcThreadAttributeHandleList, attributeHandles,
                    (IntPtr)(IntPtr.Size * handleCount), IntPtr.Zero, IntPtr.Zero))
                throw LastError();

            var startupInfo = new STARTUPINFOEX {
                StartupInfo = new STARTUPINFO {
                    Size = Marshal.SizeOf(typeof(STARTUPINFOEX)),
                    Flags = StartfUseStdHandles,
                    StandardInput = stdin,
                    StandardOutput = stdoutWrite,
                    StandardError = stderrWrite
                },
                AttributeList = attributeList
            };
            var command = new StringBuilder(commandLine);
            PROCESS_INFORMATION processInfo;
            if (!CreateProcess(null, command, IntPtr.Zero, IntPtr.Zero, true,
                    CreateSuspendedFlag | CreateNoWindow | ExtendedStartupinfoPresent,
                    IntPtr.Zero, null,
                    ref startupInfo, out processInfo)) {
                throw LastError();
            }
            process = processInfo.Process;
            thread = processInfo.Thread;
            CloseIfOpen(ref stdoutWrite);
            CloseIfOpen(ref stderrWrite);
            CloseIfOpen(ref stdin);
        } catch {
            CloseIfOpen(ref process);
            CloseIfOpen(ref thread);
            CloseIfOpen(ref stdoutRead);
            CloseIfOpen(ref stderrRead);
            throw;
        } finally {
            if (attributeListInitialized)
                DeleteProcThreadAttributeList(attributeList);
            if (attributeHandles != IntPtr.Zero)
                Marshal.FreeHGlobal(attributeHandles);
            if (attributeList != IntPtr.Zero)
                Marshal.FreeHGlobal(attributeList);
            CloseIfOpen(ref stdoutWrite);
            CloseIfOpen(ref stderrWrite);
            CloseIfOpen(ref stdin);
        }
    }

    public static StreamReader CreateReader(IntPtr handle, Encoding encoding) {
        var safeHandle = new SafeFileHandle(handle, true);
        try {
            return new StreamReader(new FileStream(safeHandle, FileAccess.Read, 4096, false),
                encoding, true, 4096, false);
        } catch {
            safeHandle.Dispose();
            throw;
        }
    }

    public static void Assign(IntPtr job, IntPtr process) {
        if (!AssignProcessToJobObject(job, process)) throw LastError();
    }

    public static void Resume(IntPtr thread) {
        if (ResumeThread(thread) == 0xffffffff) throw LastError();
    }

    public static bool Wait(IntPtr process, uint timeoutMilliseconds) {
        uint result = WaitForSingleObject(process, timeoutMilliseconds);
        if (result == WaitFailed) throw LastError();
        if (result == WaitTimeout) return false;
        if (result != WaitObject0) throw new InvalidOperationException("Unexpected process wait result");
        return true;
    }

    public static int ExitCode(IntPtr process) {
        uint exitCode;
        if (!GetExitCodeProcess(process, out exitCode)) throw LastError();
        return unchecked((int)exitCode);
    }

    public static void Terminate(IntPtr process, uint exitCode) {
        if (!TerminateProcess(process, exitCode)) throw LastError();
    }

    public static void TerminateJob(IntPtr job, uint exitCode) {
        if (!TerminateJobObject(job, exitCode)) throw LastError();
    }

    public static void Close(IntPtr handle) {
        if (handle != IntPtr.Zero) CloseHandle(handle);
    }

    public static string QuoteArgument(string value) {
        if (!string.IsNullOrEmpty(value)) {
            bool needsQuotes = false;
            foreach (char character in value) {
                if (char.IsWhiteSpace(character) || character == '"') {
                    needsQuotes = true;
                    break;
                }
            }
            if (!needsQuotes) return value;
        }
        var quoted = new StringBuilder();
        quoted.Append('"');
        int backslashes = 0;
        foreach (char character in value ?? string.Empty) {
            if (character == '\\') {
                backslashes++;
            } else if (character == '"') {
                quoted.Append('\\', backslashes * 2 + 1);
                quoted.Append('"');
                backslashes = 0;
            } else {
                quoted.Append('\\', backslashes);
                quoted.Append(character);
                backslashes = 0;
            }
        }
        quoted.Append('\\', backslashes * 2);
        quoted.Append('"');
        return quoted.ToString();
    }

    public static string BuildCommandLine(string[] arguments) {
        var commandLine = new StringBuilder();
        for (int i = 0; i < arguments.Length; i++) {
            if (i != 0) commandLine.Append(' ');
            commandLine.Append(QuoteArgument(arguments[i]));
        }
        return commandLine.ToString();
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

function Invoke-AtomicLaunchSelfTest {
    $tempPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-run-capped-" + [Guid]::NewGuid().ToString("N"))
    [IO.Directory]::CreateDirectory($tempPath) | Out-Null
    $processHandle = [IntPtr]::Zero
    $threadHandle = [IntPtr]::Zero
    $stdoutReadHandle = [IntPtr]::Zero
    $stderrReadHandle = [IntPtr]::Zero
    $job = [IntPtr]::Zero
    $sentinel = [IntPtr]::Zero
    $assigned = $false
    try {
        $markerPath = [IO.Path]::Combine($tempPath, "marker.txt")
        $probePath = [IO.Path]::Combine($tempPath, "probe.ps1")
        $probeContent = @(
            'param([Int64]$SentinelHandle, [string]$MarkerPath)'
            'Add-Type @"'
            'using System;'
            'using System.Runtime.InteropServices;'
            'public static class RushHandleProbe {'
            '    [DllImport("kernel32.dll", SetLastError = true)]'
            '    private static extern uint GetFileType(IntPtr handle);'
            '    public static bool IsOpen(IntPtr handle) { return GetFileType(handle) != 0; }'
            '}'
            '"@'
            '$handle = [IntPtr]::new($SentinelHandle)'
            '$result = if ([RushHandleProbe]::IsOpen($handle)) { "inherited" } else { "not-inherited" }'
            '[IO.File]::WriteAllText($MarkerPath, $result)'
        ) -join [Environment]::NewLine
        Set-Content -LiteralPath $probePath -Value $probeContent -Encoding UTF8
        $sentinel = [RushJobObject]::CreateInheritableSentinel()
        $commandParts = @(
            "powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
            "-File", $probePath, $sentinel.ToInt64(), $markerPath
        )
        $commandLine = [RushJobObject]::BuildCommandLine([string[]]$commandParts)
        $memoryBytes = Convert-MemoryLimit "256m"
        [RushJobObject]::CreateSuspended($commandLine, [ref]$processHandle, [ref]$threadHandle, [ref]$stdoutReadHandle, [ref]$stderrReadHandle)
        if (Test-Path -LiteralPath $markerPath) {
            throw "atomic launch self-test child ran before containment"
        }
        $job = [RushJobObject]::Create($memoryBytes)
        [RushJobObject]::Assign($job, $processHandle)
        $assigned = $true
        if (Test-Path -LiteralPath $markerPath) {
            throw "atomic launch self-test child ran before resume"
        }
        [RushJobObject]::Resume($threadHandle)
        if (-not [RushJobObject]::Wait($processHandle, 5000)) {
            throw "atomic launch self-test child did not exit"
        }
        if (-not (Test-Path -LiteralPath $markerPath)) {
            throw "atomic launch self-test marker was not written"
        }
        if ((Get-Content -Raw -LiteralPath $markerPath) -ne "not-inherited") {
            throw "atomic launch self-test inherited an unrelated handle"
        }
    } finally {
        if ($processHandle -ne [IntPtr]::Zero) {
            try {
                if ($assigned) {
                    [RushJobObject]::TerminateJob($job, 1)
                } else {
                    [RushJobObject]::Terminate($processHandle, 1)
                }
            } catch { }
            try { [RushJobObject]::Wait($processHandle, 5000) | Out-Null } catch { }
        }
        if ($job -ne [IntPtr]::Zero) { [RushJobObject]::Close($job) }
        if ($processHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($processHandle) }
        if ($threadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($threadHandle) }
        if ($stdoutReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stdoutReadHandle) }
        if ($stderrReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stderrReadHandle) }
        if ($sentinel -ne [IntPtr]::Zero) { [RushJobObject]::Close($sentinel) }
        Remove-Item -LiteralPath $tempPath -Recurse -Force -ErrorAction SilentlyContinue
    }
}

function Invoke-CapturedSelfTestProcess([string[]] $arguments, [Text.Encoding] $encoding) {
    $psi = New-Object System.Diagnostics.ProcessStartInfo
    $psi.FileName = "powershell.exe"
    $psi.Arguments = [RushJobObject]::BuildCommandLine($arguments)
    $psi.UseShellExecute = $false
    $psi.CreateNoWindow = $true
    $psi.RedirectStandardOutput = $true
    $psi.RedirectStandardError = $true
    if ($null -eq $encoding) { $encoding = [Console]::OutputEncoding }
    $psi.StandardOutputEncoding = $encoding
    $psi.StandardErrorEncoding = $encoding
    $process = New-Object System.Diagnostics.Process
    $process.StartInfo = $psi
    try {
        if (-not $process.Start()) { throw "self-test process did not start" }
        $stdout = $process.StandardOutput.ReadToEndAsync()
        $stderr = $process.StandardError.ReadToEndAsync()
        if (-not $process.WaitForExit(10000)) {
            $process.Kill()
            [void]$process.WaitForExit(5000)
            throw "self-test process timed out"
        }
        [pscustomobject]@{
            ExitCode = $process.ExitCode
            Stdout = $stdout.Result
            Stderr = $stderr.Result
        }
    } finally {
        $process.Dispose()
    }
}

function Invoke-TreeTimeoutSelfTest([string] $self, [string] $tempPath) {
    $descendantScript = [IO.Path]::Combine($tempPath, "descendant.ps1")
    $rootScript = [IO.Path]::Combine($tempPath, "root.ps1")
    $markerPath = [IO.Path]::Combine($tempPath, "delayed-marker.txt")
    $pidPath = [IO.Path]::Combine($tempPath, "descendant.pid")
    Set-Content -LiteralPath $descendantScript -Encoding UTF8 -Value @'
param([string]$MarkerPath)
Start-Sleep -Seconds 5
[IO.File]::WriteAllText($MarkerPath, "escaped")
'@
    $descendantLiteral = $descendantScript.Replace("'", "''")
    $markerLiteral = $markerPath.Replace("'", "''")
    $pidLiteral = $pidPath.Replace("'", "''")
    Set-Content -LiteralPath $rootScript -Encoding UTF8 -Value @"
`$descendant = Start-Process powershell.exe -ArgumentList @('-NoProfile','-NonInteractive','-ExecutionPolicy','Bypass','-File','$descendantLiteral','$markerLiteral') -PassThru
[IO.File]::WriteAllText('$pidLiteral', [string]`$descendant.Id)
Start-Sleep -Seconds 5
"@

    & powershell.exe -NoProfile -ExecutionPolicy Bypass -File $self 256m 1 powershell.exe -NoProfile -NonInteractive -ExecutionPolicy Bypass -File $rootScript
    if ($LASTEXITCODE -ne 124) {
        throw "tree-timeout self-test expected exit 124, got $LASTEXITCODE"
    }
    $descendantPid = [int](Get-Content -Raw -LiteralPath $pidPath)
    $descendant = $null
    try {
        $descendant = [Diagnostics.Process]::GetProcessById($descendantPid)
        if (-not $descendant.WaitForExit(5000)) {
            throw "tree-timeout self-test descendant survived"
        }
    } catch [ArgumentException] { }
    finally {
        if ($descendant -ne $null) { $descendant.Dispose() }
    }
    if (Test-Path -LiteralPath $markerPath) {
        throw "tree-timeout self-test descendant wrote its delayed marker"
    }
}

if ($SelfTest) {
    Invoke-AtomicLaunchSelfTest
    $self = $MyInvocation.MyCommand.Path
    $selfTestTempPath = [IO.Path]::Combine([IO.Path]::GetTempPath(), "rush-run-capped-selftest-" + [Guid]::NewGuid().ToString("N"))
    [IO.Directory]::CreateDirectory($selfTestTempPath) | Out-Null
    $previousEncoding = [Console]::OutputEncoding
    try {
        [Console]::OutputEncoding = New-Object System.Text.UTF8Encoding($false)
        $argumentExecutable = [IO.Path]::Combine($selfTestTempPath, "arguments.exe")
        $argumentSource = @'
using System;
using System.Text;
public static class RushArgumentOracle {
    public static void Main(string[] args) {
        foreach (string arg in args)
            Console.WriteLine(Convert.ToBase64String(Encoding.UTF8.GetBytes(arg)));
    }
}
'@
        Add-Type -TypeDefinition $argumentSource -OutputAssembly $argumentExecutable -OutputType ConsoleApplication
        $argumentDriver = [IO.Path]::Combine($selfTestTempPath, "argument-driver.ps1")
        Set-Content -LiteralPath $argumentDriver -Encoding UTF8 -Value @'
param([string]$Self, [string]$Helper)
$values = @(
    "", "space value", "embedded`"quote", "trailing\",
    [string]::Concat([char]0x0416, [char]0x0430, [char]0x0440, "-",
        [char]0x043F, [char]0x0442, [char]0x0438, [char]0x0446, [char]0x0430,
        " ", [char]0xD83C, [char]0xDF0D)
)
& $Self 256m 30 $Helper $values[0] $values[1] $values[2] $values[3] $values[4]
exit $LASTEXITCODE
'@
        $argumentValues = @(
            "", "space value", "embedded`"quote", "trailing\",
            [string]::Concat([char]0x0416, [char]0x0430, [char]0x0440, "-",
                [char]0x043F, [char]0x0442, [char]0x0438, [char]0x0446, [char]0x0430,
                " ", [char]0xD83C, [char]0xDF0D)
        )
        $argumentProcessArgs = @(
            "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $argumentDriver,
            $self, $argumentExecutable
        )
        $argumentResult = Invoke-CapturedSelfTestProcess $argumentProcessArgs
        if ($argumentResult.ExitCode -ne 0) {
            throw "argument self-test expected exit 0, got $($argumentResult.ExitCode): $($argumentResult.Stderr)"
        }
        if ($argumentResult.Stderr.Length -ne 0) {
            throw "argument self-test wrote unexpected stderr"
        }
        $encodedArguments = @($argumentResult.Stdout -split "`r?`n")
        if ($encodedArguments.Count -gt 0 -and $encodedArguments[-1] -eq "") {
            $encodedArguments = $encodedArguments[0..($encodedArguments.Count - 2)]
        }
        if ($encodedArguments.Count -ne $argumentValues.Count) {
            throw "argument self-test received $($encodedArguments.Count) arguments"
        }
        for ($index = 0; $index -lt $argumentValues.Count; $index++) {
            $received = [Text.Encoding]::UTF8.GetString([Convert]::FromBase64String($encodedArguments[$index]))
            if ($received -cne $argumentValues[$index]) {
                throw "argument self-test mismatch at index $index"
            }
        }

        $encodingExecutable = [IO.Path]::Combine($selfTestTempPath, "encoding.exe")
        $encodingSource = @'
using System;
using System.IO;
using System.Text;
public static class RushEncodingOracle {
    private static void Write(Stream stream, string value) {
        var encoding = new UTF8Encoding(false);
        byte[] preamble = encoding.GetPreamble();
        byte[] data = encoding.GetBytes(value);
        stream.Write(preamble, 0, preamble.Length);
        stream.Write(data, 0, data.Length);
        stream.Flush();
    }
    public static void Main() {
        Write(Console.OpenStandardOutput(), "\u00e9");
        Write(Console.OpenStandardError(), "\u00ef");
    }
}
'@
        Add-Type -TypeDefinition $encodingSource -OutputAssembly $encodingExecutable -OutputType ConsoleApplication
        $unicodeResult = Invoke-CapturedSelfTestProcess @(
            "powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-File", $self,
            "256m", "30", $encodingExecutable
        ) $previousEncoding
        $expectedUnicodeStdout = [string][char]0x00E9
        $expectedUnicodeStderr = [string][char]0x00EF
        if ($unicodeResult.ExitCode -ne 0 -or $unicodeResult.Stdout -ne $expectedUnicodeStdout -or
                $unicodeResult.Stderr -ne $expectedUnicodeStderr) {
            throw "encoding self-test returned unexpected stdout/stderr: exit=$($unicodeResult.ExitCode), out=[$($unicodeResult.Stdout)], err=[$($unicodeResult.Stderr)]"
        }

        Invoke-TreeTimeoutSelfTest $self $selfTestTempPath
    } finally {
        [Console]::OutputEncoding = $previousEncoding
        Remove-Item -LiteralPath $selfTestTempPath -Recurse -Force -ErrorAction SilentlyContinue
    }
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

        $commandLine = [RushJobObject]::BuildCommandLine([string[]]$Command)
$processHandle = [IntPtr]::Zero
$threadHandle = [IntPtr]::Zero
$stdoutReadHandle = [IntPtr]::Zero
$stderrReadHandle = [IntPtr]::Zero
$job = [IntPtr]::Zero
$stdoutReader = $null
$stderrReader = $null
$stdoutTask = $null
$stderrTask = $null
$assigned = $false
$jobClosed = $false
$consoleEncoding = [Console]::OutputEncoding

try {
    $job = [RushJobObject]::Create($memoryBytes)
    [RushJobObject]::CreateSuspended($commandLine, [ref]$processHandle, [ref]$threadHandle, [ref]$stdoutReadHandle, [ref]$stderrReadHandle)
    try {
        $stdoutReader = [RushJobObject]::CreateReader($stdoutReadHandle, $consoleEncoding)
    } finally {
        $stdoutReadHandle = [IntPtr]::Zero
    }
    try {
        $stderrReader = [RushJobObject]::CreateReader($stderrReadHandle, $consoleEncoding)
    } finally {
        $stderrReadHandle = [IntPtr]::Zero
    }
    $stdoutTask = $stdoutReader.ReadToEndAsync()
    $stderrTask = $stderrReader.ReadToEndAsync()
    [RushJobObject]::Assign($job, $processHandle)
    $assigned = $true
    [RushJobObject]::Resume($threadHandle)

    $completed = [RushJobObject]::Wait($processHandle, [uint32]($TimeoutSeconds * 1000))
    if (-not $completed) {
        [RushJobObject]::TerminateJob($job, 124)
        [void][RushJobObject]::Wait($processHandle, 5000)
        [RushJobObject]::Close($job)
        $job = [IntPtr]::Zero
        $jobClosed = $true
        $exitCode = 124
    } else {
        [RushJobObject]::Close($job)
        $job = [IntPtr]::Zero
        $jobClosed = $true
        $exitCode = [RushJobObject]::ExitCode($processHandle)
    }

    [Console]::Write($stdoutTask.Result)
    [Console]::Error.Write($stderrTask.Result)
    exit $exitCode
} finally {
    if ($processHandle -ne [IntPtr]::Zero) {
        try {
            if ($assigned -and -not $jobClosed -and $job -ne [IntPtr]::Zero) {
                [RushJobObject]::TerminateJob($job, 1)
            } elseif (-not $assigned) {
                [RushJobObject]::Terminate($processHandle, 1)
            }
        } catch { }
        try { [RushJobObject]::Wait($processHandle, 5000) | Out-Null } catch { }
    }
    if ($job -ne [IntPtr]::Zero) { [RushJobObject]::Close($job) }
    if ($stdoutReader -ne $null) { $stdoutReader.Dispose() }
    if ($stderrReader -ne $null) { $stderrReader.Dispose() }
    if ($stdoutReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stdoutReadHandle) }
    if ($stderrReadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($stderrReadHandle) }
    if ($processHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($processHandle) }
    if ($threadHandle -ne [IntPtr]::Zero) { [RushJobObject]::Close($threadHandle) }
}
